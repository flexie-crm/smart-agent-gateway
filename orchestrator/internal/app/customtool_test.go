package app_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// The whole native-custom-tool path: create a query tool from the template, and
// it becomes a granted, self-describing tool whose handler runs a real query,
// with its password sealed in the row. This is the feature end to end.
func TestCreateAndRunACustomQueryTool(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("dba@acme.test")

	dsn := os.Getenv("SAG_TEST_DSN")
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "query", template.Input{
		Alias:   "local",
		Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("create custom tool: %v", err)
	}
	if created.Name != "query_local" || created.Kind != string(tool.KindCustom) || created.Template != "query" {
		t.Fatalf("the custom tool row is wrong: %+v", created)
	}

	// The password is sealed in the stored row, not in the clear. Check the
	// password field itself (the local DSN reuses "sag" as the username, so a
	// substring search over the whole config would trip on that).
	row, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("reload tool: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(row.Config, &stored); err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if parsed.Passwd != "" && stored["password"] == parsed.Passwd {
		t.Fatalf("the database password is stored in the clear: %v", stored["password"])
	}

	// It resolves into a loadout (active + open to the workspace) with a bound
	// handler and its deep guide from the template.
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{"query_local"}, nil, nil, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	schema, ok := loadout.Schema("query_local")
	if !ok {
		t.Fatal("the custom tool did not reach the loadout")
	}
	if len(schema.Guide) == 0 {
		t.Fatal("the custom tool did not get its template's guide")
	}
	// The engine reaches the model through the loaded description and the topics,
	// so it does not guess the wrong SQL dialect. This is the whole load path, not
	// just the bind: create, store, resolve, load.
	if !strings.Contains(schema.Description, "MySQL") {
		t.Fatalf("the loaded description does not name the engine: %q", schema.Description)
	}
	if len(schema.Topics) == 0 {
		t.Fatal("the custom tool got no drill-down topics")
	}
	handler, ok := loadout.Handlers["query_local"]
	if !ok {
		t.Fatal("the custom tool has no handler")
	}

	// The handler runs a real query.
	args, _ := json.Marshal(map[string]any{"sql": "SELECT 1 AS n"})
	res, err := handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args})
	if err != nil {
		t.Fatalf("run query: %v", err)
	}
	if res.Failed() || !strings.Contains(string(res.Content), `"columns"`) {
		t.Fatalf("the query did not return rows: %s", res.Content)
	}

	// Deleting it removes it; a built-in cannot be deleted this way.
	if err := e.app.Store.Tools().Delete(ctx, e.ws.ID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID); err == nil {
		t.Fatal("the custom tool survived deletion")
	}
}

// Editing a custom tool rewrites its settings and presentation while keeping a
// secret the administrator did not re-enter: the stored password survives a host
// change, and the edit view never hands the secret back.
func TestUpdateCustomToolKeepsSecret(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	dsn := os.Getenv("SAG_TEST_DSN")
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)
	if parsed.Passwd == "" {
		t.Skip("the test DSN has no password; skipping the secret-preservation edit test")
	}

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "query", template.Input{
		Alias: "edit_me", Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The edit view blanks the secret and hands back the rest.
	stored, _ := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	view, err := e.app.CustomToolForEdit(stored)
	if err != nil {
		t.Fatalf("edit view: %v", err)
	}
	if view.Variant != "mysql" {
		t.Fatalf("variant: %q", view.Variant)
	}
	if view.Settings["password"] != "" {
		t.Fatalf("the secret was handed back to the form: %v", view.Settings["password"])
	}
	if view.Settings["host"] != host {
		t.Fatalf("a non-secret setting did not come back: %v", view.Settings["host"])
	}

	// This tool was made without a policy, so it has nothing stored for either of
	// the two rules. A choice cannot hold nothing: it would show its first option
	// while the form held an empty string, and a save that named a table would
	// then be refused for a rule the form appeared to have made. The template's
	// default fills it, exactly as it would on a new tool.
	for _, key := range []string{"policy.table_mode", "policy.field_mode"} {
		if view.Settings[key] != "denylist" {
			t.Fatalf("%s came back as %q, so the form would show one rule and hold another", key, view.Settings[key])
		}
	}
	// The lists themselves stay empty. Nothing is kept back from a tool that has
	// never had anything kept back from it.
	for _, key := range []string{"policy.tables", "policy.fields"} {
		if value, present := view.Settings[key]; present && value != "" {
			t.Fatalf("%s came back as %q", key, value)
		}
	}

	// The edit view is the WHOLE form, so opening a tool is one request: the
	// driver's fields, its label, what this kind of tool is, and the parameters
	// whose descriptions are edited. The console used to fetch the entire
	// template catalogue to learn those three things about one tool.
	if len(view.Sections) == 0 {
		t.Fatal("the edit view carries no form, so the console has to ask for it separately")
	}
	if view.VariantLabel == "" || view.About == "" || len(view.Params) == 0 {
		t.Fatalf("the edit view is missing what the console needs from the template: label=%q about=%q params=%d",
			view.VariantLabel, view.About, len(view.Params))
	}
	// The settings are the tool's OWN, and only those: a field it has no value for
	// is absent, never filled in from the template's default. The default seeds a
	// NEW tool, and handing it back here would offer somebody a setting they had
	// cleared and write it to the row the moment they saved.
	if _, invented := view.Settings["ssh.host"]; invented {
		t.Fatal("the edit view invented a value for a field this tool never set")
	}

	// Edit: change a non-secret (the access mode), the description and guide, and
	// a param description; leave the password blank (unchanged). The database
	// stays information_schema so the edited tool still connects below.
	settings := view.Settings
	settings["access"] = "both"
	_, err = e.app.UpdateCustomTool(ctx, e.ws.ID, created.ID, template.Input{
		Settings:          settings,
		Description:       "Now reads and writes.",
		Guide:             "Both reads and writes are allowed.",
		ParamDescriptions: map[string]string{"sql": "A statement against the schema."},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// The stored password is still sealed (not the plaintext, not blank), the
	// non-secret change persisted, and the risk rose with the access. That the
	// carried secret actually opens is proven below, where the edited tool runs.
	reloaded, _ := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	var cfg map[string]any
	_ = json.Unmarshal(reloaded.Config, &cfg)
	if cfg["password"] == parsed.Passwd || cfg["password"] == "" {
		t.Fatalf("the password was lost or exposed on edit: %v", cfg["password"])
	}
	if cfg["access"] != "both" {
		t.Fatalf("the non-secret change did not persist: %v", cfg["access"])
	}
	if reloaded.Risk != string(tool.RiskInternalWrite) {
		t.Fatalf("the risk did not rise with the access: %q", reloaded.Risk)
	}
	if reloaded.Description != "Now reads and writes." || reloaded.Guide != "Both reads and writes are allowed." {
		t.Fatalf("the presentation did not update: %+v", reloaded)
	}
	if !strings.Contains(string(reloaded.InputSchema), "A statement against the schema.") {
		t.Fatalf("the parameter description did not update: %s", reloaded.InputSchema)
	}

	// The edited tool still binds and runs, proving the carried secret is usable.
	user := e.user("dba2@acme.test")
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{"query_edit_me"}, nil, nil, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	handler, ok := loadout.Handlers["query_edit_me"]
	if !ok {
		t.Fatal("the edited tool has no handler")
	}
	args, _ := json.Marshal(map[string]any{"sql": "SELECT 1 AS n"})
	if res, err := handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args}); err != nil || res.Failed() {
		t.Fatalf("the edited tool did not run: %v %s", err, res.Content)
	}
}

// A save that changes nothing still succeeds. MySQL reports zero affected rows
// for an UPDATE whose new values equal the old, and a blank secret is carried
// back byte for byte, so re-saving an unchanged tool is a no-op UPDATE that must
// not read as a missing tool.
func TestUpdateCustomToolNoOpSucceeds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	dsn := os.Getenv("SAG_TEST_DSN")
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "query", template.Input{
		Alias: "noop", Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stored, _ := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	view, err := e.app.CustomToolForEdit(stored)
	if err != nil {
		t.Fatalf("edit view: %v", err)
	}

	// The same input twice: the second save produces byte-identical columns (the
	// blank secret is carried from the now-sealed stored value), so it is a true
	// no-op UPDATE. Both must succeed.
	in := template.Input{
		Settings: view.Settings, DisplayName: "Stable", Description: "Stable.",
		Guide: "Stable.", ParamDescriptions: view.ParamDescriptions,
	}
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, created.ID, in); err != nil {
		t.Fatalf("first edit failed: %v", err)
	}
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, created.ID, in); err != nil {
		t.Fatalf("a no-op re-save read as not found: %v", err)
	}
}

// Testing an edit carries a blank secret forward from the stored tool, so a
// connection can be checked after changing a non-secret without re-entering the
// password. A wrong password typed through still fails, proving the field is
// really used, not ignored.
func TestTestCustomToolEditCarriesSecret(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	dsn := os.Getenv("SAG_TEST_DSN")
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)
	if parsed.Passwd == "" {
		t.Skip("the test DSN has no password; skipping the edit-test secret suite")
	}

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "query", template.Input{
		Alias: "probe_edit", Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The form's settings, with the secret blanked as the edit view returns it.
	stored, _ := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	view, _ := e.app.CustomToolForEdit(stored)

	// A blank password is carried forward: the connection succeeds.
	if err := e.app.TestCustomToolEdit(ctx, e.ws.ID, created.ID, view.Settings); err != nil {
		t.Fatalf("a blank secret was not carried forward for the test: %v", err)
	}

	// A wrong password typed through is actually used: the connection fails.
	wrong := map[string]any{}
	for k, v := range view.Settings {
		wrong[k] = v
	}
	wrong["password"] = "definitely-not-the-password"
	if err := e.app.TestCustomToolEdit(ctx, e.ws.ID, created.ID, wrong); err == nil {
		t.Fatal("a wrong password should fail the edit test")
	}
}

// TestConnection opens a live connection for a stored config without the secret
// being re-entered, because the stored config's secrets are opened first.
func TestCustomToolTestConnection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	dsn := os.Getenv("SAG_TEST_DSN")
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "query", template.Input{
		Alias:   "probe",
		Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row, _ := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)

	if err := e.app.TestCustomTool(ctx, "query", row.Config); err != nil {
		t.Fatalf("the stored connection did not test cleanly: %v", err)
	}

	// A connection to a dead host fails the test.
	badConfig := json.RawMessage(`{"driver":"mysql","access":"read","host":"203.0.113.1","port":3306,"database":"x","username":"u","tls":{"mode":"disable"}}`)
	if err := e.app.TestCustomTool(ctx, "query", badConfig); err == nil {
		t.Fatal("a dead host should fail the connection test")
	}
}
