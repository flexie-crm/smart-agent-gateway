package sqlserver

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
// The verb is not the question. EXEC announces itself and `SELECT dbo.f()` does
// not, and both reach the same code: a stored function in a select list was
// measured handing over a hidden column before this existed. On this engine one
// grammar rule covers both, which is also how a table-valued function in a FROM
// is caught.

func withRoutines(t *testing.T, ticked bool) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("sqlserver", "shop", []sqlguard.Table{
		{Name: "customers", Namespace: "dbo", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Namespace: "dbo", Columns: []string{"id", "customer_id", "total"}},
		{Name: "payroll", Namespace: "dbo", Columns: []string{"id", "emp", "amount"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Namespace: "dbo", Name: "r_clean", Body: "CREATE PROCEDURE dbo.r_clean AS BEGIN SELECT id, total FROM orders; END"},
		{Namespace: "dbo", Name: "r_denied", Body: "CREATE PROCEDURE dbo.r_denied AS BEGIN SELECT id, amount FROM payroll; END"},
		{Namespace: "dbo", Name: "r_hidden", Body: "CREATE PROCEDURE dbo.r_hidden AS BEGIN SELECT id, ssn FROM customers; END"},
		{Namespace: "dbo", Name: "r_nested", Body: "CREATE PROCEDURE dbo.r_nested AS BEGIN EXEC dbo.r_clean; END"},
		{Namespace: "dbo", Name: "r_nested_bad", Body: "CREATE PROCEDURE dbo.r_nested_bad AS BEGIN EXEC dbo.r_denied; END"},
		{Namespace: "dbo", Name: "r_nested_deep", Body: "CREATE PROCEDURE dbo.r_nested_deep AS BEGIN EXEC dbo.r_nested_bad; END"},
		{Namespace: "dbo", Name: "r_by_func", Body: "CREATE FUNCTION dbo.r_by_func() RETURNS int AS BEGIN RETURN dbo.r_leak(); END"},
		{Namespace: "dbo", Name: "r_leak", Body: "CREATE FUNCTION dbo.r_leak() RETURNS int AS BEGIN RETURN (SELECT TOP 1 amount FROM payroll); END"},
		{Namespace: "dbo", Name: "r_dynamic", Body: "CREATE PROCEDURE dbo.r_dynamic AS BEGIN EXEC('SELECT ssn FROM customers'); END"},
		{Namespace: "dbo", Name: "r_opaque", Body: "this is not sql"},
		{Namespace: "dbo", Name: "r_silent", Body: ""},
	})
	var opts []sqlguard.Option
	if ticked {
		opts = append(opts, sqlguard.MayCallRoutines())
	}
	g, err := sqlguard.New("sqlserver", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, c, opts...)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

func TestACleanRoutineRuns(t *testing.T) {
	g := withRoutines(t, true)
	allowed(t, g, "EXEC dbo.r_clean")
	allowed(t, g, "SELECT dbo.r_clean()")
	allowed(t, g, "SELECT id FROM orders WHERE total > dbo.r_clean()")
	// A routine built out of another routine: the chain is followed to the end
	// and every link is held to the policy, rather than the first link being
	// refused for doing its work somewhere this never looked.
	allowed(t, g, "EXEC dbo.r_nested")
	// A built-in is not a routine, and refusing one would refuse everything.
	allowed(t, g, "SELECT LOWER(email) FROM customers")
	allowed(t, g, "SELECT LEFT(email, 3) FROM customers")
}

func TestARoutineIsHeldToThePolicyByItsBody(t *testing.T) {
	g := withRoutines(t, true)
	for _, c := range []struct{ sql, because string }{
		{"EXEC dbo.r_denied", "it reads a table the policy keeps back"},
		{"EXEC dbo.r_hidden", "it reads a field the policy hides, and a body cannot be rewritten"},
		{"EXEC dbo.r_nested_bad", "the routine it runs reads a table the policy keeps back"},
		{"EXEC dbo.r_nested_deep", "and that holds two links down"},
		{"EXEC dbo.r_by_func", "it calls a routine that reads a denied table"},
		{"EXEC dbo.r_dynamic", "it builds its statement out of a string"},
		{"EXEC dbo.r_opaque", "its body cannot be read"},
		{"EXEC dbo.r_silent", "this account cannot see its body"},
		{"EXEC dbo.r_missing", "the catalog has never heard of it"},
		{"SELECT dbo.r_denied()", "a select list reaches the same code"},
		{"SELECT id FROM orders WHERE total > dbo.r_hidden()", "so does a WHERE"},
		{"SELECT dbo.r_leak()", "and so does a function of its own"},
		{"SELECT * FROM dbo.r_denied()", "and so does a FROM, where the same rule names it"},
	} {
		reason := refused(t, g, c.sql)
		t.Logf("  refused %-50s (%s)", c.sql, c.because)
		if reason == "" {
			t.Errorf("%s: refused with no reason", c.sql)
		}
	}
}

func TestWithTheBoxUntickedNoRoutineRuns(t *testing.T) {
	g := withRoutines(t, false)
	for _, sql := range []string{
		"EXEC dbo.r_clean",
		"SELECT dbo.r_clean()",
		"SELECT id FROM orders WHERE total > dbo.r_clean()",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "not permitted to run stored routines") {
			t.Errorf("%s: refused for the wrong reason: %s", sql, reason)
		}
	}
	allowed(t, g, "SELECT LOWER(email) FROM customers")
}
