package mysql

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// These tests are the ways a check that read the TEXT of a statement would have
// been fooled, and the ways a check that read only the columns a statement PRINTS
// would have been fooled. Each one is a real route to a table an administrator
// said was out of reach, or to the value of a column they said was hidden.

func guarded(t *testing.T, p sqlguard.Policy) *sqlguard.Guard {
	t.Helper()
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "customers", Columns: []string{"id", "email", "ssn", "profile"}},
		{Name: "orders", Columns: []string{"id", "customer_id", "total", "note"}},
		{Name: "staff", Columns: []string{"id", "email", "ssn"}},
		{Name: "scratch", Columns: []string{"id", "note"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
		{Name: "customer_view", View: true, Reads: []string{"customers"}, Columns: []string{"id", "email", "ssn"},
			Origin: map[string]sqlguard.Column{
				"id":    {Table: "customers", Name: "id"},
				"email": {Table: "customers", Name: "email"},
				"ssn":   {Table: "customers", Name: "ssn"},
			}},
		{Name: "leaky_view", View: true, Reads: []string{"secret_keys"}, Columns: []string{"id", "value"}},
		{Name: "nested_view", View: true, Reads: []string{"leaky_view"}, Columns: []string{"id"}},
		{Name: "unreadable_view", View: true, Opaque: true, Columns: []string{"id"}},
	})
	g, err := sqlguard.New("mysql", p, c)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return g
}

// standard is the policy the bulk of these tests run against: one table nobody
// may reach, one column that comes back hidden.
func standard(t *testing.T) *sqlguard.Guard {
	t.Helper()
	return guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys", FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"})
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
		"SELECT * FROM `secret_keys`",
		"SELECT * FROM SECRET_KEYS",
		"SELECT * FROM Secret_Keys",
		"SELECT * FROM shop.secret_keys",
		"SELECT o.id FROM orders o JOIN secret_keys s ON s.id = o.id",
		"SELECT id FROM orders WHERE id IN (SELECT id FROM secret_keys)",
		"SELECT (SELECT value FROM secret_keys LIMIT 1) AS v",
		"SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM secret_keys)",
		"WITH s AS (SELECT * FROM secret_keys) SELECT * FROM s",
		"SELECT * FROM (SELECT * FROM secret_keys) d",
		"SELECT id FROM orders UNION SELECT id FROM secret_keys",
		"SELECT id FROM orders WHERE IF(EXISTS(SELECT 1 FROM secret_keys), SLEEP(2), 0)",
		"EXPLAIN SELECT * FROM secret_keys",
		"DESCRIBE secret_keys",
		"SHOW COLUMNS FROM secret_keys",
		"SHOW CREATE TABLE secret_keys",
		"SHOW INDEX FROM secret_keys",
		// Through a view that never names it.
		"SELECT * FROM leaky_view",
		"SELECT * FROM nested_view",
		// And a view whose definition could not be read is not assumed harmless.
		"SELECT * FROM unreadable_view",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "rewording will not help") && !strings.Contains(reason, "not allowed") {
			t.Errorf("%s: refusal should tell the assistant not to retry, got %q", sql, reason)
		}
	}
}

func TestADeniedTableCannotBeWrittenToEither(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys", FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"})
	for _, sql := range []string{
		"INSERT INTO secret_keys (id) VALUES (1)",
		"INSERT INTO scratch (note) SELECT value FROM secret_keys",
		"UPDATE scratch SET note = (SELECT value FROM secret_keys LIMIT 1)",
		"DELETE FROM secret_keys",
		"REPLACE INTO scratch SELECT * FROM secret_keys",
	} {
		refused(t, g, sql)
	}
}

func TestAnAllowlistPermitsOnlyTheTablesItNames(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "orders\nscratch"})
	allowed(t, g, "SELECT * FROM orders")
	for _, sql := range []string{
		"SELECT * FROM customers",
		"SELECT * FROM customer_view",
		"SELECT * FROM secret_keys",
		"SELECT o.id FROM orders o JOIN customers c ON c.id = o.customer_id",
	} {
		refused(t, g, sql)
	}
}

func TestOneToolReachesOneDatabase(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT * FROM otherdb.customers",
		"SELECT * FROM mysql.user",
		"SELECT * FROM performance_schema.events_statements_history",
		"SELECT * FROM sys.schema_table_statistics",
		"SELECT otherdb.customers.email FROM customers",
	} {
		refused(t, g, sql)
	}
	// Its own database named in full is the same database.
	allowed(t, g, "SELECT id FROM shop.orders")
}

// ------------------------------------------------------------ statement shapes

func TestOnlyStatementsTheGuardCanReadEndToEndAreRun(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		// Built somewhere this side cannot see.
		"PREPARE s FROM 'SELECT * FROM secret_keys'",
		"EXECUTE s",
		// Bodies this side cannot see.
		"CALL p()",
		"CREATE PROCEDURE p() BEGIN SELECT 1; END",
		"CREATE VIEW v AS SELECT * FROM secret_keys",
		// Ways to move a policy out from under itself.
		"RENAME TABLE customers TO c2",
		"ALTER TABLE customers CHANGE ssn notssn VARCHAR(32)",
		"DROP TABLE customers",
		"CREATE TABLE x AS SELECT * FROM secret_keys",
		"GRANT ALL ON *.* TO 'x'",
		// State that outlives one call, and the file system.
		"SET @a = 1",
		"SELECT @v := ssn FROM customers",
		"USE otherdb",
		"DO (SELECT 1)",
		"LOAD DATA INFILE '/etc/passwd' INTO TABLE scratch",
		"SELECT LOAD_FILE('/etc/passwd')",
		"SELECT * FROM orders INTO OUTFILE '/tmp/x'",
		// Two statements in one call, and something that is not a statement.
		"SELECT 1; SELECT * FROM secret_keys",
		"SELEC id FROM orders",
		"",
		"   ",
		// A plan that runs what it explains.
		"EXPLAIN ANALYZE SELECT id FROM orders",
		// The short form of a select. It is not written back as one, so a select
		// list this masked a column in would be dropped on the way out.
		"TABLE customers",
		"TABLE secret_keys",
	} {
		refused(t, g, sql)
	}
}

func TestTheReadableShowsAreAllowedAndTheRestAreNot(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{"SHOW TABLES", "SHOW FULL TABLES", "SHOW TABLES FROM shop", "SHOW TABLE STATUS"} {
		d := allowed(t, g, sql)
		if !d.ListsTables {
			t.Errorf("%s: the rows must be filtered before they are returned", sql)
		}
		if d.Rewritten {
			t.Errorf("%s: nothing about it needed rewriting", sql)
		}
	}
	for _, sql := range []string{"SHOW COLUMNS FROM orders", "SHOW INDEX FROM orders", "SHOW CREATE TABLE orders", "DESCRIBE orders"} {
		d := allowed(t, g, sql)
		if d.ListsTables {
			t.Errorf("%s: it does not answer with a list of tables", sql)
		}
	}
	for _, sql := range []string{
		"SHOW DATABASES",
		"SHOW VARIABLES",
		"SHOW PROCESSLIST",
		"SHOW GRANTS",
		"SHOW WARNINGS",
		"SHOW TRIGGERS",
		"SHOW PROCEDURE STATUS",
		"SHOW TABLES FROM otherdb",
		// A view's definition names the tables underneath it.
		"SHOW CREATE VIEW customer_view",
		"SHOW CREATE TABLE customer_view",
	} {
		refused(t, g, sql)
	}
}

// --------------------------------------------------------------------- hiding

func TestAHiddenColumnComesBackHidden(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT ssn FROM customers",
		"SELECT '[hidden]' AS `ssn` FROM `customers`")
	rewritten(t, g, "SELECT c.ssn FROM customers c",
		"SELECT '[hidden]' AS `ssn` FROM `customers` AS `c`")
	rewritten(t, g, "SELECT id, ssn AS social FROM customers",
		"SELECT `id`,'[hidden]' AS `social` FROM `customers`")
	rewritten(t, g, "SELECT ssn FROM customers UNION ALL SELECT ssn FROM customers",
		"SELECT '[hidden]' AS `ssn` FROM `customers` UNION ALL SELECT '[hidden]' AS `ssn` FROM `customers`")
	rewritten(t, g, "WITH c AS (SELECT ssn FROM customers) SELECT * FROM c",
		"WITH `c` AS (SELECT '[hidden]' AS `ssn` FROM `customers`) SELECT * FROM `c`")
	// A subquery that projects it is masked where it is read, so what the outer
	// query gets hold of is already the hidden value.
	rewritten(t, g, "SELECT d.ssn FROM (SELECT ssn FROM customers) d",
		"SELECT `d`.`ssn` FROM (SELECT '[hidden]' AS `ssn` FROM `customers`) AS `d`")
	rewritten(t, g, "SELECT (SELECT ssn FROM customers LIMIT 1) AS s",
		"SELECT (SELECT '[hidden]' AS `ssn` FROM `customers` LIMIT 1) AS `s`")
}

func TestAStarBecomesTheColumnsItStandsFor(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT * FROM customers",
		"SELECT `customers`.`id`,`customers`.`email`,'[hidden]' AS `ssn`,`customers`.`profile` FROM `customers`")
	rewritten(t, g, "SELECT c.* FROM customers c",
		"SELECT `c`.`id`,`c`.`email`,'[hidden]' AS `ssn`,`c`.`profile` FROM `customers` AS `c`")
	rewritten(t, g, "SELECT c.*, o.total FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT `c`.`id`,`c`.`email`,'[hidden]' AS `ssn`,`c`.`profile`,`o`.`total` FROM `customers` AS `c` JOIN `orders` AS `o` ON `o`.`customer_id`=`c`.`id`")
	// A star over both tables expands both, because one of them has something to
	// hide and a star cannot be half expanded.
	rewritten(t, g, "SELECT * FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT `c`.`id`,`c`.`email`,'[hidden]' AS `ssn`,`c`.`profile`,`o`.`id`,`o`.`customer_id`,`o`.`total`,`o`.`note` FROM `customers` AS `c` JOIN `orders` AS `o` ON `o`.`customer_id`=`c`.`id`")
	// Through a view, whose own columns are what the view declares.
	rewritten(t, g, "SELECT * FROM customer_view",
		"SELECT `customer_view`.`id`,`customer_view`.`email`,'[hidden]' AS `ssn` FROM `customer_view`")
	// A star over a subquery needs no expanding of its own: the subquery's star
	// was expanded where the columns were actually read, so what the outer star
	// stands for is already the hidden value.
	rewritten(t, g, "SELECT * FROM (SELECT * FROM customers) d",
		"SELECT * FROM (SELECT `customers`.`id`,`customers`.`email`,'[hidden]' AS `ssn`,`customers`.`profile` FROM `customers`) AS `d`")
}

func TestAQueryWithNothingToHideIsSentOnExactlyAsItWasWritten(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT * FROM orders",
		"SELECT o.* FROM orders o WHERE o.total > 10 ORDER BY o.id LIMIT 5 OFFSET 2",
		"SELECT COUNT(*) FROM orders WHERE note LIKE 'a%'",
		"SELECT id, email FROM customers WHERE id = ?",
		"WITH recent AS (SELECT id FROM orders) SELECT * FROM recent",
		"SELECT /*+ MAX_EXECUTION_TIME(1000) */ id FROM orders FOR UPDATE",
	} {
		d := allowed(t, g, sql)
		if d.Rewritten || d.SQL != sql {
			t.Errorf("%s\n  came back as %s", sql, d.SQL)
		}
	}
}

// A hidden field may be USED anywhere. What is hidden is the value, and a value
// is given away by being returned, so that is the one place it is taken out.
//
// The cost is deliberate: a condition over a hidden field answers a question
// about it, so somebody who asks enough questions can work the value out.
// Keeping it out of the answer is what this promises; making it unknowable is
// not, and would mean refusing most of the queries anybody wants to run.
func TestAHiddenFieldIsUsableEverywhereAndReturnedNowhere(t *testing.T) {
	g := standard(t)

	// Used to decide, to order, to group, to join. Nothing returned, nothing
	// rewritten: these go to the database exactly as they were written.
	for _, sql := range []string{
		"SELECT id FROM customers WHERE ssn LIKE '123%'",
		"SELECT id FROM customers WHERE SUBSTRING(ssn,1,1) = '1'",
		"SELECT COUNT(*) FROM customers WHERE ssn IS NOT NULL",
		"SELECT id FROM customers ORDER BY ssn",
		"SELECT id FROM customers GROUP BY id HAVING MAX(ssn) LIKE 'a%'",
		"SELECT c.id FROM customers c JOIN staff s ON s.ssn = c.ssn",
		"SELECT id FROM customers WHERE ssn REGEXP '^1'",
		"SELECT (SELECT COUNT(*) FROM customers WHERE ssn LIKE '1%') AS c",
	} {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: nothing is returned here, so nothing needed changing, got %s", sql, d.SQL)
		}
	}

	// Returned, in any shape: the whole select item is replaced, not only a bare
	// column, because an expression would otherwise hand the value back with a
	// little arithmetic wrapped round it.
	for _, sql := range []string{
		"SELECT ssn FROM customers",
		"SELECT CONCAT(ssn, '') FROM customers",
		"SELECT IFNULL(ssn, '') FROM customers",
		"SELECT CASE WHEN ssn IS NULL THEN 'x' ELSE ssn END FROM customers",
		"SELECT GROUP_CONCAT(ssn) FROM customers",
		"SELECT JSON_ARRAYAGG(ssn) FROM customers",
		"SELECT FIRST_VALUE(ssn) OVER (ORDER BY id) FROM customers",
		"SELECT HEX(ssn) FROM customers",
		"SELECT DEFAULT(ssn) FROM customers",
	} {
		d := allowed(t, g, sql)
		if !d.Rewritten {
			t.Errorf("%s: it is being returned, so it must be taken out: %s", sql, d.SQL)
			continue
		}
		if reads(t, d.SQL, "ssn") {
			t.Errorf("%s: the column is still read by what runs: %s", sql, d.SQL)
		}
		if !strings.Contains(d.SQL, "'"+sqlguard.Hidden+"'") {
			t.Errorf("%s: nothing stands in for it: %s", sql, d.SQL)
		}
	}
}

func TestAHiddenValueIsReplacedBeforeAnythingElseCanReadIt(t *testing.T) {
	// A subquery that selects the column plainly is allowed, because selecting it
	// plainly is what hiding it means. What matters is that the statement around
	// it never gets hold of the value: these are the two shapes that carry a value
	// out through the error message rather than the result, and both of them find
	// the stand-in there instead.
	g := standard(t)
	for _, sql := range []string{
		"SELECT EXTRACTVALUE(1, CONCAT(0x7e, (SELECT ssn FROM customers LIMIT 1)))",
		"SELECT CAST((SELECT ssn FROM customers LIMIT 1) AS UNSIGNED)",
		"SELECT LENGTH((SELECT ssn FROM customers LIMIT 1))",
	} {
		d := allowed(t, g, sql)
		if !strings.Contains(d.SQL, "'"+sqlguard.Hidden+"' AS `ssn`") {
			t.Errorf("%s: the value must be replaced where it is read, got %s", sql, d.SQL)
		}
		// The only ssn left in the statement is the name the stand-in wears.
		if strings.Count(d.SQL, "`ssn`") != strings.Count(d.SQL, "AS `ssn`") {
			t.Errorf("%s: the column is still read into the expression: %s", sql, d.SQL)
		}
	}
}

// A write reads a hidden field the same way anything else does: the rows going
// in are a select list, and a select list is where the stand-in goes. What ends
// up in the other table is [hidden], which is the administrator's business and
// not this tool's.
//
// Two things are still refused, and neither is about the policy. Writing INTO a
// hidden field would put the stand-in over whatever is really there, and a SET
// has no select list to put a stand-in in, so reading one there would copy the
// real value into a column that can be read back afterwards.
func TestAWriteReadsAHiddenFieldLikeAnythingElse(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"})

	for _, c := range []struct{ sql, want string }{
		{"INSERT INTO scratch (note) SELECT ssn FROM customers",
			"INSERT INTO `scratch` (`note`) SELECT '[hidden]' AS `ssn` FROM `customers`"},
		{"INSERT INTO scratch (note) SELECT CONCAT(ssn,'') FROM customers",
			"INSERT INTO `scratch` (`note`) SELECT '[hidden]' AS `CONCAT(``ssn``, _UTF8MB4'')` FROM `customers`"},
	} {
		rewritten(t, g, c.sql, c.want)
	}
	// Reading it to decide which rows to change returns nothing, so it is left
	// alone.
	if d := allowed(t, g, "DELETE FROM customers WHERE ssn = '1'"); d.Rewritten {
		t.Errorf("a condition returns nothing: %s", d.SQL)
	}

	// Written INTO: refused, because this tool cannot know what is really there.
	for _, sql := range []string{
		"UPDATE customers SET ssn = 'x'",
		"INSERT INTO customers (id, ssn) VALUES (2, 'x')",
		"INSERT INTO customers VALUES (2,'b','x','p')",
		"INSERT INTO customers SELECT * FROM customers",
	} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "written to") && !strings.Contains(reason, "column list") {
			t.Errorf("%s: refused for the wrong reason: %q", sql, reason)
		}
	}
	// Copied out through a SET, where there is no select list to hide it in.
	reason := refused(t, g, "UPDATE customers SET email = ssn")
	if !strings.Contains(reason, "copied") {
		t.Errorf("refused for the wrong reason: %q", reason)
	}

	// A write that goes nowhere near it is untouched.
	allowed(t, g, "UPDATE customers SET email = ? WHERE id = ?")
	allowed(t, g, "INSERT INTO scratch (note) SELECT email FROM customers")
}

func TestAHiddenColumnIsResolvedToItsOwnTable(t *testing.T) {
	g := standard(t)
	// staff has an ssn too, and the rule named customers: staff's is untouched,
	// and the statement goes on exactly as it was written.
	for _, sql := range []string{
		"SELECT ssn FROM staff",
		"SELECT id FROM staff WHERE ssn = ?",
		"SELECT s.ssn FROM staff s JOIN customers c ON c.id = s.id",
	} {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: staff's ssn is not the hidden one, got %s", sql, d.SQL)
		}
	}
	rewritten(t, g, "SELECT c.ssn FROM staff s JOIN customers c ON c.id = s.id",
		"SELECT '[hidden]' AS `ssn` FROM `staff` AS `s` JOIN `customers` AS `c` ON `c`.`id`=`s`.`id`")
	// Unqualified, with two tables in scope that both have one: which it means
	// cannot be established, so being returned it is hidden. Being wrong towards
	// hiding costs a column in a report; being wrong the other way costs the
	// value.
	rewritten(t, g, "SELECT ssn FROM staff s JOIN customers c ON c.id = s.id",
		"SELECT '[hidden]' AS `ssn` FROM `staff` AS `s` JOIN `customers` AS `c` ON `c`.`id`=`s`.`id`")
	// Used rather than returned, the same uncertainty costs nothing at all.
	if d := allowed(t, g, "SELECT s.id FROM staff s JOIN customers c ON c.id = s.id WHERE ssn = ?"); d.Rewritten {
		t.Errorf("nothing is returned here: %s", d.SQL)
	}
	// A table joined to itself is one table, and one answer.
	rewritten(t, g, "SELECT a.ssn FROM customers a JOIN customers b ON b.id = a.id",
		"SELECT '[hidden]' AS `ssn` FROM `customers` AS `a` JOIN `customers` AS `b` ON `b`.`id`=`a`.`id`")
	// A qualifier that is not in the statement at all: unresolvable, and being
	// returned, so hidden. The database has its own opinion about nosuch.
	rewritten(t, g, "SELECT nosuch.ssn FROM customers", "SELECT '[hidden]' AS `ssn` FROM `customers`")
}

func TestTheWildcardHidesAColumnWhereverItIs(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "*.ssn\norders.note"})
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS `ssn` FROM `customers`")
	rewritten(t, g, "SELECT ssn FROM staff", "SELECT '[hidden]' AS `ssn` FROM `staff`")
	// Unqualified with two tables in scope no longer has to be resolved at all:
	// the rule says the column is hidden whichever of them it came from.
	rewritten(t, g, "SELECT ssn FROM staff s JOIN customers c ON c.id = s.id",
		"SELECT '[hidden]' AS `ssn` FROM `staff` AS `s` JOIN `customers` AS `c` ON `c`.`id`=`s`.`id`")
	if d := allowed(t, g, "SELECT id FROM staff WHERE ssn = ?"); d.Rewritten {
		t.Errorf("a condition returns nothing, so nothing needed changing: %s", d.SQL)
	}
	// A rule written with a table stays scoped to it, alongside one written
	// without.
	rewritten(t, g, "SELECT note FROM orders", "SELECT '[hidden]' AS `note` FROM `orders`")
	if d := allowed(t, g, "SELECT note FROM scratch"); d.Rewritten {
		t.Errorf("scratch's note is not the hidden one, got %s", d.SQL)
	}
}

func TestAWholeTableCanBeHiddenColumnByColumn(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "customers.*"})
	rewritten(t, g, "SELECT * FROM customers",
		"SELECT '[hidden]' AS `id`,'[hidden]' AS `email`,'[hidden]' AS `ssn`,'[hidden]' AS `profile` FROM `customers`")
	rewritten(t, g, "SELECT id FROM customers WHERE email = ?",
		"SELECT '[hidden]' AS `id` FROM `customers` WHERE `email`=?")
	// A count is about rows, not values, and comes back whole.
	if d := allowed(t, g, "SELECT COUNT(*) FROM customers"); d.Rewritten {
		t.Errorf("a count returns no value of the table: %s", d.SQL)
	}
}

// ------------------------------------------------------------------- catalog

func TestACatalogReadIsNarrowedToWhatTheToolMaySee(t *testing.T) {
	g := standard(t)
	rewritten(t, g, "SELECT table_name FROM information_schema.tables",
		"SELECT `table_name` FROM `information_schema`.`tables` "+
			"WHERE `tables`.`table_schema`='shop' AND `tables`.`table_name` NOT IN ('secret_keys','leaky_view','nested_view','unreadable_view')")
	// An existing condition is kept, and kept whole.
	rewritten(t, g, "SELECT table_name FROM information_schema.tables t WHERE t.table_type = 'BASE TABLE' OR t.table_rows > 0",
		"SELECT `table_name` FROM `information_schema`.`tables` AS `t` "+
			"WHERE (`t`.`table_type`=_UTF8MB4'BASE TABLE' OR `t`.`table_rows`>0) "+
			"AND `t`.`table_schema`='shop' AND `t`.`table_name` NOT IN ('secret_keys','leaky_view','nested_view','unreadable_view')")
	// Columns are narrowed by the table they belong to.
	d := allowed(t, g, "SELECT column_name FROM information_schema.columns WHERE table_name = 'secret_keys'")
	if !strings.Contains(d.SQL, "NOT IN ('secret_keys'") {
		t.Fatalf("a column read must be narrowed too: %s", d.SQL)
	}
	// A foreign key names the table it points at.
	d = allowed(t, g, "SELECT referenced_table_name FROM information_schema.key_column_usage")
	if !strings.Contains(d.SQL, "`key_column_usage`.`referenced_table_name` IS NULL OR") {
		t.Fatalf("a foreign key's other end must be narrowed too: %s", d.SQL)
	}
	// The parts of the catalog that hold definitions are not readable at all.
	for _, sql := range []string{
		"SELECT view_definition FROM information_schema.views",
		"SELECT routine_definition FROM information_schema.routines",
		"SELECT * FROM information_schema.triggers",
		"SELECT * FROM information_schema.processlist",
		"SELECT * FROM information_schema.innodb_tables",
		"SELECT * FROM information_schema.files",
		"SELECT * FROM information_schema.column_statistics",
	} {
		refused(t, g, sql)
	}
}

func TestAnAllowlistNarrowsACatalogReadToTheNamesItPermits(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "orders\nscratch"})
	rewritten(t, g, "SELECT table_name FROM information_schema.tables",
		"SELECT `table_name` FROM `information_schema`.`tables` "+
			"WHERE `tables`.`table_schema`='shop' AND `tables`.`table_name` IN ('orders','scratch')")
}

func TestAnAllowlistThatMatchesNothingAnswersWithNothing(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "nosuch_table"})
	d := allowed(t, g, "SELECT table_name FROM information_schema.tables")
	if !strings.Contains(d.SQL, "0=1") {
		t.Fatalf("expected a condition nothing satisfies, got %s", d.SQL)
	}
}

// --------------------------------------------------------------- row filtering

func TestWhichTablesAreShown(t *testing.T) {
	g := standard(t)
	for _, name := range []string{"customers", "orders", "customer_view"} {
		if !g.Shows(name) {
			t.Errorf("%s should be listed", name)
		}
	}
	for _, name := range []string{"secret_keys", "leaky_view", "nested_view", "unreadable_view"} {
		if g.Shows(name) {
			t.Errorf("%s must not be listed", name)
		}
	}
	// A table made since the snapshot was taken is read the way the policy reads
	// any name it has not been told about.
	if !g.Shows("brand_new") {
		t.Error("a denylist has not denied a table it never named")
	}
	strict := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeAllowlist, Tables: "orders"})
	if strict.Shows("brand_new") {
		t.Error("an allowlist has not permitted a table it never named")
	}
}

func TestVisibleAndHiddenTables(t *testing.T) {
	g := standard(t)
	if got := strings.Join(g.HiddenTables(), ","); got != "secret_keys,leaky_view,nested_view,unreadable_view" {
		t.Fatalf("hidden = %q", got)
	}
	if got := strings.Join(g.VisibleTables(), ","); got != "customers,orders,staff,scratch,customer_view" {
		t.Fatalf("visible = %q", got)
	}
}

// -------------------------------------------------------------------- dialects

func TestAPolicyOnADialectThisBuildCannotReadIsRefused(t *testing.T) {
	// A policy that cannot be enforced is never quietly ignored.
	_, err := sqlguard.New("postgres", sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys"}, sqlguard.NewCatalog("mysql", "shop", nil))
	if err == nil {
		t.Fatal("expected a dialect with no analyzer to refuse to guard")
	}
	if !sqlguard.Supports("mysql") {
		t.Fatal("mysql must be supported")
	}
}

func TestGuardRefusesToBuildOnABrokenPolicyOrNoCatalog(t *testing.T) {
	if _, err := sqlguard.New("mysql", sqlguard.Policy{TableMode: sqlguard.ModeAllowlist}, sqlguard.NewCatalog("mysql", "shop", nil)); err == nil {
		t.Fatal("an allowlist naming nothing is not a policy")
	}
	if _, err := sqlguard.New("mysql", sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "x"}, nil); err == nil {
		t.Fatal("without the database's tables there is nothing to enforce against")
	}
}

// The reader that takes a statement apart keeps the state of the statement it is
// reading, and two turns can be in this package at the same moment: one agent
// asking a question while a background agent asks another. Sharing one
// reader between them produces wrong answers rather than a crash, which is the
// worst way for a check like this to fail.
func TestManyStatementsAtOnce(t *testing.T) {
	g := standard(t)
	const workers, each = 8, 40

	done := make(chan string, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			for i := 0; i < each; i++ {
				if d, reason, err := g.Check("SELECT ssn FROM customers"); err != nil || reason != "" ||
					d.SQL != "SELECT '[hidden]' AS `ssn` FROM `customers`" {
					done <- "hidden column: " + d.SQL + " " + reason
					return
				}
				if _, reason, _ := g.Check("SELECT * FROM secret_keys"); reason == "" {
					done <- "a denied table was permitted"
					return
				}
				if d, reason, err := g.Check("SELECT id, total FROM orders WHERE id = ?"); err != nil || reason != "" || d.Rewritten {
					done <- "untouched query: " + d.SQL + " " + reason
					return
				}
			}
			done <- ""
		}(w)
	}
	for w := 0; w < workers; w++ {
		if failure := <-done; failure != "" {
			t.Fatal(failure)
		}
	}
}

// A subquery is the obvious way to carry a value up to where nothing is looking:
// read the column deep down, hand it upward under another name, and select it at
// the top. It does not work here, because hiding happens where the column is
// READ rather than where the result is printed. Whatever travels upward is
// already the stand-in, however many levels it passes through and whatever each
// level decides to call it.
func TestAHiddenValueCannotBeCarriedUpThroughSubqueries(t *testing.T) {
	g := standard(t)
	for _, c := range []struct{ sql, want string }{
		{"SELECT x FROM (SELECT ssn AS x FROM customers) d",
			"SELECT `x` FROM (SELECT '[hidden]' AS `x` FROM `customers`) AS `d`"},
		{"SELECT MAX(x) FROM (SELECT ssn AS x FROM customers) d",
			"SELECT MAX(`x`) FROM (SELECT '[hidden]' AS `x` FROM `customers`) AS `d`"},
		{"SELECT * FROM (SELECT ssn AS x FROM customers) d",
			"SELECT * FROM (SELECT '[hidden]' AS `x` FROM `customers`) AS `d`"},
		{"WITH a AS (SELECT ssn FROM customers), b AS (SELECT ssn AS y FROM a) SELECT y FROM b",
			"WITH `a` AS (SELECT '[hidden]' AS `ssn` FROM `customers`), `b` AS (SELECT `ssn` AS `y` FROM `a`) SELECT `y` FROM `b`"},
		{"SELECT d.s FROM orders o JOIN (SELECT ssn AS s FROM customers) d ON d.s = o.note",
			"SELECT `d`.`s` FROM `orders` AS `o` JOIN (SELECT '[hidden]' AS `s` FROM `customers`) AS `d` ON `d`.`s`=`o`.`note`"},
	} {
		rewritten(t, g, c.sql, c.want)
	}
	// Made into a table of its own, which this tool does not do at all.
	refused(t, g, "CREATE TEMPORARY TABLE t AS SELECT ssn FROM customers")
	// Copied into another table, where what lands is the stand-in.
	rewritten(t, g, "INSERT INTO scratch (note) SELECT x FROM (SELECT ssn AS x FROM customers) d",
		"INSERT INTO `scratch` (`note`) SELECT `x` FROM (SELECT '[hidden]' AS `x` FROM `customers`) AS `d`")
}

// A field allowlist reads the other way round: what it names is what comes back,
// for the tables it names, and everything else about those tables is hidden.
func TestAFieldAllowlistShowsOnlyWhatItNames(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeAllowlist, Fields: "customers.id\ncustomers.email"})
	rewritten(t, g, "SELECT * FROM customers",
		"SELECT `customers`.`id`,`customers`.`email`,'[hidden]' AS `ssn`,'[hidden]' AS `profile` FROM `customers`")
	rewritten(t, g, "SELECT ssn FROM customers", "SELECT '[hidden]' AS `ssn` FROM `customers`")
	if d := allowed(t, g, "SELECT id FROM customers WHERE profile IS NOT NULL"); d.Rewritten {
		t.Errorf("id is on the list and profile is only a condition: %s", d.SQL)
	}
	rewritten(t, g, "SELECT profile FROM customers", "SELECT '[hidden]' AS `profile` FROM `customers`")
	// What it names is untouched, and a table it never mentions keeps everything.
	for _, sql := range []string{
		"SELECT id, email FROM customers WHERE id = ?",
		"SELECT * FROM orders",
		"SELECT o.note FROM orders o ORDER BY o.total",
	} {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: nothing here needed hiding, got %s", sql, d.SQL)
		}
	}
}

// The one attack that beats reading the statement, because the two readers
// disagree by design: a comment this side treats as a comment and the server
// runs. Found by an audit of the built code, not of the design.
//
// It is taken out rather than refused, so there is one text, and it is the text
// that runs.
func TestAStatementTheServerWouldReadDifferentlyIsCleanedFirst(t *testing.T) {
	g := standard(t)
	for _, c := range []struct{ sql, want string }{
		// MariaDB runs what is inside /*M! ... */ and nothing else does. What is
		// left is what this side always thought the statement was.
		{"SELECT id FROM orders /*M!100000 UNION SELECT ssn FROM customers */",
			"SELECT id FROM orders  "},
		{"SELECT id FROM orders /*M!100000 UNION SELECT value FROM secret_keys */",
			"SELECT id FROM orders  "},
		{"SELECT /*M!100000 ssn, */ id FROM customers",
			"SELECT   id FROM customers"},
		// The MySQL form, version-gated, goes the same way: what it does depends
		// on which server reads it, and this leaves nothing to depend on.
		{"SELECT id FROM orders /*!50000 UNION SELECT total FROM orders */",
			"SELECT id FROM orders  "},
		{"SELECT id FROM orders /*!99999 UNION SELECT ssn FROM customers */",
			"SELECT id FROM orders  "},
		// The shape a database dump is full of.
		{"/*!40101 SET NAMES utf8 */ SELECT id FROM orders", "  SELECT id FROM orders"},
	} {
		d := allowed(t, g, c.sql)
		if d.SQL != c.want {
			t.Errorf("%s\n  ran  %q\n  want %q", c.sql, d.SQL, c.want)
		}
		if !d.Rewritten {
			t.Errorf("%s: the statement that ran is not the one submitted, and must say so", c.sql)
		}
	}

	// What the comment held is gone from what runs, which is the whole point.
	d := allowed(t, g, "SELECT id FROM orders /*M!100000 UNION SELECT ssn FROM customers */")
	for _, gone := range []string{"UNION", "ssn", "customers"} {
		if strings.Contains(d.SQL, gone) {
			t.Errorf("%q survived into what runs: %s", gone, d.SQL)
		}
	}

	// A statement that merely contains the characters in a value keeps them: the
	// scan is quote-aware, and mangling this would be mangling real work.
	for _, sql := range []string{
		"SELECT id FROM orders WHERE note = '/*M!100000 x */'",
		"SELECT id FROM orders WHERE note = \"/*!50000 x */\"",
		"SELECT id FROM orders WHERE note = '/*!'",
		"SELECT id FROM orders /* just a note */",
		"SELECT /*+ MAX_EXECUTION_TIME(1000) */ id FROM orders",
	} {
		if d := allowed(t, g, sql); d.SQL != sql {
			t.Errorf("%s\n  was changed to %s", sql, d.SQL)
		}
	}

	// Once the comment is gone, what is left is checked like anything else, so a
	// statement that hid a denied table inside one is refused on its own terms.
	refused(t, g, "SELECT value FROM /*M!100000 secret_keys */ secret_keys")
	// And one that is nothing but a comment has nothing left to run.
	refused(t, g, "/*!50000 SELECT ssn FROM customers */")
}

// A hidden field may be joined on. An administrator hides what a value IS;
// which rows line up with which is a different question, and joining two tables
// on an identifier neither side may print is an ordinary thing to want.
//
// The cost is deliberate: somebody who joins against a value they supply learns
// whether they guessed right, so a hidden value can still be worked out a guess
// at a time. Hiding it in the answer is what this promises; making it unknowable
// is not, and the join is where that line was drawn.
func TestAHiddenFieldMayBeJoinedOn(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT c.id FROM customers c JOIN staff s ON s.ssn = c.ssn",
		"SELECT c.email FROM customers c LEFT JOIN staff s ON s.ssn = c.ssn",
		"SELECT c.id FROM customers c JOIN staff s USING (ssn)",
		"SELECT c.id FROM customers c NATURAL JOIN staff s",
		"SELECT COUNT(*) FROM customers c JOIN staff s ON s.ssn = c.ssn",
	} {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: a join needs no rewriting, got %s", sql, d.SQL)
		}
	}
	// Selecting it is still where the hiding happens.
	rewritten(t, g, "SELECT c.ssn FROM customers c JOIN staff s ON s.ssn = c.ssn",
		"SELECT '[hidden]' AS `ssn` FROM `customers` AS `c` JOIN `staff` AS `s` ON `s`.`ssn`=`c`.`ssn`")
}

// The short form of a select is not written back as one, so what this decided
// about is not what would run.
func TestTheShortFormOfASelectIsRefused(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{"TABLE customers", "TABLE orders", "VALUES ROW(1,2)"} {
		reason := refused(t, g, sql)
		if !strings.Contains(reason, "short form") {
			t.Errorf("%s: refused for the wrong reason: %q", sql, reason)
		}
	}
}

// A write must never have a hidden field rewritten INTO it: that would store the
// stand-in over the real value, permanently, with the control doing the damage.
func TestAWriteIsNeverMaskedIntoTheTable(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"})
	for _, sql := range []string{
		"INSERT INTO customers SELECT * FROM customers",
		"REPLACE INTO customers SELECT * FROM customers WHERE id = 1",
		"INSERT INTO customers SELECT c.* FROM customers c",
	} {
		reason := refused(t, g, sql)
		if strings.Contains(reason, sqlguard.Hidden) {
			t.Errorf("%s: the refusal should not offer the stand-in as an outcome: %q", sql, reason)
		}
	}
	// A write that names its columns and leaves the hidden one out is fine.
	allowed(t, g, "INSERT INTO customers (id, email) SELECT id, email FROM customers")
}

// An INSERT with no column list writes every column of the table, including one
// the policy hides, without naming it anywhere for the walk to have seen.
func TestAnInsertWithNoColumnListIsDecidedByTheTable(t *testing.T) {
	g := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"})
	reason := refused(t, g, "INSERT INTO customers VALUES (2,'b','ATTACKER','p')")
	if !strings.Contains(reason, "column list") {
		t.Errorf("expected it to say what to do about it, got %q", reason)
	}
	// Naming them is the way to say what you mean, and works.
	allowed(t, g, "INSERT INTO customers (id, email) VALUES (2, 'b')")
	// A table with nothing hidden in it is unaffected.
	allowed(t, g, "INSERT INTO orders VALUES (9, 1, 5.00, 'note')")
}

// DEFAULT(col) names a column the walk does not descend into, so it is collected
// by hand or it is not collected at all.
func TestDefaultOfAHiddenFieldIsSeen(t *testing.T) {
	g := standard(t)
	// Returned, so taken out.
	for _, sql := range []string{
		"SELECT DEFAULT(ssn) FROM customers",
		"SELECT DEFAULT(c.ssn) FROM customers c",
	} {
		d := allowed(t, g, sql)
		if !d.Rewritten || !strings.Contains(d.SQL, "'"+sqlguard.Hidden+"'") {
			t.Errorf("%s: the declared default of a hidden field is still a value of it: %s", sql, d.SQL)
		}
	}
	// Used rather than returned, and a field nobody hid.
	if d := allowed(t, g, "SELECT id FROM customers WHERE ssn = DEFAULT(ssn)"); d.Rewritten {
		t.Errorf("a condition returns nothing: %s", d.SQL)
	}
	if d := allowed(t, g, "SELECT DEFAULT(email) FROM customers"); d.Rewritten {
		t.Errorf("email is not hidden: %s", d.SQL)
	}
}

// Two sources answering to one name must not evict each other: the second was
// quietly replacing the first, and a read of the catalog then went unnarrowed.
func TestASecondSourceCannotEvictTheCatalogFromScope(t *testing.T) {
	g := standard(t)
	d := allowed(t, g, "SELECT table_name FROM information_schema.tables, orders AS tables")
	if !strings.Contains(d.SQL, "NOT IN") || !strings.Contains(d.SQL, "table_schema") {
		t.Fatalf("the catalog read was not narrowed: %s", d.SQL)
	}

	// Returned AND used in the same statement: the rows are still grouped by the
	// real value, and what comes back for it is the stand-in. Both halves of the
	// rule at once.
	rewritten(t, g, "SELECT ssn, COUNT(*) FROM customers GROUP BY ssn",
		"SELECT '[hidden]' AS `ssn`,COUNT(1) FROM `customers` GROUP BY `ssn`")
}

// reads reports whether a statement still reads a column of this name anywhere.
// It re-parses what would run, because the NAME of a replaced item keeps the
// text it was written as ("CONCAT(ssn,”)"), and a name is not a read.
func reads(t *testing.T, sql, column string) bool {
	t.Helper()
	stmt, reason := readStatement(sql)
	if reason != "" {
		t.Fatalf("what would run does not parse: %s", reason)
	}
	finder := &columnFinder{want: strings.ToLower(column)}
	stmt.Accept(finder)
	return finder.found
}

type columnFinder struct {
	want  string
	found bool
}

func (c *columnFinder) Enter(n ast.Node) (ast.Node, bool) {
	if name, ok := n.(*ast.ColumnName); ok && strings.EqualFold(name.Name.O, c.want) {
		c.found = true
	}
	return n, false
}
func (c *columnFinder) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// Which table an unqualified column belongs to, worked out from the statement
// and the catalog rather than guessed at. Every shape a column can arrive in.
func TestAnUnqualifiedColumnIsResolvedFromTheTree(t *testing.T) {
	// customers has ssn and is hidden; staff has an ssn of its own that is not.
	g := standard(t)

	hidden := []string{
		// One table in scope, and it is the one the rule names.
		"SELECT ssn FROM customers",
		// Several in scope, and only one of them has a column of that name.
		"SELECT ssn FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT ssn FROM orders o JOIN customers c ON o.customer_id = c.id",
		// Several have it, and they disagree about whether it is hidden. Nothing
		// can say which is meant (the database calls it ambiguous and refuses to
		// run it at all), so being returned, it is hidden.
		"SELECT ssn FROM staff s JOIN customers c ON c.id = s.id",
		// Read in an inner query, where the inner query's own FROM settles it.
		"SELECT (SELECT ssn FROM customers WHERE id = o.customer_id) AS s FROM orders o",
		// Read in an inner query that has no FROM of its own, so it belongs to
		// the query outside it.
		"SELECT (SELECT ssn) AS s FROM customers",
	}
	for _, sql := range hidden {
		d := allowed(t, g, sql)
		if !d.Rewritten || !strings.Contains(d.SQL, "'"+sqlguard.Hidden+"'") {
			t.Errorf("%s: should have resolved to the hidden one: %s", sql, d.SQL)
		}
	}

	untouched := []string{
		// The only table with that column is not the one the rule names.
		"SELECT ssn FROM staff",
		"SELECT ssn FROM staff s JOIN orders o ON o.id = s.id",
		// A column no rule mentions, however many tables are in scope.
		"SELECT email FROM customers c JOIN orders o ON o.customer_id = c.id",
		// Out of a subquery, where it was already decided at the source.
		"SELECT s FROM (SELECT email AS s FROM customers) d",
	}
	for _, sql := range untouched {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: nothing here is hidden: %s", sql, d.SQL)
		}
	}

	// And when both tables in scope hide it, there is no disagreement to resolve.
	both := guarded(t, sqlguard.Policy{FieldMode: sqlguard.ModeDenylist, Fields: "*.ssn"})
	rewritten(t, both, "SELECT ssn FROM staff s JOIN customers c ON c.id = s.id",
		"SELECT '[hidden]' AS `ssn` FROM `staff` AS `s` JOIN `customers` AS `c` ON `c`.`id`=`s`.`id`")
}

// The printer that writes a rewritten statement back out is a second
// implementation of the language, and it can write something that does not mean
// what was read. Both of these were found by running it.
func TestAStatementThatCannotBeRewrittenFaithfullyIsRefused(t *testing.T) {
	g := standard(t)

	// A backslash in a value: written back with the escaping gone, so the value
	// becomes a different one and the query quietly returns the wrong rows.
	reason := refused(t, g, `SELECT ssn, id FROM customers WHERE note = 'a\\b'`)
	if !strings.Contains(reason, "changing what") {
		t.Errorf("refused for the wrong reason: %q", reason)
	}
	// A function written back under a name the database does not have.
	reason = refused(t, g, "SELECT ssn, CHAR(65) AS a FROM customers")
	if !strings.Contains(reason, "CHAR_FUNC") {
		t.Errorf("refused for the wrong reason: %q", reason)
	}

	// The check costs nothing when nothing is rewritten: the same statements
	// without a hidden field never go near the printer.
	for _, sql := range []string{
		`SELECT id FROM customers WHERE note = 'a\\b'`,
		"SELECT CHAR(65) AS a FROM customers",
	} {
		if d := allowed(t, g, sql); d.Rewritten {
			t.Errorf("%s: nothing here needed rewriting: %s", sql, d.SQL)
		}
	}

	// And an ordinary value survives a rewrite untouched.
	rewritten(t, g, "SELECT ssn, id FROM customers WHERE email = 'o''brien'",
		"SELECT '[hidden]' AS `ssn`,`id` FROM `customers` WHERE `email`=_UTF8MB4'o''brien'")
}

// A table the snapshot has never heard of is reported, so a caller that can take
// a fresh snapshot knows to take one.
func TestAnUnknownTableIsReported(t *testing.T) {
	g := standard(t)
	d := allowed(t, g, "SELECT id FROM brand_new_table")
	if !d.SawUnknownTable {
		t.Fatal("a table this snapshot does not have must be reported, or a view made since it was taken is read as a table nobody kept back")
	}
	if d := allowed(t, g, "SELECT id FROM orders"); d.SawUnknownTable {
		t.Fatal("orders is in the snapshot")
	}
}
