package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// A routine's identity is its schema AND its name.
//
// Two things were measured against a live SQL Server before this, with
// call_routines TICKED and a policy denying payroll:
//
//	SELECT app.f_same()          ->  [[1000.00]]
//	SELECT hidden_schema.f_out() ->  [[1000.00]]
//
// The first because the snapshot keyed routines by bare name, so dbo.f_same and
// app.f_same collapsed into one entry and whichever body the database happened
// to return last is the one that was read. The second because a name the
// snapshot had never heard of was taken for a built-in and waved through, and a
// schema the driver does not read is exactly where a routine hides.
//
// A qualified call is now stored code whether or not the snapshot knows it, and
// what it cannot account for it refuses.
func TestARoutineIsIdentifiedBySchemaAndName(t *testing.T) {
	cfg := msFixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE SCHEMA app",
		"CREATE SCHEMA hidden_schema",
		"CREATE FUNCTION dbo.f_same() RETURNS nvarchar(100) AS BEGIN RETURN N'harmless' END",
		"CREATE FUNCTION app.f_same() RETURNS nvarchar(100) AS BEGIN RETURN (SELECT TOP 1 CAST(amount AS nvarchar(100)) FROM payroll) END",
		"CREATE FUNCTION hidden_schema.f_out() RETURNS nvarchar(100) AS BEGIN RETURN (SELECT TOP 1 CAST(amount AS nvarchar(100)) FROM payroll) END",
	} {
		if _, err := conn.Exec(context.Background(), stmt, nil); err != nil {
			t.Fatalf("setup %.50s: %v", stmt, err)
		}
	}
	_ = conn.Close()

	s := Settings{Connection: cfg, Access: AccessBoth,
		Allowed: map[string]bool{datasource.CapCallRoutines: true},
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "payroll",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		}}

	// The one in a schema the snapshot reads, whose body is harmless, still runs.
	// Without this the refusals below could not be told from refusing everything.
	res, reason, err := run(context.Background(), s, "SELECT dbo.f_same()", nil, 0)
	if err != nil || reason != "" {
		t.Fatalf("a harmless routine was refused: reason=%q err=%v", reason, err)
	}
	if !strings.Contains(fmt.Sprint(res.Rows), "harmless") {
		t.Fatalf("it ran but returned %v", res.Rows)
	}

	for _, c := range []struct{ sql, because string }{
		{"SELECT app.f_same()", "another schema's routine of the same name"},
		{"SELECT hidden_schema.f_out()", "a schema this snapshot does not read"},
	} {
		res, reason, err := run(context.Background(), s, c.sql, nil, 0)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if reason == "" {
			t.Errorf("%s RAN, and should not have (%s): %v", c.sql, c.because, res.Rows)
			continue
		}
		if res != nil && strings.Contains(fmt.Sprint(res.Rows), "1000") {
			t.Errorf("%s was refused and the value came back anyway", c.sql)
		}
	}
}

// A view whose FROM is a set-returning function is held to what that function
// reads, and the function itself needs the capability like any other routine.
func TestAViewOverAFunctionIsHeldToWhatTheFunctionReads(t *testing.T) {
	cfg := pgFixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP VIEW IF EXISTS tvf_view",
		"DROP FUNCTION IF EXISTS f_rows()",
		"CREATE FUNCTION f_rows() RETURNS TABLE(id int, value text) LANGUAGE sql AS $$ SELECT id, value FROM secret_keys $$",
		"CREATE VIEW tvf_view AS SELECT t.id, t.value FROM f_rows() AS t",
	} {
		if _, err := conn.Exec(context.Background(), stmt, nil); err != nil && !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("setup %.50s: %v", stmt, err)
		}
	}
	_ = conn.Close()

	s := Settings{Connection: cfg, Access: AccessBoth, Policy: sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}}
	for _, sql := range []string{"SELECT * FROM tvf_view", "SELECT value FROM tvf_view", "SELECT * FROM f_rows()"} {
		res, reason, err := run(context.Background(), s, sql, nil, 0)
		if err != nil {
			continue
		}
		if reason == "" {
			t.Errorf("%s RAN, and should not have: %v", sql, res.Rows)
		}
	}
}

// The access mode is enforced by the DATABASE as well as by this package: a read
// runs inside a read-only transaction, so a stored function that writes cannot
// write on a read-only tool even where nothing here read the function call.
// This is the second guard the design has always claimed, and it is real.
func TestAReadOnlyToolCannotWriteThroughAFunction(t *testing.T) {
	for _, c := range []struct {
		name  string
		cfg   func(*testing.T) datasource.Config
		setup []string
	}{
		{"mysql", fixture, []string{
			"DROP FUNCTION IF EXISTS f_write",
			"CREATE TABLE IF NOT EXISTS marks (id int)",
			"DELETE FROM marks",
			"CREATE FUNCTION f_write() RETURNS int MODIFIES SQL DATA BEGIN INSERT INTO marks VALUES (1); RETURN 1; END",
		}},
		{"postgres", pgFixture, []string{
			"DROP FUNCTION IF EXISTS f_write()",
			"CREATE TABLE IF NOT EXISTS marks (id int)",
			"DELETE FROM marks",
			"CREATE FUNCTION f_write() RETURNS int LANGUAGE plpgsql AS $$ BEGIN INSERT INTO marks VALUES (1); RETURN 1; END $$",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(t)
			conn, err := datasource.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.setup {
				_, _ = conn.Exec(context.Background(), stmt, nil)
			}
			_ = conn.Close()

			s := Settings{Connection: cfg, Access: AccessRead}
			_, _, _ = run(context.Background(), s, "SELECT f_write()", nil, 0)
			res, reason, err := run(context.Background(), s, "SELECT COUNT(*) FROM marks", nil, 0)
			if err != nil || reason != "" {
				t.Fatalf("could not read back: reason=%q err=%v", reason, err)
			}
			if got := fmt.Sprint(res.Rows); !strings.Contains(got, "0") {
				t.Errorf("a read-only tool wrote through a function: marks holds %s", got)
			}
		})
	}
}
