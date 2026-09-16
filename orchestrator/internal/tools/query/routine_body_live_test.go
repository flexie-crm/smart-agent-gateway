package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Real MySQL procedures with real bodies, held to a real policy.
//
// Until the routine-body reader existed, every one of these was refused as
// unreadable, which is safe and useless: an administrator ticked "call stored
// procedures" and nothing they had could be called. The engine stores a body as
// BEGIN ... END and the parser would not read it.
//
// So the clean ones here matter as much as the refused ones. A suite where
// everything is refused proves only that nothing works.
func TestRealMySQLProcedureBodies(t *testing.T) {
	cfg := fixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP PROCEDURE IF EXISTS b_clean", "DROP PROCEDURE IF EXISTS b_loop",
		"DROP PROCEDURE IF EXISTS b_else", "DROP PROCEDURE IF EXISTS b_cursor",
		"DROP PROCEDURE IF EXISTS b_hidden", "DROP PROCEDURE IF EXISTS b_handler",
		// A body with declarations, a loop and a branch, reading only what anybody may see.
		`CREATE PROCEDURE b_clean()
		 BEGIN
		   DECLARE n INT DEFAULT 0;
		   SELECT COUNT(*) FROM orders;
		   IF n = 0 THEN SELECT id, total FROM orders; ELSE SELECT id FROM orders; END IF;
		 END`,
		// The denied table, inside a loop.
		`CREATE PROCEDURE b_loop()
		 BEGIN WHILE 1=0 DO SELECT value FROM secret_keys; END WHILE; END`,
		// The denied table, only in the ELSE arm.
		`CREATE PROCEDURE b_else()
		 BEGIN IF 1=1 THEN SELECT id FROM orders; ELSE SELECT value FROM secret_keys; END IF; END`,
		// The denied table, only in a cursor's query.
		`CREATE PROCEDURE b_cursor()
		 BEGIN DECLARE c CURSOR FOR SELECT value FROM secret_keys; OPEN c; CLOSE c; END`,
		// The hidden field.
		`CREATE PROCEDURE b_hidden() BEGIN SELECT ssn FROM customers; END`,
	} {
		if _, err := conn.Exec(context.Background(), stmt, nil); err != nil {
			t.Fatalf("%.48s: %v", stmt, err)
		}
	}
	_ = conn.Close()

	s := Settings{Connection: cfg, Access: AccessBoth,
		Allowed: map[string]bool{datasource.CapCallRoutines: true},
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		}}

	// The one that must RUN, with declarations, a branch and a count in it.
	res, reason, err := run(context.Background(), s, "CALL b_clean()", nil, 0)
	if reason != "" || err != nil {
		t.Fatalf("a clean procedure body was refused: reason=%q err=%v", reason, err)
	}
	if res == nil || res.RowCount == 0 {
		t.Fatal("it ran but returned nothing")
	}

	for _, c := range []struct{ sql, because string }{
		{"CALL b_loop()", "the denied table is read inside a WHILE"},
		{"CALL b_else()", "the denied table is read only in the ELSE arm"},
		{"CALL b_cursor()", "the denied table is read only by a cursor"},
		{"CALL b_hidden()", "a hidden field, which a body cannot be rewritten to mask"},
	} {
		res, reason, err := run(context.Background(), s, c.sql, nil, 0)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if reason == "" {
			t.Errorf("%s RAN, and should not have: %s", c.sql, c.because)
			continue
		}
		if res != nil {
			out := fmt.Sprint(res.Rows)
			if strings.Contains(out, "the-key") || strings.Contains(out, "123-45-6789") {
				t.Errorf("%s was refused and the value came back anyway", c.sql)
			}
		}
	}
}
