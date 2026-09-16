package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// A view whose column is a stored function call.
//
// The tables in a view's own FROM are not the whole story when one of its
// columns is a routine call: the routine reads whatever it likes. Measured on
// all three engines before the catalog learned to follow it, with call_routines
// deliberately unticked and nothing wrong with the statement itself:
//
//	CREATE VIEW leak_view AS SELECT o.id AS id, f_leak() AS code FROM orders o
//	SELECT code FROM leak_view   ->   [[the-key]]
//
// The statement names no function, so the routine gate never fires; the view was
// credited with reading orders, which nobody keeps back. What was wrong was the
// snapshot, not the reading of the statement.
//
// The reason differs by dialect and both are right: where the routine body can
// be read, what it reads is folded into the view and the policy refuses the
// table by name; where it cannot (a MySQL function body is stored as
// RETURN (...), which that parser does not read), the view is unaccounted for
// and refused as such.
func TestAViewThatCallsARoutineIsHeldToWhatTheRoutineReads(t *testing.T) {
	for _, c := range []struct {
		name   string
		cfg    func(*testing.T) datasource.Config
		setup  []string
		policy sqlguard.Policy
		secret string
	}{
		{
			name: "mysql", cfg: fixture,
			setup: []string{
				"DROP VIEW IF EXISTS leak_view", "DROP FUNCTION IF EXISTS f_leak",
				"CREATE FUNCTION f_leak() RETURNS varchar(100) READS SQL DATA RETURN (SELECT value FROM secret_keys LIMIT 1)",
				"CREATE VIEW leak_view AS SELECT o.id AS id, f_leak() AS code FROM orders o",
			},
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			secret: "the-key",
		},
		{
			name: "postgres", cfg: pgFixture,
			setup: []string{
				"DROP VIEW IF EXISTS leak_view2", "DROP FUNCTION IF EXISTS f_leak()",
				"CREATE FUNCTION f_leak() RETURNS text LANGUAGE sql AS $$ SELECT value FROM secret_keys LIMIT 1 $$",
				"CREATE VIEW leak_view2 AS SELECT o.id AS id, f_leak() AS code FROM orders o",
			},
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			secret: "the-key",
		},
		{
			name: "sqlserver", cfg: msFixture,
			setup: []string{
				"CREATE FUNCTION dbo.f_leak() RETURNS nvarchar(100) AS BEGIN RETURN (SELECT TOP 1 CAST(amount AS nvarchar(100)) FROM payroll) END",
				"CREATE VIEW leak_view3 AS SELECT o.id AS id, dbo.f_leak() AS code FROM orders o",
			},
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "payroll",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			secret: "1000",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(t)
			conn, err := datasource.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.setup {
				if _, err := conn.Exec(context.Background(), stmt, nil); err != nil &&
					!strings.Contains(err.Error(), "does not exist") {
					t.Logf("  setup: %s -> %v", stmt, err)
				}
			}
			_ = conn.Close()

			view := map[string]string{"mysql": "leak_view", "postgres": "leak_view2", "sqlserver": "leak_view3"}[c.name]
			// call_routines DELIBERATELY unticked.
			s := Settings{Connection: cfg, Access: AccessBoth, Policy: c.policy}
			res, reason, err := run(context.Background(), s, "SELECT code FROM "+view, nil, 0)
			switch {
			case reason != "":
				t.Logf("  %-9s refused: %.90s", c.name, reason)
			case err != nil:
				t.Logf("  %-9s engine error: %.90s", c.name, err.Error())
			default:
				out := fmt.Sprint(res.Rows)
				if strings.Contains(out, c.secret) {
					t.Errorf("  %-9s LEAKED %q via a view that calls a routine: %.90s", c.name, c.secret, out)
				} else {
					t.Errorf("  %-9s ran and returned %q, which is neither the secret nor a refusal", c.name, out)
				}
			}
		})
	}
}
