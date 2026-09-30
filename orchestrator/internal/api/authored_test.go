package api

import (
	"fmt"
	"net/http"
	"testing"

	"flexie.io/sag/internal/model"
)

// Who made it comes from the REQUEST, and a client cannot say otherwise.
//
// This is the security half of the feature, and it is tested here rather than
// only in the store because the store is handed an actor: whether that actor is
// the authenticated person or something a client sent is decided in the
// handler, and nothing below it can tell the difference.
//
// The first assertion is the strong one. `created_by_name` is not a field of
// any request body, and decodeJSON refuses unknown fields (respond.go), so a
// body carrying it is rejected outright: a client cannot even name the column,
// let alone set it. The rest prove the row then records the person who actually
// sent the request.
func TestWhoMadeItComesFromTheRequestAndNotTheBody(t *testing.T) {
	env := newTestEnv(t)
	// createUser names a person by their email, so that is the name to expect.
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	const who = "admin@acme.test"

	t.Run("a body claiming to be somebody is refused", func(t *testing.T) {
		for _, path := range []string{"/v1/vendors", "/v1/groups", "/v1/roles", "/v1/workspaces"} {
			rec := env.do(http.MethodPost, path, token, map[string]any{
				"name": "Attempt", "vendor_key": "anthropic",
				"created_by": 999, "created_by_name": "Somebody Else",
			})
			// 400, because the field does not exist in the request contract.
			// Not "ignored": refused.
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST %s with created_by_name = %d, want %d: a client must not be able to name the column",
					path, rec.Code, http.StatusBadRequest)
			}
		}
	})

	t.Run("a vendor records the person who sent the request", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/v1/vendors", token, map[string]any{
			"vendor_key": "anthropic", "name": "Anthropic", "credentials": "sk-test",
		})
		env.expectStatus(rec, http.StatusCreated)
		var body vendorBody
		env.decode(rec, &body)

		vendor, err := env.app.Store.Vendors().GetByID(t.Context(), env.ws.ID, body.ID)
		if err != nil {
			t.Fatalf("read the vendor: %v", err)
		}
		if vendor.CreatedByName != who {
			t.Errorf("created by %q, want %q", vendor.CreatedByName, who)
		}
		if vendor.CreatedBy == 0 {
			t.Error("the vendor is not linked to the person who made it")
		}
		if vendor.UpdatedByName != who {
			t.Errorf("last changed by %q on creation", vendor.UpdatedByName)
		}
	})

	t.Run("an edit by somebody else moves only the second pair", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/v1/roles", token, map[string]any{
			"name": "Curator", "permissions": []string{model.PermBrainsView},
		})
		env.expectStatus(rec, http.StatusCreated)
		var body roleBody
		env.decode(rec, &body)

		env.createUser("second@acme.test", "dev-Passw0rd!", model.PermSuperuser)
		other, _ := env.login("second@acme.test", "dev-Passw0rd!")
		env.expectStatus(env.do(http.MethodPut, fmt.Sprintf("/v1/roles/%d", body.ID), other,
			map[string]any{"name": "Curator, renamed", "permissions": []string{model.PermBrainsView}}),
			http.StatusOK)

		role, err := env.app.Store.Roles().GetByID(t.Context(), env.ws.ID, body.ID)
		if err != nil {
			t.Fatalf("read the role: %v", err)
		}
		if role.CreatedByName != who {
			t.Errorf("an edit rewrote who made the role: %q", role.CreatedByName)
		}
		if role.UpdatedByName != "second@acme.test" {
			t.Errorf("the edit was recorded as %q", role.UpdatedByName)
		}
	})

	t.Run("a group and a workspace record it too", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": "Support"})
		env.expectStatus(rec, http.StatusCreated)
		var group groupBody
		env.decode(rec, &group)
		stored, err := env.app.Store.Groups().GetByID(t.Context(), env.ws.ID, group.ID)
		if err != nil {
			t.Fatalf("read the group: %v", err)
		}
		if stored.CreatedByName != who {
			t.Errorf("the group says %q", stored.CreatedByName)
		}

		rec = env.do(http.MethodPost, "/v1/workspaces", token,
			map[string]any{"name": "Second", "description": "another one"})
		env.expectStatus(rec, http.StatusCreated)
		var ws workspaceBody
		env.decode(rec, &ws)
		second, err := env.app.Store.Workspaces().GetByID(t.Context(), ws.ID)
		if err != nil {
			t.Fatalf("read the workspace: %v", err)
		}
		if second.CreatedByName != who {
			t.Errorf("the workspace says %q", second.CreatedByName)
		}
	})
}
