package sqlserver

import (
	"strings"
	"testing"
	"time"

	"flexie.io/sag/internal/sqlguard"
)

// A routine built out of other routines.
//
// These used to be refused at the first link, on the reasoning that the work
// happened somewhere this never saw. It did not: every body is in the same
// snapshot, so the chain is followed to the end and each link is held to the
// policy like the first.
//
// Both shapes are here, because this engine has two: EXEC of a procedure, which
// announces itself, and a stored function in an expression, which does not.
func chained(t *testing.T) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("sqlserver", "shop", []sqlguard.Table{
		{Namespace: "dbo", Name: "orders", Columns: []string{"id", "total", "note"}},
		{Namespace: "dbo", Name: "customers", Columns: []string{"id", "email", "ssn"}},
		{Namespace: "dbo", Name: "payroll", Columns: []string{"id", "amount"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		// A clean chain of procedures, three deep.
		{Namespace: "dbo", Name: "c_top", Body: "CREATE PROCEDURE dbo.c_top AS BEGIN EXEC dbo.c_mid; END"},
		{Namespace: "dbo", Name: "c_mid", Body: "CREATE PROCEDURE dbo.c_mid AS BEGIN EXEC dbo.c_leaf; END"},
		{Namespace: "dbo", Name: "c_leaf", Body: "CREATE PROCEDURE dbo.c_leaf AS BEGIN SELECT total FROM orders; END"},
		// The same shape, with the last link reading a denied table.
		{Namespace: "dbo", Name: "d_top", Body: "CREATE PROCEDURE dbo.d_top AS BEGIN EXEC dbo.d_mid; END"},
		{Namespace: "dbo", Name: "d_mid", Body: "CREATE PROCEDURE dbo.d_mid AS BEGIN EXEC dbo.d_leaf; END"},
		{Namespace: "dbo", Name: "d_leaf", Body: "CREATE PROCEDURE dbo.d_leaf AS BEGIN SELECT amount FROM payroll; END"},
		// And with the last link returning a hidden field.
		{Namespace: "dbo", Name: "h_top", Body: "CREATE PROCEDURE dbo.h_top AS BEGIN EXEC dbo.h_leaf; END"},
		{Namespace: "dbo", Name: "h_leaf", Body: "CREATE PROCEDURE dbo.h_leaf AS BEGIN SELECT ssn FROM customers; END"},
		// A chain of functions rather than procedures.
		{Namespace: "dbo", Name: "f_top", Body: "CREATE FUNCTION dbo.f_top() RETURNS int AS BEGIN RETURN dbo.f_leaf(); END"},
		{Namespace: "dbo", Name: "f_leaf", Body: "CREATE FUNCTION dbo.f_leaf() RETURNS int AS BEGIN RETURN (SELECT TOP 1 total FROM orders); END"},
		{Namespace: "dbo", Name: "fd_top", Body: "CREATE FUNCTION dbo.fd_top() RETURNS int AS BEGIN RETURN dbo.fd_leaf(); END"},
		{Namespace: "dbo", Name: "fd_leaf", Body: "CREATE FUNCTION dbo.fd_leaf() RETURNS int AS BEGIN RETURN (SELECT TOP 1 amount FROM payroll); END"},
		// A link nobody can read.
		{Namespace: "dbo", Name: "u_top", Body: "CREATE PROCEDURE dbo.u_top AS BEGIN EXEC dbo.u_leaf; END"},
		{Namespace: "dbo", Name: "u_leaf", Body: "this is not sql"},
		// A link whose body this account may not see.
		{Namespace: "dbo", Name: "s_top", Body: "CREATE PROCEDURE dbo.s_top AS BEGIN EXEC dbo.s_leaf; END"},
		{Namespace: "dbo", Name: "s_leaf", Body: ""},
		// A link in a schema this snapshot never read.
		{Namespace: "dbo", Name: "g_top", Body: "CREATE PROCEDURE dbo.g_top AS BEGIN EXEC hidden.f_out; END"},
		// A diamond: two branches that both end at the same clean routine.
		{Namespace: "dbo", Name: "x_top", Body: "CREATE PROCEDURE dbo.x_top AS BEGIN EXEC dbo.x_left; EXEC dbo.x_right; END"},
		{Namespace: "dbo", Name: "x_left", Body: "CREATE PROCEDURE dbo.x_left AS BEGIN EXEC dbo.x_shared; END"},
		{Namespace: "dbo", Name: "x_right", Body: "CREATE PROCEDURE dbo.x_right AS BEGIN EXEC dbo.x_shared; END"},
		{Namespace: "dbo", Name: "x_shared", Body: "CREATE PROCEDURE dbo.x_shared AS BEGIN SELECT total FROM orders; END"},
		// A loop.
		{Namespace: "dbo", Name: "l_a", Body: "CREATE PROCEDURE dbo.l_a AS BEGIN EXEC dbo.l_b; END"},
		{Namespace: "dbo", Name: "l_b", Body: "CREATE PROCEDURE dbo.l_b AS BEGIN EXEC dbo.l_a; END"},
		// One that calls itself.
		{Namespace: "dbo", Name: "self", Body: "CREATE PROCEDURE dbo.self AS BEGIN EXEC dbo.self; END"},
	})
	g, err := sqlguard.New("sqlserver", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, c, sqlguard.MayCallRoutines())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAChainOfCleanRoutinesRuns(t *testing.T) {
	g := chained(t)
	// Three deep, every link clean. Without this the refusals below would only
	// prove that chains never work.
	for _, sql := range []string{
		"EXEC dbo.c_top", "EXEC dbo.c_mid", "EXEC dbo.c_leaf",
		"SELECT dbo.f_top()", "SELECT dbo.f_leaf()",
		// A built-in is not stored code, and refusing one would refuse everything.
		"SELECT LOWER(note) FROM orders",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}
}

// Two branches meeting at one routine is not a loop. The walk has to tell the
// difference, or a utility procedure called from two places refuses everything
// above it.
func TestARoutineReachedTwiceIsNotALoop(t *testing.T) {
	g := chained(t)
	if _, reason, err := g.Check("EXEC dbo.x_top"); err != nil || reason != "" {
		t.Errorf("REFUSED a diamond, and should not have: %v %s", err, reason)
	}
}

func TestAChainIsRefusedAtTheLinkThatBreaksIt(t *testing.T) {
	g := chained(t)
	for _, c := range []struct{ sql, mustName, because string }{
		{"EXEC dbo.d_top", "d_leaf", "the third link reads a denied table"},
		{"EXEC dbo.h_top", "h_leaf", "the second link returns a hidden field"},
		{"EXEC dbo.u_top", "u_leaf", "a link cannot be read"},
		{"EXEC dbo.s_top", "s_leaf", "a link's body this account may not see"},
		{"EXEC dbo.g_top", "hidden.f_out", "a link is in a schema nobody read"},
		{"SELECT dbo.fd_top()", "fd_leaf", "and the same through a chain of functions"},
	} {
		_, reason, err := g.Check(c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if reason == "" {
			t.Errorf("RAN, and should not have: %s (%s)", c.sql, c.because)
			continue
		}
		if !strings.Contains(reason, c.mustName) {
			t.Errorf("refused without naming the link.\n  %s\n  said: %s\n  wanted it to name: %s",
				c.sql, reason, c.mustName)
		}
	}
}

func TestALoopOfRoutinesEndsTheWalk(t *testing.T) {
	g := chained(t)
	for _, sql := range []string{"EXEC dbo.l_a", "EXEC dbo.l_b", "EXEC dbo.self"} {
		done := make(chan string, 1)
		go func() {
			_, reason, _ := g.Check(sql)
			done <- reason
		}()
		select {
		case reason := <-done:
			if reason == "" {
				t.Errorf("%s was allowed, and it is a loop", sql)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not finish: the walk is not stopping at a loop", sql)
		}
	}
}

// Making a routine out of routines: the same walk, at the moment it is created.
func TestCreatingARoutineOutOfRoutinesFollowsTheChain(t *testing.T) {
	g := chained(t)
	for _, c := range []struct {
		sql     string
		allowed bool
		why     string
	}{
		{"CREATE PROCEDURE dbo.p_new AS BEGIN EXEC dbo.c_top; END", true,
			"every link of c_top is clean"},
		{"CREATE PROCEDURE dbo.p_bad AS BEGIN EXEC dbo.d_top; END", false,
			"d_top ends at a table the policy keeps back"},
		{"CREATE PROCEDURE dbo.p_hid AS BEGIN EXEC dbo.h_top; END", false,
			"h_top ends at a field the policy hides"},
		{"CREATE PROCEDURE dbo.p_low AS BEGIN SELECT LOWER(note) FROM orders; END", true,
			"LOWER is a built-in, not stored code"},
	} {
		_, reason, err := g.Check(c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if c.allowed && reason != "" {
			t.Errorf("REFUSED, and should not be (%s):\n  %s\n  said: %s", c.why, c.sql, reason)
		}
		if !c.allowed && reason == "" {
			t.Errorf("ALLOWED, and should not be (%s):\n  %s", c.why, c.sql)
		}
	}
}

// A view over a function built out of other functions. This one is decided when
// the snapshot is taken rather than when the statement runs: what the chain
// reads is folded into the view's own reads.
func TestAViewOverAChainOfFunctions(t *testing.T) {
	c := sqlguard.NewCatalog("sqlserver", "shop", []sqlguard.Table{
		{Namespace: "dbo", Name: "orders", Columns: []string{"id", "total"}},
		{Namespace: "dbo", Name: "payroll", Columns: []string{"id", "amount"}},
		{
			Namespace: "dbo", Name: "clean_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "CREATE VIEW dbo.clean_view AS SELECT o.id AS id, dbo.v_clean() AS code FROM orders o",
		},
		{
			Namespace: "dbo", Name: "leak_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "CREATE VIEW dbo.leak_view AS SELECT o.id AS id, dbo.v_leak() AS code FROM orders o",
		},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Namespace: "dbo", Name: "v_clean", Body: "CREATE FUNCTION dbo.v_clean() RETURNS int AS BEGIN RETURN dbo.v_clean_leaf(); END"},
		{Namespace: "dbo", Name: "v_clean_leaf", Body: "CREATE FUNCTION dbo.v_clean_leaf() RETURNS int AS BEGIN RETURN (SELECT TOP 1 total FROM orders); END"},
		{Namespace: "dbo", Name: "v_leak", Body: "CREATE FUNCTION dbo.v_leak() RETURNS int AS BEGIN RETURN dbo.v_leak_leaf(); END"},
		{Namespace: "dbo", Name: "v_leak_leaf", Body: "CREATE FUNCTION dbo.v_leak_leaf() RETURNS int AS BEGIN RETURN (SELECT TOP 1 amount FROM payroll); END"},
	})
	g, err := sqlguard.New("sqlserver", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
	}, c, sqlguard.MayCallRoutines())
	if err != nil {
		t.Fatal(err)
	}
	// The control: a view whose whole chain is clean is readable.
	if _, reason, _ := g.Check("SELECT id, code FROM clean_view"); reason != "" {
		t.Errorf("REFUSED a clean chained view: %s", reason)
	}
	if _, reason, _ := g.Check("SELECT id, code FROM leak_view"); reason == "" {
		t.Error("a view reaching a denied table two functions down was read")
	}
}
