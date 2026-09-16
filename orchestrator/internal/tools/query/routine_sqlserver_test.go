package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Calling a stored routine, against a real SQL Server.
//
// The whole of this feature rests on one claim: a routine is held to the policy
// by READING ITS BODY. Nothing in the analyzer's own tests can check that,
// because there the bodies are strings a test wrote. Here they are what the
// server stored, fetched back through the driver's catalogue query.
//
// Every case asserts in both directions where it can: a routine that should run
// runs and returns rows, and one that should not is refused with a reason.

func routineFixture(t *testing.T) Settings {
	t.Helper()
	cfg := msFixture(t) // customers(id,email,ssn,profile), orders, payroll, scratch

	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Each on its own, because CREATE PROCEDURE must begin a batch.
	for _, stmt := range []string{
		// Reads only what anybody may see.
		"CREATE PROCEDURE dbo.p_orders AS BEGIN SELECT id, total FROM orders; END",
		// Reads a table the policy keeps back.
		"CREATE PROCEDURE dbo.p_payroll AS BEGIN SELECT id, amount FROM payroll; END",
		// Reads a field the policy hides.
		"CREATE PROCEDURE dbo.p_ssn AS BEGIN SELECT id, ssn FROM customers; END",
		// Does its real work somewhere this side cannot read.
		"CREATE PROCEDURE dbo.p_dynamic AS BEGIN EXEC('SELECT ssn FROM customers'); END",
		// Calls another routine, so its own tables are not the whole story.
		"CREATE PROCEDURE dbo.p_nested AS BEGIN EXEC dbo.p_payroll; END",
		// The other way in. A function is called from a select list, so the
		// statement is a SELECT and nothing about its leading word says a stored
		// body is about to run.
		"CREATE FUNCTION dbo.f_clean() RETURNS int AS BEGIN RETURN (SELECT TOP 1 id FROM orders); END",
		"CREATE FUNCTION dbo.f_ssn() RETURNS nvarchar(20) AS BEGIN RETURN (SELECT TOP 1 ssn FROM customers); END",
	} {
		if _, err := conn.Exec(context.Background(), stmt, nil); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(stmt, " AS ", 2)[0], err)
		}
	}

	return Settings{
		Connection: cfg,
		Access:     AccessBoth,
		Allowed:    map[string]bool{datasource.CapCallRoutines: true},
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "payroll",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		},
	}
}

func TestARoutineIsHeldToThePolicyByItsBody(t *testing.T) {
	governed := routineFixture(t)

	// The one that should run, and does. Without this the refusals below could
	// not be told from a feature that never works.
	if res, reason, err := run(context.Background(), governed, "EXEC dbo.p_orders", nil, 0); err != nil || reason != "" {
		t.Fatalf("a clean routine was refused: reason=%q err=%v", reason, err)
	} else if res == nil {
		t.Fatal("it ran but returned nothing")
	}

	// And the same, reached as a function rather than as a statement.
	if res, reason, err := run(context.Background(), governed, "SELECT dbo.f_clean() AS id", nil, 0); err != nil || reason != "" {
		t.Fatalf("a clean function was refused: reason=%q err=%v", reason, err)
	} else if res == nil || res.RowCount == 0 {
		t.Fatal("the clean function ran but returned nothing")
	}

	for _, c := range []struct{ sql, because string }{
		{"EXEC dbo.p_payroll", "it reads a table the policy keeps back"},
		{"EXEC dbo.p_ssn", "it reads a field the policy hides, and a body cannot be rewritten"},
		{"EXEC dbo.p_dynamic", "it builds its statement out of a string"},
		{"EXEC dbo.p_nested", "it runs another routine, so its own tables are not the whole story"},
		{"EXEC dbo.does_not_exist", "the catalogue has never heard of it"},
		{"EXEC('SELECT ssn FROM customers')", "the statement itself is a string"},
		{"SELECT dbo.f_ssn() AS ssn", "a function in a select list reaches the same code"},
		{"SELECT id FROM orders WHERE note > dbo.f_ssn()", "and so does one in a WHERE"},
	} {
		_, reason, err := run(context.Background(), governed, c.sql, nil, 0)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.sql, err)
			continue
		}
		if reason == "" {
			t.Errorf("%s RAN, and should not have: %s", c.sql, c.because)
			continue
		}
		t.Logf("  refused %-38s (%s)", c.sql, c.because)
	}
}

// The control for the whole suite: ungoverned, these routines really do hand
// over what the policy exists to keep back. A refusal above is only worth
// something if the thing refused was going to happen.
func TestUngovernedTheyReallyDoLeak(t *testing.T) {
	open := routineFixture(t)
	open.Policy = sqlguard.Policy{}

	for _, c := range []struct{ sql, want string }{
		{"EXEC dbo.p_payroll", "1000"},
		{"EXEC dbo.p_ssn", msRealSSN},
		{"SELECT dbo.f_ssn() AS ssn", msRealSSN},
	} {
		res, reason, err := run(context.Background(), open, c.sql, nil, 0)
		if err != nil || reason != "" {
			t.Fatalf("the control could not run %s: reason=%q err=%v", c.sql, reason, err)
		}
		if !strings.Contains(fmt.Sprint(res.Rows), c.want) {
			t.Errorf("the control failed: %s returned no %s even ungoverned: %+v", c.sql, c.want, res.Rows)
		}
	}
}

// And the capability itself: with the box unticked, none of it runs, whatever
// the body says.
func TestWithTheBoxUntickedNothingRuns(t *testing.T) {
	off := routineFixture(t)
	off.Allowed = map[string]bool{}
	for _, sql := range []string{"EXEC dbo.p_orders", "EXEC dbo.p_payroll", "SELECT dbo.f_clean() AS id"} {
		_, reason, err := run(context.Background(), off, sql, nil, 0)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
		}
		if reason == "" {
			t.Errorf("%s ran with the capability off", sql)
		}
	}
}

// Making stored code, against a real server. The gate for it is text rather than
// a parse, because it has to work on a tool with no policy, which is where there
// is no parse at all. What cannot be checked from here is whether the statement
// the gate lets through is one the ENGINE accepts.
func TestMakingATriggerNeedsItsOwnBox(t *testing.T) {
	s := Settings{
		Connection: msFixture(t),
		Access:     AccessBoth,
		Allowed:    map[string]bool{},
	}
	const create = "CREATE TRIGGER r_trg ON orders AFTER INSERT AS BEGIN SET NOCOUNT ON END"

	if _, reason, err := run(context.Background(), s, create, nil, 0); reason == "" {
		t.Fatalf("the trigger was made with nothing ticked (err=%v)", err)
	}
	// Ticking the other box is not ticking this one.
	s.Allowed = map[string]bool{datasource.CapCreateRoutines: true}
	if _, reason, _ := run(context.Background(), s, create, nil, 0); reason == "" {
		t.Fatal("the routines box made a trigger")
	}

	s.Allowed = map[string]bool{datasource.CapCreateTriggers: true}
	if _, reason, err := run(context.Background(), s, create, nil, 0); reason != "" || err != nil {
		t.Fatalf("with the box ticked the trigger was not made: reason=%q err=%v", reason, err)
	}
	if _, reason, err := run(context.Background(), s, "DROP TRIGGER r_trg", nil, 0); reason != "" || err != nil {
		t.Fatalf("dropping it: reason=%q err=%v", reason, err)
	}
}
