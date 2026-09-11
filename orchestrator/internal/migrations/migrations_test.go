package migrations_test

import (
	"context"
	"os"
	"testing"

	"flexie.io/sag/internal/migrations"
	"flexie.io/sag/internal/testdb"
)

// A migration that changes DATA rather than shape proves nothing by applying
// cleanly: the deployment it runs on has none of the rows it is about, so a
// green run and a no-op look identical. This applies it to a database that DOES
// have the row, which is the only way to find out whether it does the thing.
//
// It works by rolling 52 back on the already-migrated scratch database, putting
// the old row in, and rolling forward again. That runs the REAL migration, from
// the same embedded file the binary ships, rather than a copy of its SQL that
// could quietly stop resembling it.
func TestThePersonalOwnerIsRenamedRatherThanDuplicated(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the migration suite")
	}
	st, _ := testdb.Open(t, dsn, "migrations")
	ctx := context.Background()
	db := st.DB()

	// To the version just BEFORE the one under test, by number rather than by
	// counting steps: "down once" tests whichever migration happens to be last.
	if err := migrations.DownTo(ctx, db, 51); err != nil {
		t.Fatalf("roll back the owner rename: %v", err)
	}
	t.Cleanup(func() {
		// Whatever this test did, leave the database at the version every other
		// package expects to find it at.
		if err := migrations.Run(ctx, db, "up"); err != nil {
			t.Errorf("restore the database: %v", err)
		}
	})

	// An installation seeded before the rename. The name is NOT the seeded
	// "Owner": setup asks what to call you first, so by the time anybody
	// upgrades they have already renamed themselves, and a migration that keyed
	// on the name would skip exactly the rows it exists for.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO users (email, name, password_hash, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, NOW(3), NOW(3))",
		"owner@personal.invalid", "Sam", "x", "active"); err != nil {
		t.Fatalf("seed an installation at the old address: %v", err)
	}
	// And somebody else, at an address the migration must not touch.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO users (email, name, password_hash, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, NOW(3), NOW(3))",
		"somebody@example.com", "Somebody", "x", "active"); err != nil {
		t.Fatalf("seed a bystander: %v", err)
	}

	if err := migrations.Run(ctx, db, "up"); err != nil {
		t.Fatalf("apply the owner rename: %v", err)
	}

	var name string
	if err := db.QueryRowContext(ctx,
		"SELECT name FROM users WHERE email = ?", "owner@localhost.fx").Scan(&name); err != nil {
		t.Fatalf("the owner was not renamed to the address the seed now looks for: %v", err)
	}
	if name != "Sam" {
		t.Fatalf("the renamed owner is %q, so it renamed the wrong row", name)
	}

	// The old address is gone rather than duplicated: a row left behind is the
	// second owner this migration exists to prevent.
	var leftBehind int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE email = ?", "owner@personal.invalid").Scan(&leftBehind); err != nil {
		t.Fatalf("count the old address: %v", err)
	}
	if leftBehind != 0 {
		t.Fatalf("%d rows still hold the old address", leftBehind)
	}

	var bystander string
	if err := db.QueryRowContext(ctx,
		"SELECT name FROM users WHERE email = ?", "somebody@example.com").Scan(&bystander); err != nil {
		t.Fatalf("the migration touched a row it had no business touching: %v", err)
	}
}

// The projected MCP tools are renamed to something a model will accept.
//
// The same reasoning as the test above: a data migration that runs on a
// database with none of the rows it is about proves nothing. This one matters
// more than most, because until it runs the installation has a working
// connection whose tools break EVERY request the agent makes.
func TestProjectedToolsAreRenamedToSomethingAModelAccepts(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the migration suite")
	}
	st, _ := testdb.Open(t, dsn, "migrations")
	ctx := context.Background()
	db := st.DB()

	if err := migrations.DownTo(ctx, db, 52); err != nil {
		t.Fatalf("roll back the tool rename: %v", err)
	}
	t.Cleanup(func() {
		if err := migrations.Run(ctx, db, "up"); err != nil {
			t.Errorf("restore the database: %v", err)
		}
	})

	var workspaceID int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM workspaces LIMIT 1").Scan(&workspaceID); err != nil {
		res, err := db.ExecContext(ctx,
			"INSERT INTO workspaces (slug, name, description, created_at, updated_at) "+
				"VALUES (?, ?, ?, NOW(3), NOW(3))",
			"tool-rename", "Renaming", "for the tool rename test")
		if err != nil {
			t.Fatalf("seed a workspace: %v", err)
		}
		workspaceID, _ = res.LastInsertId()
	}

	res, err := db.ExecContext(ctx,
		"INSERT INTO mcp_servers (workspace_id, name, url, auth_type, status, tool_prefix, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, NOW(3), NOW(3))",
		workspaceID, "Flexie CRM - FX", "https://fx.example.test/mcp", "oauth", "active", "flexie-crm-fx")
	if err != nil {
		t.Fatalf("seed a connection: %v", err)
	}
	serverID, _ := res.LastInsertId()

	// A tool as it was projected before the fix, and a built-in beside it that
	// the migration has no business touching.
	insertTool := func(name, kind string, server any, remote any) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			"INSERT INTO tools (workspace_id, name, kind, friendly_name, description, risk, mcp_server_id, remote_name) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			workspaceID, name, kind, name, "seeded", "read_only", server, remote); err != nil {
			t.Fatalf("seed the tool %q: %v", name, err)
		}
	}
	insertTool("flexie-crm-fx.invoice", "mcp", serverID, "invoice")
	insertTool("flexie-crm-fx.custom_entity_discover", "mcp", serverID, "custom_entity_discover")
	insertTool("http_request", "builtin", nil, nil)

	if err := migrations.Run(ctx, db, "up"); err != nil {
		t.Fatalf("apply the tool rename: %v", err)
	}

	names := map[string]bool{}
	rows, err := db.QueryContext(ctx, "SELECT name FROM tools WHERE workspace_id = ?", workspaceID)
	if err != nil {
		t.Fatalf("read the tools back: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names[name] = true
	}

	for _, want := range []string{"flexie-crm-fx_invoice", "flexie-crm-fx_custom_entity_discover"} {
		if !names[want] {
			t.Errorf("%q is missing; the projected tools were not renamed (have: %v)", want, names)
		}
	}
	for _, gone := range []string{"flexie-crm-fx.invoice", "flexie-crm-fx.custom_entity_discover"} {
		if names[gone] {
			t.Errorf("%q survived, and it fails every request it appears in", gone)
		}
	}
	// A built-in is not the migration's business, and a migration that rewrote
	// one would be renaming a tool the code looks up by name.
	if !names["http_request"] {
		t.Error("the migration touched a built-in tool")
	}
}
