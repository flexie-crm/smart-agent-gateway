package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Creating a routine that reads a hidden field, against real databases.
//
// The analyzers' own tests show the TEXT being rewritten. They cannot show that
// the rewritten CREATE is SQL the engine accepts, nor that the routine it makes
// actually hands back the stand-in when somebody calls it. Both are the whole
// point, so both are checked here, on all three engines, by making the routine
// and then reading what it returns.
func TestARoutineMadeWithAHiddenFieldReturnsTheStandIn(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     func(*testing.T) datasource.Config
		policy  sqlguard.Policy
		drop    string
		create  string
		call    string
		secret  string
		cleanUp string
	}{
		{
			name: "mysql", cfg: fixture,
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			drop:   "DROP PROCEDURE IF EXISTS rw_p",
			create: "CREATE PROCEDURE rw_p() BEGIN SELECT id, ssn FROM customers; END",
			call:   "CALL rw_p()", secret: "123-45-6789",
		},
		{
			name: "postgres", cfg: pgFixture,
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			drop:   "DROP FUNCTION IF EXISTS rw_f()",
			create: "CREATE FUNCTION rw_f() RETURNS text LANGUAGE sql AS $$ SELECT ssn FROM customers LIMIT 1 $$",
			call:   "SELECT rw_f() AS v", secret: "123-45-6789",
		},
		{
			name: "sqlserver", cfg: msFixture,
			policy: sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "payroll",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"},
			drop:   "",
			create: "CREATE PROCEDURE dbo.rw_p AS BEGIN SELECT id, ssn FROM customers; END",
			call:   "EXEC dbo.rw_p", secret: msRealSSN,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg(t)
			if c.drop != "" {
				conn, err := datasource.Open(cfg)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = conn.Exec(context.Background(), c.drop, nil)
				_ = conn.Close()
			}

			s := Settings{Connection: cfg, Access: AccessBoth, Policy: c.policy,
				Allowed: map[string]bool{
					datasource.CapCreateRoutines: true,
					datasource.CapCallRoutines:   true,
				}}

			// 1. The CREATE is accepted, which proves the rewritten text is SQL the
			//    engine will take. A rewrite the database rejects is worse than a
			//    refusal, because the agent is told its own SQL was wrong.
			if _, reason, err := run(context.Background(), s, c.create, nil, 0); reason != "" || err != nil {
				t.Fatalf("the rewritten CREATE was not accepted: reason=%q err=%v", reason, err)
			}

			// 2. Calling it hands back the stand-in, not the value.
			res, reason, err := run(context.Background(), s, c.call, nil, 0)
			if reason != "" || err != nil {
				t.Fatalf("calling it: reason=%q err=%v", reason, err)
			}
			out := fmt.Sprint(res.Rows)
			if strings.Contains(out, c.secret) {
				t.Errorf("the routine handed back the real value: %.120s", out)
			}
			if !strings.Contains(out, sqlguard.Hidden) {
				t.Errorf("the routine returned neither the value nor the stand-in: %.120s", out)
			}
		})
	}
}
