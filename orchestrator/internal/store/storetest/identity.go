package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

func mustWorkspace(t *testing.T, st store.Store, slug string) *model.Workspace {
	t.Helper()
	w := &model.Workspace{Slug: slug, Name: slug}
	if err := st.Workspaces().Create(ctx(), w); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return w
}

// mustUser creates a person and makes them a member of the workspace, which is
// what "a user of this workspace" now means: the row is tenant-level, the
// membership is what places them.
func mustUser(t *testing.T, st store.Store, wsID int64, email string) *model.User {
	t.Helper()
	u := &model.User{Email: email, Name: email, PasswordHash: "hash"}
	if err := st.Users().Create(ctx(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.Workspaces().SetMembers(ctx(), u.ID, []int64{wsID}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return u
}

func mustGroup(t *testing.T, st store.Store, wsID int64, name string) *model.Group {
	t.Helper()
	g := &model.Group{WorkspaceID: wsID, Name: name}
	if err := st.Groups().Create(ctx(), g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func mustRole(t *testing.T, st store.Store, wsID int64, name string, perms []string) *model.Role {
	t.Helper()
	r := &model.Role{WorkspaceID: wsID, Name: name, Permissions: perms}
	if err := st.Roles().Create(ctx(), r); err != nil {
		t.Fatalf("create role: %v", err)
	}
	return r
}

func testWorkspaces(t *testing.T, st store.Store) {
	w := mustWorkspace(t, st, "acme")
	if w.ID == 0 {
		t.Fatal("workspace id not assigned")
	}
	if w.Status != model.StatusActive {
		t.Fatalf("expected default status active, got %q", w.Status)
	}

	got, err := st.Workspaces().GetBySlug(ctx(), "acme")
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if got.ID != w.ID || got.Name != "acme" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got, err = st.Workspaces().GetByID(ctx(), w.ID); err != nil || got.Slug != "acme" {
		t.Fatalf("get by id: %v %+v", err, got)
	}

	if _, err := st.Workspaces().GetBySlug(ctx(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// The slug is the tenant key: a duplicate must be rejected, not
	// silently create a second workspace.
	if err := st.Workspaces().Create(ctx(), &model.Workspace{Slug: "acme", Name: "Other"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate slug, got %v", err)
	}
}

func testWorkspaceAdministration(t *testing.T, st store.Store) {
	acme := mustWorkspace(t, st, "acme")
	globex := mustWorkspace(t, st, "globex")

	// Update writes what it says: name, slug, description, and the status switch.
	acme.Name, acme.Slug, acme.Status = "Acme Corp", "acme-corp", model.StatusSuspended
	acme.Description = "Everything the sales team touches."
	if err := st.Workspaces().Update(ctx(), acme); err != nil {
		t.Fatalf("update workspace: %v", err)
	}
	got, err := st.Workspaces().GetByID(ctx(), acme.ID)
	if err != nil || got.Name != "Acme Corp" || got.Slug != "acme-corp" || got.Status != model.StatusSuspended {
		t.Fatalf("update did not land: %v %+v", err, got)
	}
	if got.Description != "Everything the sales team touches." {
		t.Fatalf("description did not round-trip: %+v", got)
	}

	// The slug stays the tenant key on update too.
	acme.Slug = "globex"
	if err := st.Workspaces().Update(ctx(), acme); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate slug, got %v", err)
	}

	// Updating a workspace that is not there says so.
	missing := &model.Workspace{ID: 999999, Slug: "missing", Name: "Missing", Status: model.StatusActive}
	if err := st.Workspaces().Update(ctx(), missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// AddMember grants one workspace without replacing the person's list.
	acme.Slug, acme.Status = "acme-corp", model.StatusActive
	if err := st.Workspaces().Update(ctx(), acme); err != nil {
		t.Fatalf("reactivate workspace: %v", err)
	}
	u := mustUser(t, st, globex.ID, "one@acme.test")
	if err := st.Workspaces().AddMember(ctx(), acme.ID, u.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := st.Workspaces().AddMember(ctx(), acme.ID, u.ID); err != nil {
		t.Fatalf("re-adding a member must be idempotent: %v", err)
	}
	mine, err := st.Workspaces().ListForUser(ctx(), u.ID)
	if err != nil || len(mine) != 2 {
		t.Fatalf("expected membership of both workspaces: %v %+v", err, mine)
	}

	// Delete takes the workspace and its memberships with it.
	if err := st.Workspaces().Delete(ctx(), acme.ID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if _, err := st.Workspaces().GetByID(ctx(), acme.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the workspace is still there: %v", err)
	}
	if mine, _ = st.Workspaces().ListForUser(ctx(), u.ID); len(mine) != 1 || mine[0].ID != globex.ID {
		t.Fatalf("the membership outlived the workspace: %+v", mine)
	}
	if err := st.Workspaces().Delete(ctx(), acme.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting a deleted workspace must report ErrNotFound, got %v", err)
	}
}

func testUsers(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	u := mustUser(t, st, ws.ID, "a@acme.test")

	got, err := st.Users().GetByEmail(ctx(), "a@acme.test")
	if err != nil || got.ID != u.ID {
		t.Fatalf("get by email: %v %+v", err, got)
	}
	if got, err = st.Users().GetByID(ctx(), u.ID); err != nil || got.Email != "a@acme.test" {
		t.Fatalf("get by id: %v %+v", err, got)
	}

	mustUser(t, st, ws.ID, "b@acme.test")
	list, err := st.Users().List(ctx())
	if err != nil || len(list) != 2 {
		t.Fatalf("list users: %v (%d)", err, len(list))
	}

	u.Name = "Renamed"
	u.Status = model.StatusDisabled
	if err := st.Users().Update(ctx(), u); err != nil {
		t.Fatalf("update user: %v", err)
	}
	got, err = st.Users().GetByID(ctx(), u.ID)
	if err != nil || got.Name != "Renamed" || got.Status != model.StatusDisabled {
		t.Fatalf("update not persisted: %v %+v", err, got)
	}

	// Re-applying identical values must succeed: MySQL reports zero
	// affected rows for an unchanged UPDATE, which must not be read as
	// "row missing".
	if err := st.Users().Update(ctx(), got); err != nil {
		t.Fatalf("no-op update must succeed: %v", err)
	}

	if err := st.Users().UpdatePassword(ctx(), u.ID, "new-hash"); err != nil {
		t.Fatalf("update password: %v", err)
	}
	if err := st.Users().UpdatePassword(ctx(), u.ID, "new-hash"); err != nil {
		t.Fatalf("no-op password update must succeed: %v", err)
	}
	if err := st.Users().UpdatePassword(ctx(), 999999, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("password update for unknown user must report ErrNotFound, got %v", err)
	}
	if got, _ = st.Users().GetByID(ctx(), u.ID); got.PasswordHash != "new-hash" {
		t.Fatalf("password not persisted: %q", got.PasswordHash)
	}

	if err := st.Users().Delete(ctx(), u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := st.Users().GetByID(ctx(), u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user still present after delete: %v", err)
	}
	// Deleting a missing row must report ErrNotFound, never succeed silently.
	if err := st.Users().Delete(ctx(), u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on repeat delete, got %v", err)
	}
}

// The email is the person, and there is one of them per tenant. Two rows with
// the same address would be two accounts for one colleague, and the login would
// have to guess which.
func testUserUniqueness(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	mustUser(t, st, ws.ID, "same@example.test")

	err := st.Users().Create(ctx(), &model.User{
		Email: "same@example.test", Name: "Dup", PasswordHash: "h",
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for a duplicate email, got %v", err)
	}
}

// Membership is what a workspace switch is allowed to consult, so it is tested
// for what it refuses as much as for what it returns.
func testWorkspaceMembership(t *testing.T, st store.Store) {
	acme := mustWorkspace(t, st, "acme")
	globex := mustWorkspace(t, st, "globex")
	closed := &model.Workspace{Slug: "closed", Name: "closed", Status: "suspended"}
	if err := st.Workspaces().Create(ctx(), closed); err != nil {
		t.Fatalf("create suspended workspace: %v", err)
	}
	user := mustUser(t, st, acme.ID, "member@acme.test")

	if ok, err := st.Workspaces().IsMember(ctx(), acme.ID, user.ID); err != nil || !ok {
		t.Fatalf("member of their own workspace: %v %v", err, ok)
	}
	if ok, err := st.Workspaces().IsMember(ctx(), globex.ID, user.ID); err != nil || ok {
		t.Fatalf("a stranger must not be a member: %v %v", err, ok)
	}

	// SetMembers replaces, so a person removed from a workspace is out of it.
	if err := st.Workspaces().SetMembers(ctx(), user.ID, []int64{globex.ID, closed.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	if ok, err := st.Workspaces().IsMember(ctx(), acme.ID, user.ID); err != nil || ok {
		t.Fatalf("membership must be replaced, not added to: %v %v", err, ok)
	}

	// A suspended workspace is nowhere to go, membership or not.
	if ok, err := st.Workspaces().IsMember(ctx(), closed.ID, user.ID); err != nil || ok {
		t.Fatalf("a suspended workspace grants nothing: %v %v", err, ok)
	}
	list, err := st.Workspaces().ListForUser(ctx(), user.ID)
	if err != nil || len(list) != 1 || list[0].ID != globex.ID {
		t.Fatalf("only active memberships are offered: %v %+v", err, list)
	}

	// A membership of a workspace that does not exist is refused whole: the
	// list is written in one transaction or not at all.
	if err := st.Workspaces().SetMembers(ctx(), user.ID, []int64{acme.ID, 999999}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown workspace, got %v", err)
	}
	if ok, err := st.Workspaces().IsMember(ctx(), acme.ID, user.ID); err != nil || ok {
		t.Fatalf("a refused SetMembers must leave nothing behind: %v %v", err, ok)
	}

	// AllMemberships is the raw relationship, suspended workspaces included: it
	// answers "where is this person a member", which the console draws, not
	// "where may they go", which the switcher asks.
	all, err := st.Workspaces().AllMemberships(ctx())
	if err != nil || !slices.Equal(all[user.ID], []int64{globex.ID, closed.ID}) {
		t.Fatalf("memberships by user: %v %+v", err, all)
	}
}

// A group belongs to a workspace. Putting an outsider in it would hand them the
// group's roles, so the store refuses instead of quietly widening access.
func testGroupMemberMustBelongToWorkspace(t *testing.T, st store.Store) {
	acme := mustWorkspace(t, st, "acme")
	globex := mustWorkspace(t, st, "globex")
	group := mustGroup(t, st, acme.ID, "Support")
	outsider := mustUser(t, st, globex.ID, "outsider@globex.test")

	if err := st.Groups().AddMember(ctx(), group.ID, outsider.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound adding an outsider, got %v", err)
	}
	members, err := st.Groups().ListMembers(ctx(), group.ID)
	if err != nil || len(members) != 0 {
		t.Fatalf("the outsider must not be in the group: %v %+v", err, members)
	}

	// Once they belong to the workspace, they may belong to its groups.
	if err := st.Workspaces().SetMembers(ctx(), outsider.ID, []int64{globex.ID, acme.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	if err := st.Groups().AddMember(ctx(), group.ID, outsider.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// And adding them twice is not an error.
	if err := st.Groups().AddMember(ctx(), group.ID, outsider.ID); err != nil {
		t.Fatalf("re-adding a member must succeed: %v", err)
	}
	if members, err = st.Groups().ListMembers(ctx(), group.ID); err != nil || len(members) != 1 {
		t.Fatalf("expected one member: %v %+v", err, members)
	}
}

// Settings are one person's, and the store must not let a key wander between
// people or survive being overwritten.
func testUserSettings(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	alice := mustUser(t, st, ws.ID, "alice@acme.test")
	bob := mustUser(t, st, ws.ID, "bob@acme.test")

	if _, err := st.Settings().Get(ctx(), alice.ID, model.SettingWorkspace); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unset key must report ErrNotFound, got %v", err)
	}

	if err := st.Settings().Set(ctx(), alice.ID, model.SettingWorkspace, "7"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	// The same key for another person is a different setting.
	if err := st.Settings().Set(ctx(), bob.ID, model.SettingWorkspace, "9"); err != nil {
		t.Fatalf("set setting for another user: %v", err)
	}
	if v, err := st.Settings().Get(ctx(), alice.ID, model.SettingWorkspace); err != nil || v != "7" {
		t.Fatalf("one person's setting must not be another's: %v %q", err, v)
	}

	// Setting again overwrites: a preference has no history.
	if err := st.Settings().Set(ctx(), alice.ID, model.SettingWorkspace, "8"); err != nil {
		t.Fatalf("overwrite setting: %v", err)
	}
	if v, err := st.Settings().Get(ctx(), alice.ID, model.SettingWorkspace); err != nil || v != "8" {
		t.Fatalf("overwrite not persisted: %v %q", err, v)
	}

	// The value is text, and text is whatever the caller put in it.
	if err := st.Settings().Set(ctx(), alice.ID, "layout", `{"sidebar":"collapsed"}`); err != nil {
		t.Fatalf("set json setting: %v", err)
	}
	all, err := st.Settings().All(ctx(), alice.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("expected two settings: %v %+v", err, all)
	}
	if all[0].Key != "layout" || all[0].Value != `{"sidebar":"collapsed"}` {
		t.Fatalf("json value came back changed: %+v", all[0])
	}

	if err := st.Settings().Delete(ctx(), alice.ID, "layout"); err != nil {
		t.Fatalf("delete setting: %v", err)
	}
	// Deleting what is not there is what the caller asked for, not an error.
	if err := st.Settings().Delete(ctx(), alice.ID, "layout"); err != nil {
		t.Fatalf("repeat delete must succeed: %v", err)
	}
	if all, err = st.Settings().All(ctx(), alice.ID); err != nil || len(all) != 1 {
		t.Fatalf("expected one setting left: %v %+v", err, all)
	}

	// A deleted person's preferences go with them.
	if err := st.Users().Delete(ctx(), alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if all, err = st.Settings().All(ctx(), alice.ID); err != nil || len(all) != 0 {
		t.Fatalf("settings outlived their owner: %v %+v", err, all)
	}
}

func testGroups(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	g := mustGroup(t, st, ws.ID, "Sales")

	got, err := st.Groups().GetByID(ctx(), ws.ID, g.ID)
	if err != nil || got.Name != "Sales" {
		t.Fatalf("get group: %v %+v", err, got)
	}

	g.Name = "Sales EMEA"
	if err := st.Groups().Update(ctx(), g); err != nil {
		t.Fatalf("update group: %v", err)
	}
	if got, _ = st.Groups().GetByID(ctx(), ws.ID, g.ID); got.Name != "Sales EMEA" {
		t.Fatalf("group rename not persisted: %+v", got)
	}
	// Unchanged UPDATE must not be mistaken for a missing row.
	if err := st.Groups().Update(ctx(), g); err != nil {
		t.Fatalf("no-op group update must succeed: %v", err)
	}
	if err := st.Groups().Update(ctx(), &model.Group{ID: 999999, WorkspaceID: ws.ID, Name: "X"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of unknown group must report ErrNotFound, got %v", err)
	}

	mustGroup(t, st, ws.ID, "Support")
	list, err := st.Groups().List(ctx(), ws.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list groups: %v (%d)", err, len(list))
	}

	if err := st.Groups().Create(ctx(), &model.Group{WorkspaceID: ws.ID, Name: "Support"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate group name, got %v", err)
	}

	other := mustWorkspace(t, st, "globex")
	if _, err := st.Groups().GetByID(ctx(), other.ID, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group leaked across workspaces: %v", err)
	}
}

func testGroupMembership(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	g := mustGroup(t, st, ws.ID, "Sales")
	u1 := mustUser(t, st, ws.ID, "u1@acme.test")
	u2 := mustUser(t, st, ws.ID, "u2@acme.test")

	if err := st.Groups().AddMember(ctx(), g.ID, u1.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Adding twice must be idempotent, not an error and not a duplicate.
	if err := st.Groups().AddMember(ctx(), g.ID, u1.ID); err != nil {
		t.Fatalf("re-add member must be idempotent: %v", err)
	}
	if err := st.Groups().AddMember(ctx(), g.ID, u2.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	members, err := st.Groups().ListMembers(ctx(), g.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("list members: %v (%d)", err, len(members))
	}

	groups, err := st.Groups().ListForUser(ctx(), u1.ID)
	if err != nil || len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("list groups for user: %v %+v", err, groups)
	}

	if err := st.Groups().RemoveMember(ctx(), g.ID, u1.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := st.Groups().RemoveMember(ctx(), g.ID, u1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removing a non-member must report ErrNotFound, got %v", err)
	}
	if members, _ = st.Groups().ListMembers(ctx(), g.ID); len(members) != 1 {
		t.Fatalf("expected 1 member left, got %d", len(members))
	}
}

// testGroupSetForUser pins the wholesale form of membership: the person's
// groups WITHIN ONE WORKSPACE become exactly the list, and nothing outside
// that workspace moves.
func testGroupSetForUser(t *testing.T, st store.Store) {
	acme := mustWorkspace(t, st, "acme")
	globex := mustWorkspace(t, st, "globex")
	sales := mustGroup(t, st, acme.ID, "Sales")
	support := mustGroup(t, st, acme.ID, "Support")
	remote := mustGroup(t, st, globex.ID, "Remote")

	u := mustUser(t, st, acme.ID, "one@acme.test")
	if err := st.Workspaces().SetMembers(ctx(), u.ID, []int64{acme.ID, globex.ID}); err != nil {
		t.Fatalf("grant both workspaces: %v", err)
	}
	if err := st.Groups().AddMember(ctx(), remote.ID, u.ID); err != nil {
		t.Fatalf("join the other workspace's group: %v", err)
	}

	groupIDs := func() []int64 {
		t.Helper()
		groups, err := st.Groups().ListForUser(ctx(), u.ID)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		ids := make([]int64, 0, len(groups))
		for _, g := range groups {
			ids = append(ids, g.ID)
		}
		slices.Sort(ids)
		return ids
	}

	// The memberships become exactly the list; the other workspace's group
	// stays.
	if err := st.Groups().SetForUser(ctx(), acme.ID, u.ID, []int64{sales.ID, support.ID}); err != nil {
		t.Fatalf("set groups: %v", err)
	}
	want := []int64{sales.ID, support.ID, remote.ID}
	slices.Sort(want)
	if got := groupIDs(); !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}

	// A shorter list removes what it no longer names.
	if err := st.Groups().SetForUser(ctx(), acme.ID, u.ID, []int64{support.ID}); err != nil {
		t.Fatalf("shrink groups: %v", err)
	}
	want = []int64{support.ID, remote.ID}
	slices.Sort(want)
	if got := groupIDs(); !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}

	// Naming another workspace's group refuses the whole save, and the
	// memberships stay what they were: no half-applied list.
	if err := st.Groups().SetForUser(ctx(), acme.ID, u.ID, []int64{sales.ID, remote.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a foreign group, got %v", err)
	}
	if got := groupIDs(); !slices.Equal(got, want) {
		t.Fatalf("a refused save must change nothing: %v", got)
	}

	// The empty list empties this workspace's memberships only.
	if err := st.Groups().SetForUser(ctx(), acme.ID, u.ID, []int64{}); err != nil {
		t.Fatalf("clear groups: %v", err)
	}
	if got := groupIDs(); !slices.Equal(got, []int64{remote.ID}) {
		t.Fatalf("expected only the other workspace's group, got %v", got)
	}

	// A person who cannot act in the workspace cannot be handed its groups.
	outsider := mustUser(t, st, globex.ID, "two@globex.test")
	if err := st.Groups().SetForUser(ctx(), acme.ID, outsider.ID, []int64{sales.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an outsider, got %v", err)
	}
}

func testRoles(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	r := mustRole(t, st, ws.ID, "Ops", []string{model.PermUsersView, model.PermGroupsView})

	got, err := st.Roles().GetByID(ctx(), ws.ID, r.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if !slices.Contains(got.Permissions, model.PermUsersView) ||
		!slices.Contains(got.Permissions, model.PermGroupsView) || len(got.Permissions) != 2 {
		t.Fatalf("permissions not round-tripped: %+v", got.Permissions)
	}

	list, err := st.Roles().List(ctx(), ws.ID)
	if err != nil || len(list) != 1 || len(list[0].Permissions) != 2 {
		t.Fatalf("list roles must include permissions: %v %+v", err, list)
	}

	if err := st.Roles().Create(ctx(), &model.Role{WorkspaceID: ws.ID, Name: "Ops"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate role name, got %v", err)
	}

	// Group role assignment round trip.
	g := mustGroup(t, st, ws.ID, "Sales")
	if err := st.Groups().AssignRole(ctx(), g.ID, r.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	if err := st.Groups().AssignRole(ctx(), g.ID, r.ID); err != nil {
		t.Fatalf("re-assign must be idempotent: %v", err)
	}
	roles, err := st.Groups().ListRoles(ctx(), g.ID)
	if err != nil || len(roles) != 1 || len(roles[0].Permissions) != 2 {
		t.Fatalf("list group roles: %v %+v", err, roles)
	}
	if err := st.Groups().UnassignRole(ctx(), g.ID, r.ID); err != nil {
		t.Fatalf("unassign role: %v", err)
	}
	if roles, _ = st.Groups().ListRoles(ctx(), g.ID); len(roles) != 0 {
		t.Fatalf("role still assigned after unassign: %+v", roles)
	}
}

func testRolePermissionReplacement(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	r := mustRole(t, st, ws.ID, "Ops", []string{model.PermUsersView, model.PermUsersEdit})

	// Update replaces the whole set: removed permissions must disappear.
	r.Name = "Ops 2"
	r.Permissions = []string{model.PermGroupsView}
	if err := st.Roles().Update(ctx(), r); err != nil {
		t.Fatalf("update role: %v", err)
	}
	got, err := st.Roles().GetByID(ctx(), ws.ID, r.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if got.Name != "Ops 2" {
		t.Fatalf("role rename not persisted: %q", got.Name)
	}
	if len(got.Permissions) != 1 || got.Permissions[0] != model.PermGroupsView {
		t.Fatalf("permissions not replaced: %+v", got.Permissions)
	}

	// Clearing permissions leaves an empty set, not the old one.
	r.Permissions = nil
	if err := st.Roles().Update(ctx(), r); err != nil {
		t.Fatalf("clear permissions: %v", err)
	}
	if got, _ = st.Roles().GetByID(ctx(), ws.ID, r.ID); len(got.Permissions) != 0 {
		t.Fatalf("expected no permissions, got %+v", got.Permissions)
	}
}

func testEffectivePermissions(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	u := mustUser(t, st, ws.ID, "u@acme.test")

	// No groups: no permissions, and nothing is granted.
	perms, err := st.Users().EffectivePermissions(ctx(), u.ID)
	if err != nil || len(perms) != 0 {
		t.Fatalf("expected no permissions: %v %+v", err, perms)
	}
	if model.HasPermission(perms, model.PermUsersView) {
		t.Fatal("permission granted with no roles")
	}

	// user -> group -> role -> permissions, resolved through the join.
	viewers := mustGroup(t, st, ws.ID, "Viewers")
	editors := mustGroup(t, st, ws.ID, "Editors")
	viewRole := mustRole(t, st, ws.ID, "Viewer", []string{model.PermUsersView})
	editRole := mustRole(t, st, ws.ID, "Editor", []string{model.PermUsersView, model.PermUsersEdit})

	for _, pair := range [][2]int64{{viewers.ID, viewRole.ID}, {editors.ID, editRole.ID}} {
		if err := st.Groups().AssignRole(ctx(), pair[0], pair[1]); err != nil {
			t.Fatalf("assign role: %v", err)
		}
	}
	if err := st.Groups().AddMember(ctx(), viewers.ID, u.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	perms, err = st.Users().EffectivePermissions(ctx(), u.ID)
	if err != nil {
		t.Fatalf("effective permissions: %v", err)
	}
	if !model.HasPermission(perms, model.PermUsersView) {
		t.Fatalf("expected users:view via group role, got %+v", perms)
	}
	if model.HasPermission(perms, model.PermUsersEdit) {
		t.Fatalf("permission leaked from a group the user is not in: %+v", perms)
	}

	// Second group: the union is returned, deduplicated.
	if err := st.Groups().AddMember(ctx(), editors.ID, u.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	perms, err = st.Users().EffectivePermissions(ctx(), u.ID)
	if err != nil {
		t.Fatalf("effective permissions: %v", err)
	}
	if !model.HasPermission(perms, model.PermUsersEdit) || !model.HasPermission(perms, model.PermUsersView) {
		t.Fatalf("expected union of both roles, got %+v", perms)
	}
	if len(perms) != 2 {
		t.Fatalf("expected deduplicated permissions, got %+v", perms)
	}

	// Superuser satisfies any check, including permissions it never lists.
	admins := mustGroup(t, st, ws.ID, model.BootstrapAdminGroup)
	adminRole := mustRole(t, st, ws.ID, model.BootstrapAdminRole, []string{model.PermSuperuser})
	if err := st.Groups().AssignRole(ctx(), admins.ID, adminRole.ID); err != nil {
		t.Fatalf("assign admin role: %v", err)
	}
	admin := mustUser(t, st, ws.ID, "admin@acme.test")
	if err := st.Groups().AddMember(ctx(), admins.ID, admin.ID); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	perms, err = st.Users().EffectivePermissions(ctx(), admin.ID)
	if err != nil {
		t.Fatalf("effective permissions: %v", err)
	}
	if !model.HasPermission(perms, model.PermRolesDelete) || !model.HasPermission(perms, "anything:at:all") {
		t.Fatalf("superuser must satisfy every check, got %+v", perms)
	}
}

// testCascadingDeletes proves a deleted group or role cannot keep granting
// permissions through orphaned join rows.
func testCascadingDeletes(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	u := mustUser(t, st, ws.ID, "u@acme.test")
	g := mustGroup(t, st, ws.ID, "Sales")
	r := mustRole(t, st, ws.ID, "Editor", []string{model.PermUsersEdit})

	if err := st.Groups().AssignRole(ctx(), g.ID, r.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	if err := st.Groups().AddMember(ctx(), g.ID, u.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	perms, _ := st.Users().EffectivePermissions(ctx(), u.ID)
	if !model.HasPermission(perms, model.PermUsersEdit) {
		t.Fatalf("setup failed, expected users:edit: %+v", perms)
	}

	// Deleting the role removes the grant.
	if err := st.Roles().Delete(ctx(), ws.ID, r.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	perms, err := st.Users().EffectivePermissions(ctx(), u.ID)
	if err != nil {
		t.Fatalf("effective permissions: %v", err)
	}
	if model.HasPermission(perms, model.PermUsersEdit) {
		t.Fatalf("deleted role still grants permissions: %+v", perms)
	}
	if roles, _ := st.Groups().ListRoles(ctx(), g.ID); len(roles) != 0 {
		t.Fatalf("orphaned group_roles row survived role delete: %+v", roles)
	}

	// Deleting a group removes its memberships.
	r2 := mustRole(t, st, ws.ID, "Editor2", []string{model.PermUsersEdit})
	if err := st.Groups().AssignRole(ctx(), g.ID, r2.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	if err := st.Groups().Delete(ctx(), ws.ID, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	perms, _ = st.Users().EffectivePermissions(ctx(), u.ID)
	if len(perms) != 0 {
		t.Fatalf("deleted group still grants permissions: %+v", perms)
	}
	if groups, _ := st.Groups().ListForUser(ctx(), u.ID); len(groups) != 0 {
		t.Fatalf("orphaned membership survived group delete: %+v", groups)
	}

	// Deleting a user removes their memberships.
	g2 := mustGroup(t, st, ws.ID, "Support")
	if err := st.Groups().AddMember(ctx(), g2.ID, u.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := st.Users().Delete(ctx(), u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if members, _ := st.Groups().ListMembers(ctx(), g2.ID); len(members) != 0 {
		t.Fatalf("orphaned membership survived user delete: %+v", members)
	}
}

func testSessions(t *testing.T, st store.Store) {
	ws := mustWorkspace(t, st, "acme")
	u := mustUser(t, st, ws.ID, "u@acme.test")

	s := &model.UserSession{
		TokenHash: "hash-1", UserID: u.ID, WorkspaceID: ws.ID,
		UserAgent: "test-agent", IP: "127.0.0.1",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := st.Sessions().Create(ctx(), s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := st.Sessions().GetByHash(ctx(), "hash-1")
	if err != nil || got.UserID != u.ID || got.WorkspaceID != ws.ID {
		t.Fatalf("get session: %v %+v", err, got)
	}
	if got.Used || got.Revoked {
		t.Fatalf("new session must be unused and not revoked: %+v", got)
	}
	if got.UserAgent != "test-agent" || got.IP != "127.0.0.1" {
		t.Fatalf("session metadata not persisted: %+v", got)
	}

	if err := st.Sessions().MarkUsed(ctx(), s.ID); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	if got, _ = st.Sessions().GetByHash(ctx(), "hash-1"); !got.Used {
		t.Fatal("used flag not persisted")
	}
	// The consume is single-use and atomic: a second MarkUsed on an already-used
	// session is refused (ErrNotFound), which is what stops two concurrent
	// refreshes from both redeeming one token.
	if err := st.Sessions().MarkUsed(ctx(), s.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second MarkUsed must report not-found, got: %v", err)
	}

	if err := st.Sessions().Revoke(ctx(), s.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, _ = st.Sessions().GetByHash(ctx(), "hash-1"); !got.Revoked {
		t.Fatal("revoked flag not persisted")
	}

	// A rotation family: two sessions sharing a family_id are revoked together,
	// which is how a replayed (reused) refresh token severs the whole chain.
	const fam = "fam-abc"
	f1 := &model.UserSession{TokenHash: "hash-f1", FamilyID: fam, UserID: u.ID, WorkspaceID: ws.ID, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	f2 := &model.UserSession{TokenHash: "hash-f2", FamilyID: fam, UserID: u.ID, WorkspaceID: ws.ID, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := st.Sessions().Create(ctx(), f1); err != nil {
		t.Fatalf("create f1: %v", err)
	}
	if err := st.Sessions().Create(ctx(), f2); err != nil {
		t.Fatalf("create f2: %v", err)
	}
	if got, _ := st.Sessions().GetByHash(ctx(), "hash-f1"); got.FamilyID != fam {
		t.Fatalf("family_id not persisted: %+v", got)
	}
	if err := st.Sessions().RevokeFamily(ctx(), fam); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	if got, _ := st.Sessions().GetByHash(ctx(), "hash-f1"); !got.Revoked {
		t.Fatal("family member f1 not revoked")
	}
	if got, _ := st.Sessions().GetByHash(ctx(), "hash-f2"); !got.Revoked {
		t.Fatal("family member f2 not revoked")
	}
	// An empty family is a no-op, not a sweep of every empty-family row.
	if err := st.Sessions().RevokeFamily(ctx(), ""); err != nil {
		t.Fatalf("revoke empty family should be a no-op: %v", err)
	}

	// Logout everywhere: every live session for the user dies.
	for _, h := range []string{"hash-2", "hash-3"} {
		if err := st.Sessions().Create(ctx(), &model.UserSession{
			TokenHash: h, UserID: u.ID, WorkspaceID: ws.ID,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	if err := st.Sessions().RevokeAllForUser(ctx(), u.ID); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	for _, h := range []string{"hash-2", "hash-3"} {
		got, err := st.Sessions().GetByHash(ctx(), h)
		if err != nil || !got.Revoked {
			t.Fatalf("session %s not revoked: %v %+v", h, err, got)
		}
	}

	if _, err := st.Sessions().GetByHash(ctx(), "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// A token hash is unique: the same hash can never be inserted twice.
	if err := st.Sessions().Create(ctx(), &model.UserSession{
		TokenHash: "hash-1", UserID: u.ID, WorkspaceID: ws.ID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate token hash, got %v", err)
	}
}
