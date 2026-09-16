package postgres

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
func chained(t *testing.T) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("postgres", "public", []sqlguard.Table{
		{Namespace: "public", Name: "orders", Columns: []string{"id", "total", "note"}},
		{Namespace: "public", Name: "customers", Columns: []string{"id", "email", "ssn"}},
		{Namespace: "public", Name: "secret_keys", Columns: []string{"id", "value"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		// A clean chain, three deep.
		{Namespace: "public", Name: "c_top", Body: "SELECT c_mid()"},
		{Namespace: "public", Name: "c_mid", Body: "SELECT c_leaf()"},
		{Namespace: "public", Name: "c_leaf", Body: "SELECT total FROM orders LIMIT 1"},
		// The same shape, with the last link reading a denied table.
		{Namespace: "public", Name: "d_top", Body: "SELECT d_mid()"},
		{Namespace: "public", Name: "d_mid", Body: "SELECT d_leaf()"},
		{Namespace: "public", Name: "d_leaf", Body: "SELECT value FROM secret_keys LIMIT 1"},
		// And with the last link returning a hidden field.
		{Namespace: "public", Name: "h_top", Body: "SELECT h_leaf()"},
		{Namespace: "public", Name: "h_leaf", Body: "SELECT ssn FROM customers LIMIT 1"},
		// A link nobody can read.
		{Namespace: "public", Name: "u_top", Body: "SELECT u_leaf()"},
		{Namespace: "public", Name: "u_leaf", Body: "this is not sql"},
		// A link in a schema this snapshot never read.
		{Namespace: "public", Name: "g_top", Body: "SELECT hidden_schema.f_out()"},
		// And one calling a bare name nobody has heard of, which is a built-in.
		{Namespace: "public", Name: "b_top", Body: "SELECT lower('AB')"},
		// A diamond: two branches that both end at the same clean routine.
		{Namespace: "public", Name: "x_top", Body: "SELECT x_left() + x_right()"},
		{Namespace: "public", Name: "x_left", Body: "SELECT x_shared()"},
		{Namespace: "public", Name: "x_right", Body: "SELECT x_shared()"},
		{Namespace: "public", Name: "x_shared", Body: "SELECT total FROM orders LIMIT 1"},
		// A loop.
		{Namespace: "public", Name: "l_a", Body: "SELECT l_b()"},
		{Namespace: "public", Name: "l_b", Body: "SELECT l_a()"},
		// One that calls itself.
		{Namespace: "public", Name: "self", Body: "SELECT self()"},
	})
	g, err := sqlguard.New("postgres", sqlguard.Policy{
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
		// A bare name the snapshot never saw is a built-in, not stored code.
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
		if !strings.Contains(reason, c.mustName) {
			t.Errorf("refused without naming the link.\n  %s\n  said: %s\n  wanted it to name: %s",
				c.sql, reason, c.mustName)
		}
	}
}

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

// Making a routine out of routines: the same walk, at the moment it is created.
func TestCreatingARoutineOutOfRoutinesFollowsTheChain(t *testing.T) {
	g := chained(t)
	for _, c := range []struct {
		sql     string
		allowed bool
		why     string
	}{
		{"CREATE FUNCTION f_new() RETURNS int LANGUAGE sql AS $$ SELECT c_top() $$", true,
			"every link of c_top is clean"},
		{"CREATE FUNCTION f_bad() RETURNS int LANGUAGE sql AS $$ SELECT d_top() $$", false,
			"d_top ends at a table the policy keeps back"},
		{"CREATE FUNCTION f_hid() RETURNS int LANGUAGE sql AS $$ SELECT h_top() $$", false,
			"h_top ends at a field the policy hides"},
		{"CREATE FUNCTION f_low() RETURNS text LANGUAGE sql AS $$ SELECT lower(note) FROM orders $$", true,
			"lower is a built-in, not stored code"},
		// A procedure written out of the procedures already in the database.
		{"CREATE PROCEDURE p_new() LANGUAGE sql AS $$ CALL c_top() $$", true,
			"every link of c_top is clean"},
		{"CREATE PROCEDURE p_bad() LANGUAGE sql AS $$ CALL d_top() $$", false,
			"d_top ends at a table the policy keeps back"},
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
	c := sqlguard.NewCatalog("postgres", "public", []sqlguard.Table{
		{Namespace: "public", Name: "orders", Columns: []string{"id", "total"}},
		{Namespace: "public", Name: "secret_keys", Columns: []string{"id", "value"}},
		{
			Namespace: "public", Name: "clean_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "SELECT o.id AS id, v_clean() AS code FROM orders o",
		},
		{
			Namespace: "public", Name: "leak_view", View: true,
			Columns:    []string{"id", "code"},
			Definition: "SELECT o.id AS id, v_leak() AS code FROM orders o",
		},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Namespace: "public", Name: "v_clean", Body: "SELECT v_clean_leaf()"},
		{Namespace: "public", Name: "v_clean_leaf", Body: "SELECT total FROM orders LIMIT 1"},
		{Namespace: "public", Name: "v_leak", Body: "SELECT v_leak_leaf()"},
		{Namespace: "public", Name: "v_leak_leaf", Body: "SELECT value FROM secret_keys LIMIT 1"},
	})
	g, err := sqlguard.New("postgres", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
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
