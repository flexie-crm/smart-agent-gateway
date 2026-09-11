package postgres

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// These tests are the ways a check that read the TEXT of a statement would have
// been fooled, and the ways a check that read only the columns a statement
// PRINTS would have been fooled. Each one is a real route to a table an
// administrator said was out of reach, or to the value of a column they said was
// hidden.
//
// The routes PostgreSQL adds over MySQL have a section of their own at the end:
// a row is a value here, so a table can be handed back whole without a single
// column being named, and the server's own catalog is on every search path
// whether anybody put it there or not.

func guarded(t *testing.T, p sqlguard.Policy) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("postgres", "shop", []sqlguard.Table{
		{Name: "customers", Namespace: "public", Columns: []string{"id", "email", "ssn", "profile"}},
		{Name: "orders", Namespace: "public", Columns: []string{"id", "customer_id", "total", "note"}},
		{Name: "staff", Namespace: "public", Columns: []string{"id", "email", "ssn"}},
		{Name: "scratch", Namespace: "public", Columns: []string{"id", "note"}},
		{Name: "secret_keys", Namespace: "public", Columns: []string{"id", "value"}},
		{Name: "customer_view", Namespace: "public", View: true, Reads: []string{"customers"},
			Columns: []string{"id", "email", "ssn"},
			Origin: map[string]sqlguard.Column{
				"id":    {Table: "customers", Name: "id"},
				"email": {Table: "customers", Name: "email"},
				"ssn":   {Table: "customers", Name: "ssn"},
			}},
		{Name: "leaky_view", Namespace: "public", View: true, Reads: []string{"secret_keys"}, Columns: []string{"id", "value"}},
		{Name: "nested_view", Namespace: "public", View: true, Reads: []string{"leaky_view"}, Columns: []string{"id"}},
		{Name: "unreadable_view", Namespace: "public", View: true, Opaque: true, Columns: []string{"id"}},
	})
	g, err := sqlguard.New("postgres", p, c)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

// standard is the policy the bulk of these tests run against: one table nobody
// may reach, one column that comes back hidden.
func standard(t *testing.T) *sqlguard.Guard {
	t.Helper()
	return guarded(t, sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	})
}

func refused(t *testing.T, g *sqlguard.Guard, sql string) string {
	t.Helper()
	d, reason, err := g.Check(sql)
	if err != nil {
		t.Fatalf("%s: unexpected error %v", sql, err)
	}
	if reason == "" {
		t.Fatalf("%s: expected a refusal, got %q", sql, d.SQL)
	}
	return reason
}

func allowed(t *testing.T, g *sqlguard.Guard, sql string) sqlguard.Decision {
	t.Helper()
	d, reason, err := g.Check(sql)
	if err != nil {
		t.Fatalf("%s: unexpected error %v", sql, err)
	}
	if reason != "" {
		t.Fatalf("%s: expected it to run, refused with %q", sql, reason)
	}
	return d
}

func rewritten(t *testing.T, g *sqlguard.Guard, sql, want string) {
	t.Helper()
	d := allowed(t, g, sql)
	if !d.Rewritten {
		t.Fatalf("%s: expected it to be rewritten, it was passed through as-is", sql)
	}
	if d.SQL != want {
		t.Fatalf("%s\n  got  %s\n  want %s", sql, d.SQL, want)
	}
}

// ---------------------------------------------------------------- denied table

func TestADeniedTableCannotBeReachedAnyWay(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT * FROM secret_keys",
		`SELECT * FROM "secret_keys"`,
		"SELECT * FROM public.secret_keys",
		"SELECT id FROM orders WHERE id IN (SELECT id FROM secret_keys)",
		"SELECT id FROM orders UNION SELECT id FROM secret_keys",
		"WITH q AS (SELECT * FROM secret_keys) SELECT * FROM q",
		"SELECT * FROM orders o JOIN secret_keys k ON k.id = o.id",
		"SELECT * FROM (SELECT * FROM secret_keys) x",
		"SELECT (SELECT value FROM secret_keys LIMIT 1)",
		"SELECT * FROM leaky_view",
		"SELECT * FROM nested_view",
		"SELECT * FROM unreadable_view",
		"SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM secret_keys)",
		"SELECT * FROM orders, LATERAL (SELECT value FROM secret_keys) k",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "rewording will not help") {
			t.Errorf("%s: refusal should tell the caller not to try again: %q", sql, reason)
		}
	}
}

func TestADeniedTableCannotBeWrittenToEither(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"INSERT INTO secret_keys (id, value) VALUES (1, 'x')",
		"UPDATE secret_keys SET value = 'x'",
		"DELETE FROM secret_keys",
		"INSERT INTO orders (note) SELECT value FROM secret_keys",
		"WITH d AS (DELETE FROM secret_keys RETURNING id) SELECT * FROM d",
		"UPDATE orders SET note = 'x' FROM secret_keys WHERE secret_keys.id = orders.id",
	} {
		refused(t, g, sql)
	}
}

func TestAnAllowlistPermitsOnlyTheTablesItNames(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "orders\ncustomers"})
	allowed(t, g, "SELECT id FROM orders")
	allowed(t, g, "SELECT id FROM customers")
	refused(t, g, "SELECT id FROM staff")
	refused(t, g, "SELECT id FROM secret_keys")
	// A table made since the snapshot was taken is not on a list of names, so an
	// allowlist keeps it out without having heard of it.
	refused(t, g, "SELECT id FROM made_this_afternoon")
}

func TestOneToolReachesOneSetOfSchemas(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT id FROM other.customers",
		"SELECT other.customers.id FROM other.customers",
		"INSERT INTO other.customers (id) VALUES (1)",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "other") {
			t.Errorf("%s: refusal should name the schema it would not reach: %q", sql, reason)
		}
	}
	// The schema the snapshot covers is reached by name as well as bare.
	allowed(t, g, "SELECT id FROM public.orders")
}

// ------------------------------------------------------- what may run at all

func TestOnlyStatementsTheGuardCanReadEndToEndAreRun(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"CREATE TABLE x (a int)",
		"DROP TABLE orders",
		"ALTER TABLE customers RENAME TO safe",
		"CREATE VIEW v AS SELECT * FROM customers",
		"CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql",
		"DO $$ BEGIN END $$",
		"COPY customers TO STDOUT",
		"COPY customers FROM '/tmp/x'",
		"GRANT SELECT ON customers TO public",
		"TRUNCATE customers",
		"SET search_path TO other",
		"CREATE TEMP TABLE t AS SELECT * FROM customers",
		"PREPARE p AS SELECT * FROM customers",
		"BEGIN",
		"VACUUM customers",
		"LISTEN x",
		"CREATE RULE r AS ON SELECT TO orders DO INSTEAD SELECT * FROM secret_keys",
	} {
		refused(t, g, sql)
	}
}

func TestManyStatementsAtOnce(t *testing.T) {
	g := standard(t)
	// The one that took a table away when the driver fell back to the simple
	// protocol. It is refused here as well, above the driver.
	for _, sql := range []string{
		"SELECT 1; DROP TABLE orders",
		"SELECT id FROM orders; SELECT value FROM secret_keys",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "one statement") {
			t.Errorf("%s: %q", sql, reason)
		}
	}
	if reason := refused(t, g, "   "); !strings.Contains(reason, "empty") {
		t.Errorf("blank statement: %q", reason)
	}
	// A trailing semicolon on a single statement is not two statements.
	allowed(t, g, "SELECT id FROM orders;")
}

func TestAPlanIsReadLikeTheStatementItExplains(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "EXPLAIN SELECT ssn FROM customers",
		"EXPLAIN SELECT '[hidden]' AS ssn FROM customers")
	refused(t, g, "EXPLAIN SELECT * FROM secret_keys")
	// ANALYZE runs the statement rather than explaining it.
	reason := refused(t, g, "EXPLAIN ANALYZE SELECT id FROM orders")
	if !strings.Contains(reason, "ANALYZE") {
		t.Errorf("EXPLAIN ANALYZE: %q", reason)
	}
}

// --------------------------------------------------------------- hidden field

func TestAHiddenColumnComesBackHidden(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	rewritten(t, g, "SELECT c.ssn FROM customers c", "SELECT '[hidden]' AS ssn FROM customers c")
	rewritten(t, g, "SELECT ssn AS s FROM customers", "SELECT '[hidden]' AS s FROM customers")
	rewritten(t, g, "SELECT id, ssn, email FROM customers",
		"SELECT id, '[hidden]' AS ssn, email FROM customers")
	// An expression that reads it hands the value back with arithmetic wrapped
	// round it, so the whole item goes rather than the column inside it.
	rewritten(t, g, "SELECT upper(ssn) FROM customers", "SELECT '[hidden]' AS upper FROM customers")
	rewritten(t, g, "SELECT ssn || 'x' FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	rewritten(t, g, "SELECT ssn::text FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	rewritten(t, g, "SELECT CASE WHEN id > 0 THEN ssn ELSE email END FROM customers",
		`SELECT '[hidden]' AS "case" FROM customers`)
	rewritten(t, g, "SELECT coalesce(ssn, email) FROM customers",
		`SELECT '[hidden]' AS "coalesce" FROM customers`)
	rewritten(t, g, "SELECT substr(ssn, 1, 1) FROM customers", "SELECT '[hidden]' AS substr FROM customers")
}

func TestAStarBecomesTheColumnsItStandsFor(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT * FROM customers",
		"SELECT customers.id, customers.email, '[hidden]' AS ssn, customers.profile FROM customers")
	rewritten(t, g, "SELECT * FROM customers c",
		"SELECT c.id, c.email, '[hidden]' AS ssn, c.profile FROM customers c")
	rewritten(t, g, "SELECT c.* FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT c.id, c.email, '[hidden]' AS ssn, c.profile FROM customers c JOIN orders o ON o.customer_id = c.id")
	// The short form of a select is the same star written another way.
	rewritten(t, g, "TABLE customers",
		"SELECT customers.id, customers.email, '[hidden]' AS ssn, customers.profile FROM customers")
	// A natural join names no columns at all, and is expanded like any other.
	d := allowed(t, g, "SELECT * FROM customers c NATURAL JOIN orders o")
	if !strings.Contains(d.SQL, "'[hidden]' AS ssn") {
		t.Fatalf("natural join: %s", d.SQL)
	}
}

func TestAQueryWithNothingToHideIsSentOnExactlyAsItWasWritten(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT * FROM orders",
		"SELECT id, note FROM orders WHERE total > $1 ORDER BY id LIMIT 10",
		"SELECT o.*, s.email FROM orders o JOIN staff s ON s.id = o.customer_id",
		"SELECT ssn FROM staff",
		"SELECT count(*) FROM customers",
	} {
		d := allowed(t, g, sql)
		if d.Rewritten || d.SQL != sql {
			t.Errorf("%s: was rewritten to %s", sql, d.SQL)
		}
	}
}

func TestAHiddenFieldIsUsableEverywhereAndReturnedNowhere(t *testing.T) {
	g := standard(t)
	// Reading it to decide which rows come back, in what order, or what to join
	// them to gives nothing away, and is left exactly as it was written.
	for _, sql := range []string{
		"SELECT id FROM customers WHERE ssn = $1",
		"SELECT id FROM customers WHERE ssn LIKE $1 || '%'",
		"SELECT id FROM customers ORDER BY ssn",
		"SELECT count(*) FROM customers GROUP BY ssn",
		"SELECT count(*) FROM customers HAVING count(ssn) > 0",
		"SELECT c.id FROM customers c JOIN staff s ON s.ssn = c.ssn",
		"SELECT id FROM customers WHERE ssn IS NOT NULL",
		"DELETE FROM customers WHERE ssn = $1",
		"UPDATE customers SET email = $1 WHERE ssn = $2",
		"SELECT id FROM customers GROUP BY ssn HAVING ssn > $1",
	} {
		d := allowed(t, g, sql)
		if d.Rewritten {
			t.Errorf("%s: should have been left alone, became %s", sql, d.SQL)
		}
	}
}

func TestAHiddenValueCannotBeCarriedUpThroughSubqueries(t *testing.T) {
	g := standard(t)
	// Whatever route the value takes towards the caller, it is taken out at the
	// point it would be returned.
	rewritten(t, g, "SELECT * FROM (SELECT ssn FROM customers) x",
		"SELECT * FROM (SELECT '[hidden]' AS ssn FROM customers) x")
	rewritten(t, g, "SELECT x.s FROM (SELECT ssn AS s FROM customers) x",
		"SELECT x.s FROM (SELECT '[hidden]' AS s FROM customers) x")
	rewritten(t, g, "WITH q AS (SELECT ssn FROM customers) SELECT * FROM q",
		"WITH q AS (SELECT '[hidden]' AS ssn FROM customers) SELECT * FROM q")
	rewritten(t, g, "SELECT (SELECT ssn FROM customers LIMIT 1) AS leak",
		"SELECT (SELECT '[hidden]' AS ssn FROM customers LIMIT 1) AS leak")
	rewritten(t, g, "SELECT ssn FROM customers UNION ALL SELECT email FROM customers",
		"SELECT '[hidden]' AS ssn FROM customers UNION ALL SELECT email FROM customers")
	rewritten(t, g, "SELECT t.ssn FROM customers c, LATERAL (SELECT c.ssn AS ssn) t",
		"SELECT t.ssn FROM customers c, LATERAL (SELECT '[hidden]' AS ssn) t")
	// A star over a subquery does not need expanding: what the subquery projects
	// was already decided inside it.
	rewritten(t, g, "SELECT * FROM (SELECT * FROM customers) x",
		"SELECT * FROM (SELECT customers.id, customers.email, '[hidden]' AS ssn, customers.profile FROM customers) x")
}

// A column in a subquery's WHERE belongs to the subquery, not to the select item
// the subquery sits inside. Getting that wrong hid the outer item.
func TestAnInnerConditionDoesNotHideTheItemItSitsIn(t *testing.T) {
	g := standard(t)
	d := allowed(t, g, "SELECT (SELECT count(*) FROM customers WHERE ssn = $1) AS n")
	if d.Rewritten {
		t.Fatalf("should have been left alone, became %s", d.SQL)
	}
	d = allowed(t, g, "SELECT o.id FROM orders o WHERE o.customer_id IN (SELECT id FROM customers WHERE ssn = $1)")
	if d.Rewritten {
		t.Fatalf("should have been left alone, became %s", d.SQL)
	}
}

func TestAHiddenColumnIsResolvedToItsOwnTable(t *testing.T) {
	g := standard(t)
	// staff.ssn is a different column with the same name, and no rule mentions
	// it.
	d := allowed(t, g, "SELECT s.ssn FROM staff s")
	if d.Rewritten {
		t.Fatalf("staff.ssn should be untouched, became %s", d.SQL)
	}
	rewritten(t, g, "SELECT c.ssn, s.ssn FROM customers c JOIN staff s ON s.id = c.id",
		"SELECT '[hidden]' AS ssn, s.ssn FROM customers c JOIN staff s ON s.id = c.id")
	// Unqualified and only one table in scope has the column.
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	// Unqualified and the tables disagree: hidden when it would be returned.
	rewritten(t, g, "SELECT ssn FROM customers, staff", "SELECT '[hidden]' AS ssn FROM customers, staff")
}

func TestTheWildcardHidesAColumnWhereverItIs(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "*.ssn"})
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	rewritten(t, g, "SELECT ssn FROM staff", "SELECT '[hidden]' AS ssn FROM staff")
	rewritten(t, g, "SELECT * FROM staff",
		"SELECT staff.id, staff.email, '[hidden]' AS ssn FROM staff")
}

func TestAFieldAllowlistShowsOnlyWhatItNames(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeAllowlist, Fields: "customers.id\ncustomers.email"})
	rewritten(t, g, "SELECT * FROM customers",
		"SELECT customers.id, customers.email, '[hidden]' AS ssn, '[hidden]' AS profile FROM customers")
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	d := allowed(t, g, "SELECT email FROM customers")
	if d.Rewritten {
		t.Fatalf("an allowed field should be untouched, became %s", d.SQL)
	}
}

func TestAHiddenFieldIsReachedThroughAViewByWhateverNameItHasThere(t *testing.T) {
	g := standard(t)
	// customer_view.ssn reads customers.ssn, which is hidden, so it is hidden
	// here too even though no rule names the view.
	rewritten(t, g, "SELECT ssn FROM customer_view", "SELECT '[hidden]' AS ssn FROM customer_view")
	rewritten(t, g, "SELECT * FROM customer_view",
		"SELECT customer_view.id, customer_view.email, '[hidden]' AS ssn FROM customer_view")
}

// ---------------------------------------------------------------------- writes

func TestAWriteReadsAHiddenFieldLikeAnythingElse(t *testing.T) {
	g := standard(t)
	// Returned by a write, so taken out.
	rewritten(t, g, "UPDATE customers SET email = $1 WHERE id = $2 RETURNING id, ssn",
		"UPDATE customers SET email = $1 WHERE id = $2 RETURNING id, '[hidden]' AS ssn")
	rewritten(t, g, "DELETE FROM customers WHERE id = $1 RETURNING *",
		"DELETE FROM customers WHERE id = $1 RETURNING customers.id, customers.email, '[hidden]' AS ssn, customers.profile")
	rewritten(t, g, "INSERT INTO customers (email) VALUES ($1) RETURNING ssn",
		"INSERT INTO customers (email) VALUES ($1) RETURNING '[hidden]' AS ssn")
	// Copied into another column, where it would be readable afterwards.
	for _, sql := range []string{
		"UPDATE customers SET profile = ssn",
		"UPDATE customers SET profile = upper(ssn)",
		"INSERT INTO customers (email) VALUES ('x') ON CONFLICT (id) DO UPDATE SET profile = customers.ssn",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "copied into another column") {
			t.Errorf("%s: %q", sql, reason)
		}
	}
	// Written INTO, where the stand-in would go over the real value.
	for _, sql := range []string{
		"UPDATE customers SET ssn = $1",
		"INSERT INTO customers (ssn) VALUES ($1)",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "cannot be written to") {
			t.Errorf("%s: %q", sql, reason)
		}
	}
}

func TestAWriteIsNeverMaskedIntoTheTable(t *testing.T) {
	g := standard(t)
	// The rows going IN cannot be masked: the stand-in would be stored.
	reason := refused(t, g, "INSERT INTO scratch SELECT * FROM customers")
	if !strings.Contains(reason, "Name the columns") {
		t.Errorf("%q", reason)
	}
	// An INSERT ... SELECT that names the hidden column is a business decision
	// rather than a leak: the row that arrives holds the stand-in.
	rewritten(t, g, "INSERT INTO scratch (note) SELECT ssn FROM customers",
		"INSERT INTO scratch (note) SELECT '[hidden]' AS ssn FROM customers")
}

func TestAnInsertWithNoColumnListIsDecidedByTheTable(t *testing.T) {
	g := standard(t)
	reason := refused(t, g, "INSERT INTO customers VALUES (1, 'a', 'b', 'c')")
	if !strings.Contains(reason, "without a column list") {
		t.Errorf("%q", reason)
	}
	// A table with nothing hidden in it is unaffected.
	allowed(t, g, "INSERT INTO orders VALUES (1, 2, 3, 'note')")
}

// ------------------------------------------------------------ catalog reading

func TestACatalogReadIsNarrowedToWhatTheToolMaySee(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT table_name FROM information_schema.tables",
		"SELECT table_name FROM information_schema.tables WHERE tables.table_name NOT IN ('secret_keys', 'leaky_view', 'nested_view', 'unreadable_view')")
	rewritten(t, g, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND tables.table_name NOT IN ('secret_keys', 'leaky_view', 'nested_view', 'unreadable_view')")
	rewritten(t, g, "SELECT column_name FROM information_schema.columns c WHERE c.table_name = $1",
		"SELECT column_name FROM information_schema.columns c WHERE c.table_name = $1 AND c.table_name NOT IN ('secret_keys', 'leaky_view', 'nested_view', 'unreadable_view')")
	// An OR in the existing condition keeps its own shape: the narrowing is
	// another argument of an AND, not text glued onto the end.
	rewritten(t, g, "SELECT table_name FROM information_schema.tables WHERE table_name = 'a' OR table_name = 'b'",
		"SELECT table_name FROM information_schema.tables WHERE (table_name = 'a' OR table_name = 'b') AND tables.table_name NOT IN ('secret_keys', 'leaky_view', 'nested_view', 'unreadable_view')")
	// The parts of the catalog that describe something other than tables and
	// columns cannot be narrowed, so they are not read.
	refused(t, g, "SELECT * FROM information_schema.views")
	refused(t, g, "SELECT * FROM information_schema.routines")
	refused(t, g, "SELECT * FROM information_schema.triggers")
}

func TestAnAllowlistNarrowsACatalogReadToTheNamesItPermits(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "orders"})
	rewritten(t, g, "SELECT table_name FROM information_schema.tables",
		"SELECT table_name FROM information_schema.tables WHERE tables.table_name IN ('orders')")
}

func TestAnAllowlistThatMatchesNothingAnswersWithNothing(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "nothing_by_this_name"})
	rewritten(t, g, "SELECT table_name FROM information_schema.tables",
		"SELECT table_name FROM information_schema.tables WHERE 0 = 1")
}

func TestASecondSourceCannotEvictTheCatalogFromScope(t *testing.T) {
	g := standard(t)
	// A second source claiming the same name must not take the catalog's place:
	// that is how a read stopped being narrowed at all.
	d := allowed(t, g, "SELECT t.table_name FROM information_schema.tables, orders AS tables")
	if !strings.Contains(d.SQL, "tables.table_name NOT IN ('secret_keys'") {
		t.Fatalf("catalog read was not narrowed: %s", d.SQL)
	}
}

func TestTheCatalogIsNotReadFromAStatementThatChangesRows(t *testing.T) {
	g := standard(t)
	// There is no one WHERE to narrow, and half a narrowing is worse than none.
	reason := refused(t, g, "DELETE FROM scratch USING information_schema.tables WHERE scratch.note = tables.table_name")
	if !strings.Contains(reason, "changes rows") {
		t.Errorf("%q", reason)
	}
	// Asked as a SELECT of its own, it is narrowed and allowed.
	allowed(t, g, "SELECT table_name FROM information_schema.tables")
}

// ------------------------------------------------- what PostgreSQL adds

// A row is a value in PostgreSQL, so a table can be handed back whole without a
// single column being named. Read as written, SELECT c FROM customers c mentions
// no column at all, and every column of the table comes back with it.
func TestAWholeRowCannotBeHandedBackWhenSomethingInItIsHidden(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT customers FROM customers",
		"SELECT c FROM customers c",
		"SELECT row_to_json(c) FROM customers c",
		"SELECT to_jsonb(c) FROM customers c",
		"SELECT c::text FROM customers c",
		"INSERT INTO scratch (note) SELECT c::text FROM customers c",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "whole row") {
			t.Errorf("%s: %q", sql, reason)
		}
	}
	// A row of a table with nothing hidden in it is an ordinary value.
	allowed(t, g, "SELECT row_to_json(o) FROM orders o")
	allowed(t, g, "SELECT s FROM staff s")
	// A row built out of a subquery holds what that subquery projected, which
	// was already decided there.
	allowed(t, g, "SELECT x FROM (SELECT id, email FROM customers) x")
	// Used without being returned, a row gives nothing away.
	d := allowed(t, g, "SELECT id FROM customers c WHERE c IS NOT NULL")
	if d.Rewritten {
		t.Fatalf("should have been left alone, became %s", d.SQL)
	}
}

// (c).ssn names a column without a column reference anywhere in it: the row is a
// reference and the field beside it is a bare string.
func TestAFieldTakenOutOfARowIsStillTheColumnItNames(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT (c).ssn FROM customers c", "SELECT '[hidden]' AS ssn FROM customers c")
	rewritten(t, g, "SELECT (customers.*).ssn FROM customers", "SELECT '[hidden]' AS ssn FROM customers")
	d := allowed(t, g, "SELECT (c).email FROM customers c")
	if d.Rewritten {
		t.Fatalf("an unhidden field should be untouched, became %s", d.SQL)
	}
	// A field of a row of a table with something hidden in it, where the field
	// itself cannot be read, falls back to the row rule.
	refused(t, g, "SELECT (c).*::text FROM customers c")
}

// pg_catalog is on every connection's search path whether anybody put it there
// or not, so the server's own catalog is reachable without a schema in front of
// it to refuse.
func TestTheServersOwnCatalogIsNotReadable(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT * FROM pg_catalog.pg_class",
		"SELECT relname FROM pg_class",
		"SELECT * FROM pg_tables",
		"SELECT * FROM pg_views",
		"SELECT definition FROM pg_views WHERE viewname = 'customer_view'",
		"SELECT * FROM pg_toast.x",
		"SELECT id FROM orders WHERE id IN (SELECT oid FROM pg_class)",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "catalog") {
			t.Errorf("%s: %q", sql, reason)
		}
	}
}

// A shape the first pass does not understand leaves what it names undecided, and
// an undecided name is refused rather than let past.
func TestAStatementThatCannotBeReadEndToEndIsRefused(t *testing.T) {
	g := standard(t)
	// A multi-column assignment reads a subquery into several columns at once,
	// which this does not account for.
	reason := refused(t, g, "UPDATE customers SET (email, profile) = (SELECT email, ssn FROM staff LIMIT 1)")
	if !strings.Contains(reason, "plainer way") {
		t.Errorf("%q", reason)
	}
}

// ------------------------------------------------------------------- the rest

func TestAnUnknownTableIsReported(t *testing.T) {
	g := standard(t)
	d := allowed(t, g, "SELECT id FROM made_this_afternoon")
	if !d.SawUnknownTable {
		t.Fatal("a table the snapshot has never heard of should be reported, so the catalog can be read again")
	}
	d = allowed(t, g, "SELECT id FROM orders")
	if d.SawUnknownTable {
		t.Fatal("a table in the snapshot is not unknown")
	}
}

func TestAPolicyOnADialectThisBuildCannotReadIsRefused(t *testing.T) {
	if !sqlguard.Supports("postgres") {
		t.Fatal("importing this package should register the postgres analyzer")
	}
}

func TestAParameterSurvivesBeingRewritten(t *testing.T) {
	g := standard(t)
	// The placeholder keeps its number and its place. A rewrite that renumbered
	// or renamed them would send the arguments to the wrong columns.
	rewritten(t, g, "SELECT ssn, id FROM customers WHERE email = $1 AND id > $2",
		"SELECT '[hidden]' AS ssn, id FROM customers WHERE email = $1 AND id > $2")
}

func TestALiteralSurvivesBeingRewritten(t *testing.T) {
	g := standard(t)
	// A backslash in a literal is a backslash, and stays one: reading it as an
	// escape would send the database a different value than the caller wrote.
	d := allowed(t, g, `SELECT ssn FROM customers WHERE email = 'a\b'`)
	if !strings.Contains(d.SQL, `E'a\\b'`) {
		t.Fatalf("literal changed meaning: %s", d.SQL)
	}
	d = allowed(t, g, `SELECT ssn FROM customers WHERE email = 'it''s'`)
	if !strings.Contains(d.SQL, `'it''s'`) {
		t.Fatalf("literal changed meaning: %s", d.SQL)
	}
}
