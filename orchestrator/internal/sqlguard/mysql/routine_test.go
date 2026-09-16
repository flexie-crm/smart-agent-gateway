package mysql

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// Running the database's own stored code.
//
// Two rules hold, and each test below asserts both directions of one of them.
// A routine runs only when an administrator ticked the box for it, and only when
// READING ITS BODY shows the body stays inside the policy.
//
// The verb is not the question. CALL announces itself and `SELECT f()` does not,
// and both reach the same code: a stored function in a select list was measured
// handing over a hidden column before this existed.

func withRoutines(t *testing.T, ticked bool) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "customers", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Columns: []string{"id", "customer_id", "total"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Namespace: "", Name: "r_clean", Body: "SELECT id, total FROM orders"},
		{Namespace: "", Name: "r_denied", Body: "SELECT id, value FROM secret_keys"},
		{Namespace: "", Name: "r_hidden", Body: "SELECT id, ssn FROM customers"},
		{Namespace: "", Name: "r_nested", Body: "CALL r_clean()"},
		{Namespace: "", Name: "r_by_func", Body: "SELECT r_denied()"},
		{Namespace: "", Name: "r_opaque", Body: "this is not sql"},
		{Namespace: "", Name: "r_silent", Body: ""},
	})
	var opts []sqlguard.Option
	if ticked {
		opts = append(opts, sqlguard.MayCallRoutines())
	}
	g, err := sqlguard.New("mysql", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, c, opts...)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

// With the box ticked, a routine that stays inside the policy runs, whichever
// way it is called. Without this the refusals below could not be told from a
// feature that never works.
func TestACleanRoutineRuns(t *testing.T) {
	g := withRoutines(t, true)
	allowed(t, g, "CALL r_clean()")
	allowed(t, g, "SELECT r_clean()")
	allowed(t, g, "SELECT id FROM orders WHERE total > r_clean()")
	// A built-in is not a routine, and refusing one would refuse everything.
	allowed(t, g, "SELECT LOWER(email) FROM customers")
}

func TestARoutineIsHeldToThePolicyByItsBody(t *testing.T) {
	g := withRoutines(t, true)
	for _, c := range []struct{ sql, because string }{
		{"CALL r_denied()", "it reads a table the policy keeps back"},
		{"CALL r_hidden()", "it reads a field the policy hides, and a body cannot be rewritten"},
		{"CALL r_nested()", "it runs another routine, so its own tables are not the whole story"},
		{"CALL r_by_func()", "it calls a routine that reads a denied table"},
		{"CALL r_opaque()", "its body cannot be read"},
		{"CALL r_silent()", "this account cannot see its body"},
		{"CALL r_missing()", "the catalog has never heard of it"},
		// The same bodies, reached as a function instead of a statement.
		{"SELECT r_denied()", "a select list reaches the same code"},
		{"SELECT id FROM orders WHERE total > r_hidden()", "so does a WHERE"},
		{"SELECT r_by_func()", "and so does a routine that calls one"},
	} {
		reason := refused(t, g, c.sql)
		t.Logf("  refused %-46s (%s)", c.sql, c.because)
		if reason == "" {
			t.Errorf("%s: refused with no reason", c.sql)
		}
	}
}

// The control for all of it: with the box unticked nothing runs, however clean
// the body is, and the reason says so rather than blaming the policy.
func TestWithTheBoxUntickedNoRoutineRuns(t *testing.T) {
	g := withRoutines(t, false)
	for _, sql := range []string{
		"CALL r_clean()",
		"SELECT r_clean()",
		"SELECT id FROM orders WHERE total > r_clean()",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "not permitted to run stored routines") {
			t.Errorf("%s: refused for the wrong reason: %s", sql, reason)
		}
	}
	// And a statement with no routine in it is untouched by the capability.
	allowed(t, g, "SELECT LOWER(email) FROM customers")
}
