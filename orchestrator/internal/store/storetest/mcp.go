package storetest

import (
	"errors"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

func mustMCPServer(t *testing.T, st store.Store, wsID int64, name, prefix string) *model.MCPServer {
	t.Helper()
	m := &model.MCPServer{
		WorkspaceID: wsID,
		Name:        name,
		URL:         "https://tools.example.test/mcp",
		AuthType:    model.MCPAuthAPIKey,
		APIKey:      []byte("sealed-key"),
		ToolPrefix:  prefix,
	}
	if err := st.MCPServers().Create(ctx(), m); err != nil {
		t.Fatalf("create mcp server: %v", err)
	}
	return m
}

func projected(serverPrefix, remote, hash string) *model.Tool {
	return &model.Tool{
		Name:           serverPrefix + "." + remote,
		Kind:           "mcp",
		FriendlyName:   remote,
		Description:    "remote tool " + remote,
		Risk:           "external_communication",
		RemoteName:     remote,
		DefinitionHash: hash,
	}
}

func testMCPServers(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	m := mustMCPServer(t, st, ws.ID, "CRM", "crm")

	got, err := st.MCPServers().GetByID(ctx(), ws.ID, m.ID)
	if err != nil || got.Name != "CRM" || got.ToolPrefix != "crm" || !got.HasAPIKey() {
		t.Fatalf("round trip mismatch: %v %+v", err, got)
	}

	// A nil secret on update keeps the stored one; a new one replaces it.
	got.Name, got.URL, got.APIKey = "CRM Prod", "https://crm.example.test/mcp", nil
	if err := st.MCPServers().Update(ctx(), got); err != nil {
		t.Fatalf("update: %v", err)
	}
	kept, err := st.MCPServers().GetByID(ctx(), ws.ID, m.ID)
	if err != nil || kept.Name != "CRM Prod" || string(kept.APIKey) != "sealed-key" {
		t.Fatalf("a nil secret wiped the stored one: %v %+v", err, kept)
	}
	kept.APIKey = []byte("sealed-key-2")
	if err := st.MCPServers().Update(ctx(), kept); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if again, _ := st.MCPServers().GetByID(ctx(), ws.ID, m.ID); string(again.APIKey) != "sealed-key-2" {
		t.Fatalf("the new secret did not land: %+v", again)
	}

	// The OAuth columns are gateway-owned and survive an admin update.
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	if err := st.MCPServers().SetOAuthClient(ctx(), m.ID, "client-123", []byte("sealed-cs"), []byte(`{"issuer":"x"}`)); err != nil {
		t.Fatalf("set oauth client: %v", err)
	}
	if err := st.MCPServers().SetTokens(ctx(), m.ID, []byte("sealed-at"), []byte("sealed-rt"), &expires); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	withTokens, _ := st.MCPServers().GetByID(ctx(), ws.ID, m.ID)
	if !withTokens.Connected() || withTokens.OAuthClientID != "client-123" ||
		withTokens.OAuthTokenExpires == nil || !withTokens.OAuthTokenExpires.Equal(expires) {
		t.Fatalf("oauth state did not land: %+v", withTokens)
	}

	// A second connect attempt refreshes only the discovery metadata: it must
	// keep the issued client and its secret, or the token exchange loses the
	// credentials it needs.
	if err := st.MCPServers().SetOAuthMetadata(ctx(), m.ID, []byte(`{"issuer":"y"}`)); err != nil {
		t.Fatalf("set oauth metadata: %v", err)
	}
	refreshed, _ := st.MCPServers().GetByID(ctx(), ws.ID, m.ID)
	if refreshed.OAuthClientID != "client-123" || string(refreshed.OAuthClientSecret) != "sealed-cs" ||
		string(refreshed.OAuthMetadata) != `{"issuer":"y"}` {
		t.Fatalf("metadata refresh disturbed the credentials: %+v", refreshed)
	}

	// Sync bookkeeping.
	syncedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.MCPServers().SetSyncState(ctx(), m.ID, syncedAt, "the service did not answer"); err != nil {
		t.Fatalf("set sync state: %v", err)
	}
	stated, _ := st.MCPServers().GetByID(ctx(), ws.ID, m.ID)
	if stated.LastSyncedAt == nil || stated.LastError != "the service did not answer" {
		t.Fatalf("sync state did not land: %+v", stated)
	}

	// The prefix is the namespace key: two connections cannot share one.
	dup := &model.MCPServer{WorkspaceID: ws.ID, Name: "Other", URL: "https://o.test", AuthType: model.MCPAuthNone, ToolPrefix: "crm"}
	if err := st.MCPServers().Create(ctx(), dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate prefix, got %v", err)
	}

	// Another workspace cannot see or touch it.
	other := mustWorkspace(t, st, "globex")
	if _, err := st.MCPServers().GetByID(ctx(), other.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace read: %v", err)
	}
	if err := st.MCPServers().Delete(ctx(), other.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace delete: %v", err)
	}
}

// testMCPToolProjection pins the drift semantics the projection promises.
func testMCPToolProjection(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	user := mustUser(t, st, ws.ID, "one@acme.test")
	server := mustMCPServer(t, st, ws.ID, "CRM", "crm")

	// First sync: two tools arrive active, and reachable by nobody until an
	// agent is given them. They carry no approval bit of their own: where a
	// call pauses for a person is the agent's decision (migration 57).
	result, err := st.Tools().SyncMCPTools(ctx(), ws.ID, server.ID, []*model.Tool{
		projected("crm", "create_lead", "hash-a"),
		projected("crm", "find_contact", "hash-b"),
	})
	if err != nil || result.Added != 2 || result.Missing != 0 {
		t.Fatalf("first sync: %v %+v", err, result)
	}
	tools, err := st.Tools().ListByMCPServer(ctx(), ws.ID, server.ID)
	if err != nil || len(tools) != 2 {
		t.Fatalf("projection: %v (%d)", err, len(tools))
	}
	for _, tool := range tools {
		if tool.RequiresApproval || tool.Status != model.StatusActive || tool.RemoteMissing {
			t.Fatalf("a new remote tool must arrive active and unheld: %+v", tool)
		}
	}

	// An admin grants one tool to a group. A definition-preserving re-sync
	// keeps that decision, and stamps nothing.
	lead := tools[0]
	if lead.RemoteName != "create_lead" {
		lead = tools[1]
	}
	group := mustGroup(t, st, ws.ID, "ops")
	lead.Grants = []int64{group.ID}
	if err := st.Tools().Update(ctx(), lead); err != nil {
		t.Fatalf("grant the tool: %v", err)
	}
	if _, err := st.Tools().SyncMCPTools(ctx(), ws.ID, server.ID, []*model.Tool{
		projected("crm", "create_lead", "hash-a"),
		projected("crm", "find_contact", "hash-b"),
	}); err != nil {
		t.Fatalf("steady sync: %v", err)
	}
	steady, _ := st.Tools().GetByID(ctx(), ws.ID, lead.ID)
	if len(steady.Grants) != 1 || steady.DefinitionChangedAt != nil {
		t.Fatalf("an unchanged definition must not touch the admin's decision: %+v", steady)
	}

	// The remote redefines the tool: the projection updates, the grants stand,
	// and the change is STAMPED so the console can say so. It is not held for
	// approval: that used to happen here, and it read as a guard against a
	// service redefining a tool under us while being unable to be one, since
	// nothing syncs on its own.
	result, err = st.Tools().SyncMCPTools(ctx(), ws.ID, server.ID, []*model.Tool{
		projected("crm", "create_lead", "hash-a2"),
		projected("crm", "find_contact", "hash-b"),
	})
	if err != nil || result.Changed != 1 {
		t.Fatalf("changed sync: %v %+v", err, result)
	}
	stamped, _ := st.Tools().GetByID(ctx(), ws.ID, lead.ID)
	if stamped.RequiresApproval || stamped.DefinitionHash != "hash-a2" || stamped.DefinitionChangedAt == nil {
		t.Fatalf("a changed definition must be stamped and left alone: %+v", stamped)
	}
	if len(stamped.Grants) != 1 {
		t.Fatalf("a changed definition took the grants with it: %+v", stamped)
	}

	// The remote stops offering find_contact: flagged missing, never deleted,
	// and no longer reachable by a user.
	result, err = st.Tools().SyncMCPTools(ctx(), ws.ID, server.ID, []*model.Tool{
		projected("crm", "create_lead", "hash-a2"),
	})
	if err != nil || result.Missing != 1 || result.Added != 0 {
		t.Fatalf("missing sync: %v %+v", err, result)
	}
	all, _ := st.Tools().ListByMCPServer(ctx(), ws.ID, server.ID)
	if len(all) != 2 {
		t.Fatalf("a missing tool was deleted: %d rows", len(all))
	}
	reachable, err := st.Tools().ListForUser(ctx(), ws.ID, user.ID)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	for _, tool := range reachable {
		if tool.RemoteName == "find_contact" {
			t.Fatalf("a missing tool reached a user: %+v", tool)
		}
	}

	// It comes back: the flag clears and the governance it kept still holds.
	if _, err := st.Tools().SyncMCPTools(ctx(), ws.ID, server.ID, []*model.Tool{
		projected("crm", "create_lead", "hash-a2"),
		projected("crm", "find_contact", "hash-b"),
	}); err != nil {
		t.Fatalf("return sync: %v", err)
	}
	back, _ := st.Tools().ListByMCPServer(ctx(), ws.ID, server.ID)
	for _, tool := range back {
		if tool.RemoteMissing {
			t.Fatalf("a returned tool is still flagged missing: %+v", tool)
		}
	}

	// Deleting the connection takes the whole projection with it.
	if err := st.MCPServers().Delete(ctx(), ws.ID, server.ID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	if gone, _ := st.Tools().ListByMCPServer(ctx(), ws.ID, server.ID); len(gone) != 0 {
		t.Fatalf("the projection outlived its connection: %d rows", len(gone))
	}
}
