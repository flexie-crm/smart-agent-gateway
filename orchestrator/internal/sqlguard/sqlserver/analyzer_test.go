package sqlserver

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
// What T-SQL adds over the other two dialects has a section of its own at the
// end: OUTPUT is a select list standing somewhere else, FOR XML and FOR JSON
// roll a whole row into one value with no item left to replace, sys.* is this
// engine's pg_catalog, and a four-part name walks off the server entirely.

func guarded(t *testing.T, p sqlguard.Policy) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("sqlserver", "shop", []sqlguard.Table{
		{Name: "customers", Namespace: "dbo", Columns: []string{"id", "email", "ssn"}},
		{Name: "orders", Namespace: "dbo", Columns: []string{"id", "customer_id", "total", "note"}},
		{Name: "staff", Namespace: "dbo", Columns: []string{"id", "email", "ssn"}},
		{Name: "scratch", Namespace: "dbo", Columns: []string{"id", "note"}},
		{Name: "payroll", Namespace: "dbo", Columns: []string{"id", "emp", "amount"}},
		{Name: "customer_view", Namespace: "dbo", View: true, Reads: []string{"customers"},
			Columns: []string{"id", "email", "code"},
			Origin: map[string]sqlguard.Column{
				"id":    {Table: "customers", Name: "id"},
				"email": {Table: "customers", Name: "email"},
				// The renaming view: a rule about customers.ssn has to hold when the
				// column is offered under a name no rule mentions.
				"code": {Table: "customers", Name: "ssn"},
			}},
		{Name: "leaky_view", Namespace: "dbo", View: true, Reads: []string{"payroll"}, Columns: []string{"id", "amount"}},
		{Name: "nested_view", Namespace: "dbo", View: true, Reads: []string{"leaky_view"}, Columns: []string{"id"}},
		{Name: "unreadable_view", Namespace: "dbo", View: true, Opaque: true, Columns: []string{"id"}},
	})
	g, err := sqlguard.New("sqlserver", p, c)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

// standard is the policy the bulk of these run against: one table nobody may
// reach, one column that comes back hidden.
func standard(t *testing.T) *sqlguard.Guard {
	t.Helper()
	return guarded(t, sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
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

// masked asserts a statement runs and that the value cannot come back: the
// stand-in is in what will run, and the column is not returned by name.
func masked(t *testing.T, g *sqlguard.Guard, sql string) sqlguard.Decision {
	t.Helper()
	d := allowed(t, g, sql)
	if !strings.Contains(d.SQL, "'[hidden]'") {
		t.Fatalf("%s: nothing was hidden, ran as %q", sql, d.SQL)
	}
	return d
}

// untouched asserts a statement runs exactly as it was written. This is half the
// promise: a hidden field must go on working in a WHERE, an ORDER BY, a GROUP BY
// and a join, or the policy refuses most of what anybody wants to ask.
func untouched(t *testing.T, g *sqlguard.Guard, sql string) {
	t.Helper()
	d := allowed(t, g, sql)
	if d.Rewritten || d.SQL != sql {
		t.Fatalf("%s: should have been sent as written, became %q", sql, d.SQL)
	}
}

// --- the table rule -------------------------------------------------------

func TestATableOutOfReachIsRefusedWhereverItIsNamed(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT * FROM payroll`,
		`SELECT * FROM dbo.payroll`,
		`SELECT * FROM [dbo].[payroll]`,
		`SELECT * FROM "dbo"."payroll"`,
		`SELECT id FROM orders WHERE id IN (SELECT id FROM payroll)`,
		`SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM payroll WHERE payroll.id = orders.id)`,
		`SELECT o.id FROM orders o JOIN payroll p ON p.id = o.id`,
		`WITH a AS (SELECT id FROM payroll) SELECT id FROM a`,
		`SELECT id FROM (SELECT id FROM payroll) x`,
		`INSERT INTO scratch (id) SELECT id FROM payroll`,
		`UPDATE scratch SET note = 'x' FROM payroll WHERE payroll.id = scratch.id`,
		`DELETE FROM scratch WHERE id IN (SELECT id FROM payroll)`,
		`SELECT id FROM orders UNION SELECT id FROM payroll`,
	} {
		refused(t, g, sql)
	}
}

// A view is decided by what it reads, not by what it is called. The statement
// here never names the table nobody may see.
func TestAViewOverATableOutOfReachIsRefused(t *testing.T) {
	g := standard(t)
	refused(t, g, `SELECT * FROM leaky_view`)
	refused(t, g, `SELECT * FROM nested_view`)
	// And a view nothing can account for is refused rather than assumed harmless.
	refused(t, g, `SELECT * FROM unreadable_view`)
}

func TestAnAllowlistPermitsOnlyWhatItNames(t *testing.T) {
	g := guarded(t, sqlguard.Policy{
		TableMode: sqlguard.ModeAllowlist, Tables: "customers\norders",
	})
	allowed(t, g, `SELECT id FROM customers`)
	allowed(t, g, `SELECT id FROM orders`)
	refused(t, g, `SELECT id FROM staff`)
	refused(t, g, `SELECT id FROM scratch`)
}

// --- the field rule -------------------------------------------------------

func TestAHiddenFieldIsReplacedWhereItIsReturned(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT ssn FROM customers`,
		`SELECT c.ssn FROM customers c`,
		`SELECT customers.ssn FROM customers`,
		`SELECT [ssn] FROM customers`,
		`SELECT TOP 10 ssn FROM customers`,
		`SELECT DISTINCT ssn FROM customers`,
	} {
		masked(t, g, sql)
	}
}

// The other half of the promise, and the reason the rule is about what is
// RETURNED rather than about what is mentioned.
func TestAHiddenFieldStillWorksEverywhereElse(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT id FROM customers WHERE ssn LIKE '1%'`,
		`SELECT id FROM customers ORDER BY ssn`,
		`SELECT COUNT(*) FROM customers GROUP BY ssn`,
		`SELECT COUNT(*) FROM customers WHERE ssn IS NOT NULL`,
		`SELECT c.id FROM customers c JOIN staff s ON s.ssn = c.ssn`,
		`SELECT id FROM customers GROUP BY id, ssn HAVING COUNT(ssn) > 1`,
	} {
		untouched(t, g, sql)
	}
}

// The whole item goes, not the column inside it. Otherwise the value comes back
// with a little arithmetic wrapped round it.
func TestAnExpressionCannotCarryTheValueOut(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT CONCAT(ssn, '') FROM customers`,
		`SELECT UPPER(ssn) FROM customers`,
		`SELECT SUBSTRING(ssn, 1, 3) FROM customers`,
		`SELECT ssn + '' FROM customers`,
		`SELECT MAX(ssn) FROM customers`,
		`SELECT CASE WHEN 1=1 THEN ssn ELSE '' END FROM customers`,
		`SELECT IIF(id > 0, ssn, '') FROM customers`,
		`SELECT CAST(ssn AS varchar(20)) FROM customers`,
	} {
		d := masked(t, g, sql)
		if strings.Contains(strings.ToLower(d.SQL), "ssn)") || strings.Contains(strings.ToLower(d.SQL), "ssn,") {
			t.Fatalf("%s: the column is still read in what would run: %q", sql, d.SQL)
		}
	}
}

// Hiding happens at EVERY projection level, so nothing can read the column deep
// down and hand it upward under another name.
func TestASubqueryCannotCarryTheValueOut(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`WITH a AS (SELECT ssn FROM customers) SELECT ssn FROM a`,
		`WITH a AS (SELECT ssn FROM customers), b AS (SELECT ssn AS y FROM a) SELECT y FROM b`,
		`SELECT x.ssn FROM (SELECT ssn FROM customers) x`,
		`SELECT (SELECT TOP 1 ssn FROM customers) AS leaked`,
	} {
		masked(t, g, sql)
	}
}

// A rule about customers.ssn has to hold when the column is reached through a
// view that calls it something else. A check that read the policy's own wording
// would wave this through: no rule anywhere mentions "code".
func TestAViewCannotRenameTheValueOut(t *testing.T) {
	g := standard(t)
	masked(t, g, `SELECT code FROM customer_view`)
	masked(t, g, `SELECT v.code FROM customer_view v`)
	untouched(t, g, `SELECT id FROM customer_view WHERE code = '1'`)
}

// A star is expanded into the columns it stands for, with the hidden one
// replaced, and only where that changes something.
func TestAStarIsExpandedOnlyWhereItHasTo(t *testing.T) {
	g := standard(t)

	d := masked(t, g, `SELECT * FROM customers`)
	for _, want := range []string{"[id]", "[email]", "'[hidden]' AS [ssn]"} {
		if !strings.Contains(d.SQL, want) {
			t.Fatalf("expanded star is missing %s: %q", want, d.SQL)
		}
	}
	masked(t, g, `SELECT c.* FROM customers c`)

	// Nothing hidden in reach: the star is left exactly as written.
	untouched(t, g, `SELECT * FROM orders`)
	untouched(t, g, `SELECT o.* FROM orders o`)
}

// --- what may run at all --------------------------------------------------

func TestOnlyStatementsThisCanReadAreAllowed(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		// Built somewhere this side cannot see.
		`EXEC sp_executesql N'SELECT ssn FROM customers'`,
		`EXECUTE ('SELECT ssn FROM customers')`,
		// A body that hides what it runs.
		`CREATE VIEW v AS SELECT ssn FROM customers`,
		// Moves a table out from under its own policy.
		`ALTER TABLE customers DROP COLUMN ssn`,
		`DROP TABLE customers`,
		// Not a read at all.
		`BACKUP DATABASE shop TO DISK = 'x'`,
		`DBCC CHECKDB`,
		// Reads a table without naming one this side can resolve.
		`SELECT * FROM OPENQUERY(other, 'SELECT ssn FROM customers')`,
		`SELECT * FROM OPENROWSET('SQLNCLI', 'x', 'SELECT ssn FROM customers')`,
		// Refused as a whole rather than half-read.
		`MERGE customers AS t USING staff AS s ON t.id = s.id WHEN MATCHED THEN UPDATE SET t.email = s.email;`,
		// Makes a new table out of the answer, which has no policy of its own.
		`SELECT * INTO copy FROM customers`,
	} {
		refused(t, g, sql)
	}
}

func TestOneStatementAtATime(t *testing.T) {
	g := standard(t)
	// The driver cannot enforce this on SQL Server the way it can on Postgres,
	// so this is the only thing standing between a batch and the database.
	for _, sql := range []string{
		`SELECT id FROM orders; SELECT ssn FROM customers`,
		"SELECT id FROM orders\nGO\nSELECT ssn FROM customers",
		`SELECT id FROM orders; DROP TABLE customers`,
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "one statement per call") {
			t.Fatalf("%s: refused for the wrong reason: %s", sql, reason)
		}
	}
	// One statement with a trailing semicolon is still one statement.
	untouched(t, g, `SELECT id FROM orders`)
}

func TestWhatCannotBeReadIsRefused(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT FROM WHERE`,
		`SELECT * FROM (`,
		`THIS IS NOT SQL AT ALL`,
		`SELECT id FROM customers WHERE )))`,
	} {
		refused(t, g, sql)
	}
}

// --- what this dialect adds -----------------------------------------------

// OUTPUT is a select list standing somewhere else. Everything true of one is
// true of the other, and a check that only looked at select lists would hand the
// value straight back.
func TestOutputIsAProjectionLikeAnyOther(t *testing.T) {
	g := standard(t)
	masked(t, g, `DELETE FROM customers OUTPUT deleted.ssn WHERE id = 1`)
	masked(t, g, `UPDATE customers SET email = 'x' OUTPUT inserted.ssn WHERE id = 1`)
	masked(t, g, `DELETE FROM customers OUTPUT deleted.* WHERE id = 1`)
	// And one that returns nothing hidden runs as written.
	untouched(t, g, `DELETE FROM customers OUTPUT deleted.id WHERE id = 1`)
}

// OUTPUT ... INTO writes the rows into a real table. Masking there would put the
// stand-in over real data, so it is refused instead.
func TestOutputIntoIsRefused(t *testing.T) {
	g := standard(t)
	refused(t, g, `DELETE FROM customers OUTPUT deleted.ssn INTO scratch WHERE id = 1`)
}

// FOR XML and FOR JSON roll the whole row into one value. There is no item left
// to replace, so a statement over a table with something hidden in it is
// refused, and one over a table without is not.
func TestRollingAWholeRowIntoOneValue(t *testing.T) {
	g := standard(t)
	refused(t, g, `SELECT * FROM customers FOR JSON AUTO`)
	refused(t, g, `SELECT id, ssn FROM customers FOR XML AUTO`)
	untouched(t, g, `SELECT * FROM orders FOR JSON AUTO`)
}

// sys.* is this engine's pg_catalog: on every name-resolution path, and holding
// the text of every view definition, which names the columns a policy exists to
// keep quiet.
func TestTheServersOwnCatalogIsRefused(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		`SELECT name FROM sys.tables`,
		`SELECT definition FROM sys.sql_modules`,
		`SELECT name FROM sys.columns`,
		`SELECT name FROM sysobjects`,
	} {
		refused(t, g, sql)
	}
}

// One tool is one database. Three parts reach another database on this server,
// four reach another server entirely.
func TestNothingOutsideThisDatabase(t *testing.T) {
	g := standard(t)
	refused(t, g, `SELECT ssn FROM other.dbo.customers`)
	refused(t, g, `SELECT ssn FROM remote.shop.dbo.customers`)
	// The database's own name in front of it is the database it already is.
	masked(t, g, `SELECT ssn FROM shop.dbo.customers`)
}

// Writing INTO a hidden field would put the stand-in over what is really there,
// and reading one in a SET would copy the real value somewhere readable. Neither
// refusal is about the policy's wording.
func TestAHiddenFieldCannotBeWrittenOrCopied(t *testing.T) {
	g := standard(t)
	refused(t, g, `UPDATE customers SET ssn = 'x' WHERE id = 1`)
	refused(t, g, `UPDATE customers SET email = ssn WHERE id = 1`)
	// Writing something else is fine.
	untouched(t, g, `UPDATE customers SET email = 'x' WHERE id = 1`)
}

// A star in the rows an INSERT takes is values going IN. Masking there would
// store the stand-in permanently, so it is refused instead.
func TestAStarFeedingAWriteIsRefusedRatherThanMasked(t *testing.T) {
	g := standard(t)
	refused(t, g, `INSERT INTO staff SELECT * FROM customers`)
}

// --- a policy that governs nothing ----------------------------------------

// Every tool made before this feature existed has a blank policy, and must go on
// behaving exactly as it did. The guard is not even built for one, so this
// asserts the analyzer agrees when it is.
func TestABlankPolicyChangesNothing(t *testing.T) {
	g := guarded(t, sqlguard.Policy{})
	for _, sql := range []string{
		`SELECT ssn FROM customers`,
		`SELECT * FROM customers`,
		`SELECT * FROM payroll`,
	} {
		untouched(t, g, sql)
	}
}

// --- the sweep ------------------------------------------------------------

// PIVOT and UNPIVOT invent columns out of a hidden one and give them new names,
// so there is no reference left for the select list above to replace. They are
// refused in the descent, by name.
func TestPivotIsRefused(t *testing.T) {
	g := standard(t)
	refused(t, g, `SELECT * FROM (SELECT id, ssn FROM customers) s PIVOT (MAX(ssn) FOR id IN ([1],[2])) p`)
	refused(t, g, `SELECT product, qty FROM customers UNPIVOT (qty FOR product IN (id, email)) u`)
}

// The sweep is the pass that makes the descent trustworthy, and it is worth a
// test of its own rather than a test of some shape that happens to be
// unhandled today. A shape refused BY NAME in the descent (PIVOT, above) proves
// nothing about the sweep: it would be refused with the sweep deleted.
//
// So this drives the mechanism directly. It reads a statement the descent
// handles completely, then takes one reference out of what the descent accounted
// for, exactly as an unvisited node would be absent, and asserts the sweep
// refuses. The control is the same statement with nothing removed: if that ever
// refuses too, this test is measuring something other than what it claims.
func TestTheSweepRefusesWhatTheDescentNeverVisited(t *testing.T) {
	g := standard(t)
	const sql = `SELECT c.id, c.email FROM customers c WHERE c.ssn LIKE '1%'`

	// The control first: fully accounted for, nothing refused.
	clause, stream, reason := readStatement(sql)
	if reason != "" {
		t.Fatalf("control: %s", reason)
	}
	a := newAnalysis(g, stream, clause)
	a.statement(clause)
	a.sweep(clause)
	if a.reason != "" {
		t.Fatalf("control: a fully visited statement was refused: %s", a.reason)
	}
	if len(a.columns) == 0 {
		t.Fatal("control: the descent found no columns, so removing one proves nothing")
	}

	// Now the same statement with one column unaccounted for.
	clause, stream, reason = readStatement(sql)
	if reason != "" {
		t.Fatalf("subject: %s", reason)
	}
	b := newAnalysis(g, stream, clause)
	b.statement(clause)
	if len(b.seenColumns) == 0 {
		t.Fatal("the descent accounted for no columns at all")
	}
	for node := range b.seenColumns {
		delete(b.seenColumns, node)
		break
	}
	b.sweep(clause)
	if b.reason == "" {
		t.Fatal("a column the descent never accounted for was not refused, " +
			"so nothing stands between an unwalked corner of the grammar and a hidden value")
	}
}
