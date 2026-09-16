package postgres

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
	c := sqlguard.NewCatalog("postgres", "shop", []sqlguard.Table{
		{Name: "customers", Namespace: "public", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Namespace: "public", Columns: []string{"id", "customer_id", "total"}},
		{Name: "secret_keys", Namespace: "public", Columns: []string{"id", "value"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Namespace: "public", Name: "r_clean", Body: "SELECT id, total FROM orders"},
		{Namespace: "public", Name: "r_denied", Body: "SELECT id, value FROM secret_keys"},
		{Namespace: "public", Name: "r_hidden", Body: "SELECT id, ssn FROM customers"},
		{Namespace: "public", Name: "r_nested", Body: "CALL r_clean()"},
		{Namespace: "public", Name: "r_nested_bad", Body: "CALL r_denied()"},
		{Namespace: "public", Name: "r_nested_deep", Body: "CALL r_nested_bad()"},
		{Namespace: "public", Name: "r_by_func", Body: "SELECT r_denied()"},
		{Namespace: "public", Name: "r_opaque", Body: "this is not sql"},
		{Namespace: "public", Name: "r_silent", Body: ""},
	})
	var opts []sqlguard.Option
	if ticked {
		opts = append(opts, sqlguard.MayCallRoutines())
	}
	g, err := sqlguard.New("postgres", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, c, opts...)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

func TestACleanRoutineRuns(t *testing.T) {
	g := withRoutines(t, true)
	allowed(t, g, "CALL r_clean()")
	allowed(t, g, "SELECT r_clean()")
	allowed(t, g, "SELECT id FROM orders WHERE total > r_clean()")
	// A routine built out of another routine: the chain is followed to the end
	// and every link is held to the policy, rather than the first link being
	// refused for doing its work somewhere this never looked.
	allowed(t, g, "CALL r_nested()")
	// A built-in is not a routine, and refusing one would refuse everything.
	allowed(t, g, "SELECT lower(email) FROM customers")
	// And standard SQL whose spelling this parser rewrites into a pg_catalog
	// name nobody wrote. Every one of these was refused as unknown stored code.
	allowed(t, g, "SELECT SUBSTRING(email FROM 1 FOR 3) FROM customers")
	allowed(t, g, "SELECT EXTRACT(year FROM now())")
	allowed(t, g, "SELECT TRIM(both ' ' FROM email) FROM customers")
	allowed(t, g, "SELECT POSITION('@' IN email) FROM customers")
	allowed(t, g, "SELECT OVERLAY(email PLACING 'x' FROM 1) FROM customers")
	allowed(t, g, "SELECT NORMALIZE(email) FROM customers")
}

func TestARoutineIsHeldToThePolicyByItsBody(t *testing.T) {
	g := withRoutines(t, true)
	for _, c := range []struct{ sql, because string }{
		{"CALL r_denied()", "it reads a table the policy keeps back"},
		{"CALL r_hidden()", "it reads a field the policy hides, and a body cannot be rewritten"},
		{"CALL r_nested_bad()", "the routine it runs reads a table the policy keeps back"},
		{"CALL r_nested_deep()", "and that holds two links down"},
		// A pg_catalog name somebody writes out in full keeps the ordinary rule.
		{"SELECT pg_catalog.substring(email, 1, 3) FROM customers", "a qualified name is stored code"},
		{"CALL r_by_func()", "it calls a routine that reads a denied table"},
		{"CALL r_opaque()", "its body cannot be read"},
		{"CALL r_silent()", "this account cannot see its body"},
		{"CALL r_missing()", "the catalog has never heard of it"},
		{"SELECT r_denied()", "a select list reaches the same code"},
		{"SELECT id FROM orders WHERE total > r_hidden()", "so does a WHERE"},
		{"SELECT r_by_func()", "and so does a routine that calls one"},
		{"SELECT * FROM r_denied()", "and so does a FROM"},
	} {
		reason := refused(t, g, c.sql)
		t.Logf("  refused %-46s (%s)", c.sql, c.because)
		if reason == "" {
			t.Errorf("%s: refused with no reason", c.sql)
		}
	}
}

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
	allowed(t, g, "SELECT lower(email) FROM customers")
}
