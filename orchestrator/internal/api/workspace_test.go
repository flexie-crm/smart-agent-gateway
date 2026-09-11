package api

import (
	"context"
	"net/http"
	"testing"

	"flexie.io/sag/internal/model"
)

func TestWorkspaceCRUD(t *testing.T) {
	env := newTestEnv(t)
	admin := env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// Create derives the slug from the name and makes the creator a member:
	// a workspace you cannot enter is a workspace you cannot configure.
	rec := env.do(http.MethodPost, "/v1/workspaces", token,
		map[string]any{"name": "Sales Team", "description": "Where the sales team works."})
	env.expectStatus(rec, http.StatusCreated)
	var created workspaceAdminBody
	env.decode(rec, &created)
	if created.Slug != "sales-team" || created.Status != model.StatusActive {
		t.Fatalf("create did not derive the slug or default the status: %+v", created)
	}
	// The description is the field the person actually fills in.
	if created.Description != "Where the sales team works." {
		t.Fatalf("create did not keep the description: %+v", created)
	}
	mine, err := env.app.Store.Workspaces().ListForUser(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	joined := false
	for _, ws := range mine {
		joined = joined || ws.ID == created.ID
	}
	if !joined {
		t.Fatalf("the creator did not join the workspace they created: %+v", mine)
	}

	// The slug is the tenant key; the collision lands on the slug field.
	env.expectFields(env.do(http.MethodPost, "/v1/workspaces", token,
		map[string]any{"name": "Sales Team"}), http.StatusConflict, "conflict", map[string]string{
		"slug": "another workspace already uses this slug",
	})

	// A blank name and a malformed explicit slug answer together.
	env.expectFields(env.do(http.MethodPost, "/v1/workspaces", token,
		map[string]any{"name": " ", "slug": "Bad Slug"}), http.StatusBadRequest, "invalid_request",
		map[string]string{
			"name": "a name is required",
			"slug": "lowercase letters, digits and single dashes only",
		})

	// Update writes name, slug, and the status switch; an unknown status is
	// refused on its field.
	env.expectFields(env.do(http.MethodPut, "/v1/workspaces/"+itoa(created.ID), token,
		map[string]any{"name": "Sales", "slug": "sales", "status": "paused"}),
		http.StatusBadRequest, "invalid_request", map[string]string{
			"status": "must be active or suspended",
		})
	rec = env.do(http.MethodPut, "/v1/workspaces/"+itoa(created.ID), token,
		map[string]any{"name": "Sales", "slug": "sales", "status": model.StatusSuspended,
			"description": "The renamed sales workspace."})
	env.expectStatus(rec, http.StatusOK)
	var updated workspaceAdminBody
	env.decode(rec, &updated)
	if updated.Name != "Sales" || updated.Slug != "sales" || updated.Status != model.StatusSuspended {
		t.Fatalf("update did not land: %+v", updated)
	}
	if updated.Description != "The renamed sales workspace." {
		t.Fatalf("update did not keep the description: %+v", updated)
	}

	// The ground the caller stands on is off limits.
	env.expectStatus(env.do(http.MethodDelete, "/v1/workspaces/"+itoa(env.ws.ID), token, nil),
		http.StatusConflict)
	// Another workspace can go, once.
	env.expectStatus(env.do(http.MethodDelete, "/v1/workspaces/"+itoa(created.ID), token, nil),
		http.StatusNoContent)
	env.expectStatus(env.do(http.MethodDelete, "/v1/workspaces/"+itoa(created.ID), token, nil),
		http.StatusNotFound)
}

func TestANewWorkspaceCanDoSomethingTheMomentItExists(t *testing.T) {
	// The server offers the build's tools to every workspace AT BOOT. A
	// workspace made after that boot got none of them, so its agents could do
	// nothing at all, and every screen looked correctly configured while they
	// did it. The only cure was a restart, which is not a thing anybody would
	// think to try.
	//
	// Found on the first real deployment: the stack came up, `sag bootstrap`
	// made the workspace ninety seconds later, and the tools table stayed empty.
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/workspaces", token,
		map[string]any{"name": "Late Arrival", "description": "Made long after the server started."})
	env.expectStatus(rec, http.StatusCreated)
	var created workspaceAdminBody
	env.decode(rec, &created)

	tools, err := env.app.Store.Tools().List(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("a workspace was created with no tools, so nothing in it can do anything")
	}
	// The same set the build offers, not an arbitrary non-empty one.
	if want := len(env.app.Tools.All()); len(tools) != want {
		t.Errorf("workspace got %d tools, the build has %d", len(tools), want)
	}
}

func TestWorkspacesRequireTheirPermissions(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("viewer@acme.test", "dev-Passw0rd!", model.PermWorkspacesView)
	viewer, _ := env.login("viewer@acme.test", "dev-Passw0rd!")
	env.createUser("nobody@acme.test", "dev-Passw0rd!")
	nobody, _ := env.login("nobody@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/workspaces", viewer, nil), http.StatusOK)
	env.expectStatus(env.do(http.MethodPost, "/v1/workspaces", viewer,
		map[string]any{"name": "Mine"}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodDelete, "/v1/workspaces/999999", viewer, nil),
		http.StatusForbidden)
	env.expectStatus(env.do(http.MethodGet, "/v1/workspaces", nobody, nil), http.StatusForbidden)
}

// TestPermissionCatalogSpeaksHuman pins the /v1/permissions contract: every
// entry carries the key the API validates AND the words a person reads, so
// the roles form never shows anyone a machine key.
func TestPermissionCatalogSpeaksHuman(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodGet, "/v1/permissions", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var catalog []model.PermissionInfo
	env.decode(rec, &catalog)

	if len(catalog) != len(model.KnownPermissions) {
		t.Fatalf("the catalog and the validation list disagree: %d vs %d",
			len(catalog), len(model.KnownPermissions))
	}
	words := map[string]model.PermissionInfo{}
	for _, p := range catalog {
		if p.Key == "" || p.Area == "" || p.Label == "" {
			t.Fatalf("a catalog entry is missing its words: %+v", p)
		}
		words[p.Key] = p
	}
	// The full CRUD sets this change added are present, with their words.
	if p := words[model.PermBrainsCreate]; p.Area != "Brains" || p.Label != "Create" {
		t.Fatalf("brains:create speaks the wrong words: %+v", p)
	}
	if p := words[model.PermWorkspacesDelete]; p.Area != "Workspaces" || p.Label != "Delete" {
		t.Fatalf("workspaces:delete speaks the wrong words: %+v", p)
	}
	if p := words[model.PermSuperuser]; p.Label == "" || p.Area != "Everything" {
		t.Fatalf("the superuser entry lost its warning label: %+v", p)
	}
}
