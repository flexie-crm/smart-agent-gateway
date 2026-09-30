package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/agentguide"
	"flexie.io/sag/internal/tools/toolguide"
)

// What the Gateway is allowed to know about its agents, against a real
// database.
//
// The prompt names an agent and says what it is for; everything else about it is
// fetched with agent_guide when the Gateway is deciding where to send a task.
// These tests are here rather than beside the renderer because the answer is
// assembled from rows: an agent's tools are its own selection intersected with
// the grants of the person asking, and a projected tool is not in the same list
// as a built-in one.

// mcpTool projects one remote tool onto the workspace, exactly as a sync does,
// and returns its callable name.
func (e *env) mcpTool(serverName, prefix, remoteName, friendly, description string) string {
	e.t.Helper()
	ctx := context.Background()

	server := &model.MCPServer{
		WorkspaceID: e.ws.ID, Name: serverName, URL: "https://" + prefix + ".test/mcp",
		AuthType: model.MCPAuthNone, ToolPrefix: prefix,
	}
	if err := e.app.Store.MCPServers().Create(ctx, server, model.Nobody()); err != nil {
		e.t.Fatalf("create mcp server: %v", err)
	}
	name := prefix + "_" + remoteName
	offered := []*model.Tool{{
		Name: name, FriendlyName: friendly, Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Risk:        string(tool.RiskExternalCommunication),
		RemoteName:  remoteName, DefinitionHash: "hash-" + remoteName,
	}}
	if _, err := e.app.Store.Tools().SyncMCPTools(ctx, e.ws.ID, server.ID, offered); err != nil {
		e.t.Fatalf("project mcp tools: %v", err)
	}
	return name
}

// gatewayTools resolves what the Gateway runs as, which is where agent_guide is
// wired in.
func (e *env) gatewayTools(u *model.User) tool.Loadout {
	e.t.Helper()
	req := app.ProfileRequest{WorkspaceID: e.ws.ID, UserID: u.ID, Channel: model.ChannelChat}
	profile, err := e.app.ResolveProfile(context.Background(), req)
	if err != nil {
		e.t.Fatalf("resolve profile: %v", err)
	}
	loadout, err := e.app.ResolveTools(context.Background(), req, profile)
	if err != nil {
		e.t.Fatalf("resolve tools: %v", err)
	}
	return loadout
}

// look calls a tool in the Gateway's loadout and returns what the model would
// receive.
func look(t *testing.T, loadout tool.Loadout, name string, args map[string]any) (string, tool.Result) {
	t.Helper()
	handler, ok := loadout.Handlers[name]
	if !ok {
		t.Fatalf("%s is not in the Gateway's loadout", name)
	}
	raw, _ := json.Marshal(args)
	res, err := handler(context.Background(), tool.Call{Args: raw})
	if err != nil {
		t.Fatalf("system error from %s: %v", name, err)
	}
	return string(res.Content), res
}

// The whole reason this exists: an agent's tools reach the Gateway, including
// the ones projected from a connected service.
//
// A projected tool never lands in the offered list (it is held on demand so
// that dozens of them are not described on every turn), and reading only the
// offered list is why an agent whose entire purpose was one connection reached
// the Gateway as an agent with no tools at all.
func TestAnAgentsToolsReachTheGatewayIncludingTheProjectedOnes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("router@acme.test")

	remote := e.mcpTool("NLI", "nli", "update_entity", "Create Record", "Write a record.")

	researcher := &model.Agent{
		WorkspaceID: e.ws.ID, Key: "researcher", Name: "Researcher",
		Instructions: "Finds source material.\nAlways cite the source.",
		Status:       model.StatusActive,
		Tools:        []string{"http_request", remote},
	}
	if err := e.app.Store.Agents().Create(ctx, researcher, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	loadout := e.gatewayTools(user)
	got, res := look(t, loadout, agentguide.Name, map[string]any{"agent": "researcher"})
	if res.Failed() {
		t.Fatalf("looking the agent up failed: %s", got)
	}

	for _, want := range []string{
		// what it is for, whole
		"Always cite the source.",
		// the built-in, by friendly name and by the key tool_guide takes
		"http_request",
		// and the projected one, which is the half that was missing
		"Create Record", remote,
		// grouped under the service it came from, by the name an administrator
		// gave that connection
		`"from":"NLI"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not carry %q:\n%s", want, got)
		}
	}
}

// An agent's knowledge reaches the Gateway too: which bases it can consult,
// which are read-only, and the brain it keeps its own memory in.
func TestAnAgentsKnowledgeReachesTheGateway(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("router@acme.test")

	playbook := &model.Brain{WorkspaceID: e.ws.ID, Name: "Support Playbook", Locked: true}
	if err := e.app.Store.Brains().CreateBrain(ctx, playbook, model.Nobody()); err != nil {
		t.Fatalf("create brain: %v", err)
	}
	memory := &model.Brain{WorkspaceID: e.ws.ID, Name: "Researcher Memory"}
	if err := e.app.Store.Brains().CreateBrain(ctx, memory, model.Nobody()); err != nil {
		t.Fatalf("create memory brain: %v", err)
	}

	researcher := &model.Agent{
		WorkspaceID: e.ws.ID, Key: "researcher", Name: "Researcher",
		Instructions: "Finds source material.", Status: model.StatusActive,
		Tools:         []string{"http_request"},
		Brains:        []int64{playbook.ID},
		MemoryBrainID: &memory.ID,
	}
	if err := e.app.Store.Agents().Create(ctx, researcher, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	got, _ := look(t, e.gatewayTools(user), agentguide.Name, map[string]any{"agent": "researcher"})
	for _, want := range []string{"Support Playbook", `"read_only":true`, "Researcher Memory"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not carry %q:\n%s", want, got)
		}
	}
}

// The answer is the asking person's. An agent's tools are its own selection
// intersected with their grants, so somebody outside the granted group is told
// about the agent and not about the ability they could not have used anyway.
//
// This is the property that makes the answer uncacheable, so it is asserted
// rather than assumed.
func TestWhatAnAgentCanDoIsAnsweredForThePersonAsking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	insider := e.user("insider@acme.test")
	outsider := e.user("outsider@acme.test")
	e.group("Web", insider)

	// The first grant is what makes a tool exclusive.
	tools, err := e.app.Store.Tools().List(ctx, e.ws.ID)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var web *model.Tool
	for _, row := range tools {
		if row.Name == "http_request" {
			web = row
		}
	}
	if web == nil {
		t.Fatal("this build has no http_request tool to grant")
	}
	groups, err := e.app.Store.Groups().List(ctx, e.ws.ID)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	web.Grants = []int64{groups[0].ID}
	if err := e.app.Store.Tools().Update(ctx, web, model.Nobody()); err != nil {
		t.Fatalf("grant the tool: %v", err)
	}

	researcher := &model.Agent{
		WorkspaceID: e.ws.ID, Key: "researcher", Name: "Researcher",
		Instructions: "Finds source material.", Status: model.StatusActive,
		Tools: []string{"http_request"},
	}
	if err := e.app.Store.Agents().Create(ctx, researcher, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	granted, _ := look(t, e.gatewayTools(insider), agentguide.Name, map[string]any{"agent": "researcher"})
	if !strings.Contains(granted, "http_request") {
		t.Fatalf("a person in the granted group was not told the agent holds the tool:\n%s", granted)
	}
	withheld, _ := look(t, e.gatewayTools(outsider), agentguide.Name, map[string]any{"agent": "researcher"})
	if strings.Contains(withheld, "http_request") {
		t.Fatalf("a person outside the granted group was told the agent holds the tool:\n%s", withheld)
	}
	// The agent itself is still there to delegate to: it is the ABILITY that is
	// withheld, not the agent.
	if !strings.Contains(withheld, "researcher") {
		t.Fatalf("the agent itself disappeared for a person without the grant:\n%s", withheld)
	}
}

// Once the Gateway has an agent's tool key, tool_guide reads it, and says whose
// it is. Reading and holding must not look the same.
func TestTheGatewayCanReadTheGuideOfAnAgentsTool(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("router@acme.test")

	// An agent holding a tool the Gateway itself does not: the Gateway's
	// default tool set is what every workspace starts with, and this is not in
	// it.
	researcher := &model.Agent{
		WorkspaceID: e.ws.ID, Key: "researcher", Name: "Researcher",
		Instructions: "Queries things.", Status: model.StatusActive,
		Tools: []string{"http_request"},
	}
	if err := e.app.Store.Agents().Create(ctx, researcher, model.Nobody()); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	loadout := e.gatewayTools(user)
	if _, own := loadout.Handlers["http_request"]; own {
		t.Skip("this build gives the Gateway http_request by default, so it is not a foreign tool here")
	}

	got, res := look(t, loadout, toolguide.Name, map[string]any{"tool_name": "http_request"})
	if res.Failed() {
		t.Fatalf("the Gateway could not read its agent's tool: %s", got)
	}
	for _, want := range []string{"researcher", "cannot call", "Delegate"} {
		if !strings.Contains(got, want) {
			t.Errorf("reading an agent's tool does not say it is the agent's (%q missing):\n%s", want, got)
		}
	}
}

// A workspace with no agents carries no agent_guide. A tool whose only possible
// answer is "there are none" is a tool that costs every turn and answers
// nothing.
func TestAWorkspaceWithNoAgentsHasNoAgentGuide(t *testing.T) {
	e := newEnv(t)
	user := e.user("alone@acme.test")

	loadout := e.gatewayTools(user)
	if _, ok := loadout.Handlers[agentguide.Name]; ok {
		t.Fatal("agent_guide was loaded into a workspace with no agents")
	}
	for _, schema := range loadout.Schemas {
		if schema.Name == agentguide.Name {
			t.Fatal("agent_guide was described to a workspace with no agents")
		}
	}
}
