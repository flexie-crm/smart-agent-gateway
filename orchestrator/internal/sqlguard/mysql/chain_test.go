package mysql

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
// The bodies here are FUNCTIONS. On this engine a procedure body containing a
// CALL statement does not parse at all (the parser this package carries has no
// rule for CALL inside a body), so a chain between procedures is refused before
// any of this is reached. A stored function called from an expression is a
// different node, is read, and is what the chain follows here. See
// TestAProcedureCallingAProcedureIsStillUnreadable for the other half stated
// out loud.
func chained(t *testing.T) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "orders", Columns: []string{"id", "total", "note"}},
		{Name: "customers", Columns: []string{"id", "email", "ssn"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		// A clean chain, three deep.
		{Name: "c_top", Body: "RETURN c_mid()"},
		{Name: "c_mid", Body: "RETURN c_leaf()"},
		{Name: "c_leaf", Body: "RETURN (SELECT total FROM orders LIMIT 1)"},
		// The same shape, with the last link reading a denied table.
		{Name: "d_top", Body: "RETURN d_mid()"},
		{Name: "d_mid", Body: "RETURN d_leaf()"},
		{Name: "d_leaf", Body: "RETURN (SELECT value FROM secret_keys LIMIT 1)"},
		// And with the last link returning a hidden field.
		{Name: "h_top", Body: "RETURN h_leaf()"},
		{Name: "h_leaf", Body: "RETURN (SELECT ssn FROM customers LIMIT 1)"},
		// A link nobody can read.
		{Name: "u_top", Body: "RETURN u_leaf()"},
		{Name: "u_leaf", Body: "this is not sql"},
		// A link in a schema this snapshot never read: stored code, whatever the
		// catalogue knows, because that is exactly where a routine hides.
		{Name: "g_top", Body: "RETURN hidden_schema.f_out()"},
		// And one calling a bare name nobody has heard of, which is a built-in.
		{Name: "b_top", Body: "RETURN lower(concat('a', 'b'))"},
		// A diamond: two branches that both end at the same clean routine.
		{Name: "x_top", Body: "RETURN x_left() + x_right()"},
		{Name: "x_left", Body: "RETURN x_shared()"},
		{Name: "x_right", Body: "RETURN x_shared()"},
		{Name: "x_shared", Body: "RETURN (SELECT total FROM orders LIMIT 1)"},
		// A loop.
		{Name: "l_a", Body: "RETURN l_b()"},
		{Name: "l_b", Body: "RETURN l_a()"},
		// One that calls itself.
		{Name: "self", Body: "RETURN self()"},
	})
	g, err := sqlguard.New("mysql", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
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
		"SELECT c_top()", "SELECT c_mid()", "SELECT c_leaf()",
		// A bare name the snapshot never saw is a built-in, not stored code: the
		// same reading the top level gives it. Refusing these would refuse every
		// body worth having.
		"SELECT b_top()", "SELECT lower(note) FROM orders",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}
}

// Two branches meeting at one routine is not a loop. The walk has to tell the
// difference, or a utility function called from two places refuses everything
// above it.
func TestARoutineReachedTwiceIsNotALoop(t *testing.T) {
	g := chained(t)
	if _, reason, err := g.Check("SELECT x_top()"); err != nil || reason != "" {
		t.Errorf("REFUSED a diamond, and should not have: %v %s", err, reason)
	}
}

func TestAChainIsRefusedAtTheLinkThatBreaksIt(t *testing.T) {
	g := chained(t)
	for _, c := range []struct{ sql, mustName, because string }{
		{"SELECT d_top()", "d_leaf", "the third link reads a denied table"},
		{"SELECT h_top()", "h_leaf", "the second link returns a hidden field"},
		{"SELECT u_top()", "u_leaf", "a link cannot be read"},
		{"SELECT g_top()", "hidden_schema.f_out", "a link is in a schema nobody read"},
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
		// The refusal has to name the link that failed, or somebody reading it
		// has to guess which of three routines is the problem.
		if !strings.Contains(reason, c.mustName) {
			t.Errorf("refused without naming the link.\n  %s\n  said: %s\n  wanted it to name: %s",
				c.sql, reason, c.mustName)
		}
	}
}

// Two routines that call each other, and one that calls itself, end the walk
// rather than running forever.
func TestALoopOfRoutinesEndsTheWalk(t *testing.T) {
	g := chained(t)
	for _, sql := range []string{"SELECT l_a()", "SELECT l_b()", "SELECT self()"} {
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

// The limit on this engine, stated rather than left to be discovered.
//
// A procedure that CALLs another procedure cannot be read here at all: the
// parser has no rule for CALL inside a body, so the body fails to parse and the
// routine is refused as unreadable, one link earlier than the chain walk. The
// chain follows stored FUNCTIONS, which is what an expression calls.
func TestAProcedureCallingAProcedureIsStillUnreadable(t *testing.T) {
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "orders", Columns: []string{"id", "total"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Name: "p_top", Body: "BEGIN SELECT id FROM orders; CALL p_leaf(); END"},
		{Name: "p_leaf", Body: "BEGIN SELECT total FROM orders; END"},
		// The control: the same procedure without the CALL runs.
		{Name: "p_plain", Body: "BEGIN SELECT id FROM orders; END"},
	})
	g, err := sqlguard.New("mysql", sqlguard.Policy{}, c, sqlguard.MayCallRoutines())
	if err != nil {
		t.Fatal(err)
	}
	if _, reason, _ := g.Check("CALL p_plain()"); reason != "" {
		t.Fatalf("the control was refused, so the case below proves nothing: %s", reason)
	}
	if _, reason, _ := g.Check("CALL p_top()"); reason == "" {
		t.Error("a procedure calling a procedure was allowed: the parser has learned CALL, " +
			"and this test and the comment above it are now wrong")
	} else {
		t.Logf("as expected on this engine: %s", reason)
	}
}

// Making a routine out of routines.
//
// The same walk decides a body that is being CREATED, so a procedure built on
// the ones already in the database can be written, and one built on a routine
// that reads somewhere it may not still cannot.
func TestCreatingARoutineOutOfRoutinesFollowsTheChain(t *testing.T) {
	g := chained(t)
	for _, c := range []struct {
		sql     string
		allowed bool
		why     string
	}{
		{"CREATE PROCEDURE p_new() BEGIN SELECT c_top(); END", true,
			"every link of c_top is clean"},
		{"CREATE PROCEDURE p_bad() BEGIN SELECT d_top(); END", false,
			"d_top ends at a table the policy keeps back"},
		{"CREATE PROCEDURE p_hid() BEGIN SELECT h_top(); END", false,
			"h_top ends at a field the policy hides"},
		{"CREATE PROCEDURE p_low() BEGIN SELECT lower(note) FROM orders; END", true,
			"lower is a built-in, not stored code"},
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

// A view over a function built out of other functions.
//
// This one is decided when the snapshot is taken rather than when the statement
// runs: what the chain reads is folded into the view's own reads, so a rule
// about a table two functions down still holds, and a view whose chain cannot be
// read stays refused.
func TestAViewOverAChainOfFunctions(t *testing.T) {
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "orders", Columns: []string{"id", "total"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
		{
			Name: "clean_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "SELECT o.id AS id, v_clean() AS code FROM orders o",
		},
		{
			Name: "leak_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "SELECT o.id AS id, v_leak() AS code FROM orders o",
		},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Name: "v_clean", Body: "RETURN v_clean_leaf()"},
		{Name: "v_clean_leaf", Body: "RETURN (SELECT total FROM orders LIMIT 1)"},
		{Name: "v_leak", Body: "RETURN v_leak_leaf()"},
		{Name: "v_leak_leaf", Body: "RETURN (SELECT value FROM secret_keys LIMIT 1)"},
	})
	g, err := sqlguard.New("mysql", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
	}, c, sqlguard.MayCallRoutines())
	if err != nil {
		t.Fatal(err)
	}
	// The control: a view whose whole chain is clean is readable. Without it the
	// refusal below would only prove that chained views never work.
	if _, reason, _ := g.Check("SELECT id, code FROM clean_view"); reason != "" {
		t.Errorf("REFUSED a clean chained view: %s", reason)
	}
	if _, reason, _ := g.Check("SELECT id, code FROM leak_view"); reason == "" {
		t.Error("a view reaching a denied table two functions down was read")
	}
}
