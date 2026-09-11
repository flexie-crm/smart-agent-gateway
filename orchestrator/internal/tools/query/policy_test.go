package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/internal/tool"
	"github.com/go-sql-driver/mysql"
)

// The policy against a real database.
//
// Everything else about the guard is decided from a snapshot a test wrote by
// hand. This suite is the one that finds out whether the snapshot matches what a
// database actually says about itself: the order its columns come back in, and,
// above all, the form it stores a view's definition in, which is the text this
// has to read to know that a view's "code" column is somebody's ssn.
func fixture(t *testing.T) datasource.Config {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the policy suite")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("read the test DSN: %v", err)
	}
	host, portText, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portText)

	const database = "flexie_sag_policy_test"
	admin := datasource.Config{
		Driver: "mysql", Host: host, Port: port, Database: "information_schema",
		Username: parsed.User, Password: parsed.Passwd, TLS: datasource.TLS{Mode: "disable"},
	}
	run := func(cfg datasource.Config, statements ...string) {
		t.Helper()
		conn, err := datasource.Open(cfg)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = conn.Close() }()
		for _, statement := range statements {
			if _, err := conn.Exec(context.Background(), statement, nil); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}

	run(admin, "DROP DATABASE IF EXISTS "+database, "CREATE DATABASE "+database)
	t.Cleanup(func() { run(admin, "DROP DATABASE IF EXISTS "+database) })

	target := admin
	target.Database = database
	run(target,
		"CREATE TABLE customers (id INT PRIMARY KEY, email VARCHAR(100), ssn VARCHAR(20), profile VARCHAR(100))",
		"CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, total DECIMAL(10,2), note VARCHAR(100))",
		"CREATE TABLE secret_keys (id INT PRIMARY KEY, value VARCHAR(100))",
		// The two views this suite exists for: one that renames a hidden column,
		// and one that never names the table it reads.
		"CREATE VIEW customer_view AS SELECT id, email, ssn AS code FROM customers",
		"CREATE VIEW leaky_view AS SELECT id, value FROM secret_keys",
		"INSERT INTO customers VALUES (1, 'a@example.com', '123-45-6789', 'notes')",
		"INSERT INTO orders VALUES (1, 1, 9.99, 'first')",
		"INSERT INTO secret_keys VALUES (1, 'the-key')",
	)
	return target
}

// governed is the tool these tests run as: one table out of reach, one field
// hidden.
func governed(t *testing.T) Settings {
	t.Helper()
	return Settings{
		Connection: fixture(t),
		Access:     AccessRead,
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		},
	}
}

func rows(t *testing.T, settings Settings, sql string, args ...any) *datasource.Result {
	t.Helper()
	res, reason, err := run(context.Background(), settings, sql, args, 0)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if reason != "" {
		t.Fatalf("%s: refused with %q", sql, reason)
	}
	return res
}

func refusal(t *testing.T, settings Settings, sql string) string {
	t.Helper()
	res, reason, err := run(context.Background(), settings, sql, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if reason == "" {
		t.Fatalf("%s: expected a refusal, got %+v", sql, res)
	}
	return reason
}

// cell reads one value out of a result, by the column's name.
func cell(t *testing.T, res *datasource.Result, column string) any {
	t.Helper()
	if len(res.Rows) == 0 {
		t.Fatalf("no rows came back")
	}
	for i, name := range res.Columns {
		if strings.EqualFold(name, column) {
			return res.Rows[0][i]
		}
	}
	t.Fatalf("no column %q in %v", column, res.Columns)
	return nil
}

func TestAHiddenFieldNeverLeavesTheDatabase(t *testing.T) {
	settings := governed(t)

	// Selected plainly, it comes back as the stand-in, under its own name.
	res := rows(t, settings, "SELECT id, ssn FROM customers")
	if got := cell(t, res, "ssn"); got != sqlguard.Hidden {
		t.Fatalf("ssn came back as %v", got)
	}
	if got := cell(t, res, "id"); fmt.Sprint(got) != "1" {
		t.Fatalf("the rest of the row should be untouched, id = %v", got)
	}

	// A star over the table stands for its columns, with that one hidden.
	res = rows(t, settings, "SELECT * FROM customers")
	if got := cell(t, res, "ssn"); got != sqlguard.Hidden {
		t.Fatalf("* let the value through: %v", got)
	}
	if got := cell(t, res, "email"); got != "a@example.com" {
		t.Fatalf("* hid too much: email = %v", got)
	}

	// Through a view that calls it something else, the rule still holds. This is
	// the one that depends on reading the definition the database itself stored.
	res = rows(t, settings, "SELECT code FROM customer_view")
	if got := cell(t, res, "code"); got != sqlguard.Hidden {
		t.Fatalf("the view handed the value over as %v", got)
	}

	// It may be USED: to decide which rows come back, what order they come in,
	// what they join to. None of that returns it, and none of it is rewritten.
	for _, sql := range []string{
		"SELECT id FROM customers WHERE ssn LIKE '123%'",
		"SELECT id FROM customers ORDER BY ssn",
		"SELECT id FROM customer_view WHERE code LIKE '123%'",
	} {
		if res := rows(t, settings, sql); res.RowCount == 0 {
			t.Errorf("%s: came back empty, so it proved nothing", sql)
		}
	}
	// Wrapped in an expression it is still being returned, so the whole item goes.
	res = rows(t, settings, "SELECT CONCAT(ssn, '') AS c FROM customers")
	if got := cell(t, res, "c"); got != sqlguard.Hidden {
		t.Fatalf("an expression handed the value back as %v", got)
	}
}

func TestATableOutOfReachIsOutOfReachAndUnlisted(t *testing.T) {
	settings := governed(t)

	for _, sql := range []string{
		"SELECT * FROM secret_keys",
		"SELECT value FROM secret_keys",
		"SELECT o.id FROM orders o JOIN secret_keys k ON k.id = o.id",
		"SELECT id FROM orders WHERE id IN (SELECT id FROM secret_keys)",
		// Through the view that never names it.
		"SELECT * FROM leaky_view",
		"DESCRIBE secret_keys",
		"SHOW CREATE TABLE secret_keys",
	} {
		refusal(t, settings, sql)
	}

	// It is not in the answer when the tables are listed, and neither is the view
	// that reads it.
	listed := map[string]bool{}
	for _, row := range rows(t, settings, "SHOW TABLES").Rows {
		listed[fmt.Sprint(row[0])] = true
	}
	if !listed["customers"] || !listed["orders"] || !listed["customer_view"] {
		t.Fatalf("the tables it may see were left out: %v", listed)
	}
	if listed["secret_keys"] || listed["leaky_view"] {
		t.Fatalf("a table it may not see was listed: %v", listed)
	}

	// Nor when the catalog is read directly.
	res := rows(t, settings, "SELECT table_name FROM information_schema.tables")
	named := map[string]bool{}
	for _, row := range res.Rows {
		named[fmt.Sprint(row[0])] = true
	}
	if !named["customers"] {
		t.Fatalf("the catalog read came back with nothing useful: %v", named)
	}
	if named["secret_keys"] || named["leaky_view"] {
		t.Fatalf("the catalog read named a table it may not see: %v", named)
	}
	// Including when it goes looking for exactly that one.
	res = rows(t, settings, "SELECT table_name FROM information_schema.tables WHERE table_name = 'secret_keys'")
	if len(res.Rows) != 0 {
		t.Fatalf("asking for it by name found it: %v", res.Rows)
	}
}

func TestAGovernedToolStillAnswersOrdinaryQuestions(t *testing.T) {
	settings := governed(t)
	res := rows(t, settings, "SELECT id, email FROM customers WHERE id = ?", 1)
	if got := cell(t, res, "email"); got != "a@example.com" {
		t.Fatalf("email = %v", got)
	}
	res = rows(t, settings, "SELECT * FROM orders")
	if got := cell(t, res, "note"); got != "first" {
		t.Fatalf("a table with nothing hidden in it should come back whole: %v", got)
	}
	res = rows(t, settings, "SELECT COUNT(*) AS n FROM customers c JOIN orders o ON o.customer_id = c.id")
	if got := fmt.Sprint(cell(t, res, "n")); got != "1" {
		t.Fatalf("count = %v", got)
	}
	// Describing a table it may see still works, hidden column and all: the model
	// needs to know the column is there to understand what came back.
	res = rows(t, settings, "SHOW COLUMNS FROM customers")
	if res.RowCount != 4 {
		t.Fatalf("columns = %d, want 4", res.RowCount)
	}
}

func TestAToolWithoutAPolicyIsUntouched(t *testing.T) {
	settings := governed(t)
	settings.Policy = sqlguard.Policy{}

	res := rows(t, settings, "SELECT ssn FROM customers")
	if got := cell(t, res, "ssn"); got != "123-45-6789" {
		t.Fatalf("without a policy the value is the value: %v", got)
	}
	if res := rows(t, settings, "SELECT * FROM secret_keys"); res.RowCount != 1 {
		t.Fatalf("without a policy every table is in reach: %+v", res)
	}
}

func TestAWriteToolIsGovernedToo(t *testing.T) {
	settings := governed(t)
	settings.Access = AccessBoth

	// The hidden field cannot be written INTO or copied out through a SET, and
	// the table out of reach cannot be changed, though this tool may write.
	for _, sql := range []string{
		"UPDATE customers SET ssn = 'x' WHERE id = 1",
		"UPDATE customers SET email = ssn WHERE id = 1",
		"INSERT INTO secret_keys VALUES (2, 'another')",
		"DELETE FROM secret_keys",
		// And a governed tool does not run the statements that would move a table
		// out from under its own policy.
		"RENAME TABLE secret_keys TO fine",
		"ALTER TABLE customers CHANGE ssn notssn VARCHAR(20)",
	} {
		refusal(t, settings, sql)
	}

	// What it may write, it writes.
	if _, reason, err := run(context.Background(), settings, "UPDATE orders SET note = 'second' WHERE id = 1", nil, 0); err != nil || reason != "" {
		t.Fatalf("an ordinary write should run: reason=%q err=%v", reason, err)
	}
	if got := cell(t, rows(t, settings, "SELECT note FROM orders WHERE id = 1"), "note"); got != "second" {
		t.Fatalf("the write did not land: %v", got)
	}
}

func TestTheModelIsToldThereIsAPolicyAndNotWhatItHides(t *testing.T) {
	config, err := tmpl().Config("mysql", map[string]any{
		"access": "read", "host": "db.internal", "database": "shop", "username": "app_ro",
		"policy.table_mode": "denylist", "policy.tables": "secret_keys\npayroll",
		"policy.field_mode": "denylist", "policy.fields": "customers.ssn",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	doc := tmpl().Documentation(config)

	if !strings.Contains(doc.Note, sqlguard.Hidden) || !strings.Contains(doc.Note, "kept back") {
		t.Fatalf("the description must say there is a policy: %q", doc.Note)
	}
	var policy *tool.Topic
	for i, topic := range doc.Topics {
		if topic.ID == "policy" {
			policy = &doc.Topics[i]
		}
	}
	if policy == nil {
		t.Fatalf("there is no policy topic to drill into: %+v", doc.Topics)
	}
	// The one thing it must never say is which tables are kept back. Naming one
	// would undo the keeping back, and the assistant would go looking for it.
	for _, secret := range []string{"secret_keys", "payroll", "ssn"} {
		if strings.Contains(doc.Note, secret) || strings.Contains(policy.Body, secret) {
			t.Errorf("%q is named in what the model is told:\n%s\n%s", secret, doc.Note, policy.Body)
		}
	}
	// It must say enough to stop the assistant rewording its way around it.
	for _, phrase := range []string{"rewording", "WHERE", "ORDER BY"} {
		if !strings.Contains(policy.Body, phrase) {
			t.Errorf("the guide does not mention %q:\n%s", phrase, policy.Body)
		}
	}
}

func TestAnAllowlistNamesWhatCanBeReached(t *testing.T) {
	// Naming what is in reach costs nothing and saves the assistant guessing.
	config, err := tmpl().Config("mysql", map[string]any{
		"access": "read", "host": "db.internal", "database": "shop", "username": "app_ro",
		"policy.table_mode": "allowlist", "policy.tables": "orders\ncustomers",
		"policy.field_mode": "allowlist", "policy.fields": "customers.id\ncustomers.email",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var body string
	for _, topic := range tmpl().Documentation(config).Topics {
		if topic.ID == "policy" {
			body = topic.Body
		}
	}
	for _, named := range []string{"orders", "customers", "customers.id", "customers.email"} {
		if !strings.Contains(body, named) {
			t.Errorf("an allowlist should name %q:\n%s", named, body)
		}
	}
}

func TestATypoInAPolicyIsCaughtWhereItIsWritten(t *testing.T) {
	settings := governed(t)

	// A name with a typo in it keeps nothing back and says nothing about it,
	// which is the one mistake here that leaves somebody believing a table is out
	// of reach when it is not.
	settings.Policy.Tables = "secret_keys\nsecret_kyes"
	config := configOf(t, settings)
	err := tmpl().Test(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "secret_kyes") {
		t.Fatalf("expected the typo to be reported, got %v", err)
	}

	// A field written against a table that has no such column, likewise.
	settings.Policy.Tables = "secret_keys"
	settings.Policy.Fields = "customers.nosuchfield"
	if err := tmpl().Test(context.Background(), configOf(t, settings)); err == nil ||
		!strings.Contains(err.Error(), "customers.nosuchfield") {
		t.Fatalf("expected the field to be reported, got %v", err)
	}

	// A pattern is written for what the database will hold as much as for what it
	// holds today, so one that matches nothing yet is left alone.
	settings.Policy.Tables = "secret_keys\narchive_*"
	settings.Policy.Fields = "customers.ssn"
	if err := tmpl().Test(context.Background(), configOf(t, settings)); err != nil {
		t.Fatalf("a pattern that matches nothing yet is not a mistake: %v", err)
	}
}

// configOf writes settings back out as the config JSON a stored tool holds.
func configOf(t *testing.T, settings Settings) json.RawMessage {
	t.Helper()
	c := settings.Connection
	config, err := tmpl().Config("mysql", map[string]any{
		"access": string(settings.Access), "host": c.Host, "port": float64(c.Port),
		"database": c.Database, "username": c.Username, "password": c.Password,
		"policy.table_mode": string(settings.Policy.TableMode), "policy.tables": settings.Policy.Tables,
		"policy.field_mode": string(settings.Policy.FieldMode), "policy.fields": settings.Policy.Fields,
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return config
}

// The attack that beats reading the statement, against the server that runs it.
//
// /*M! ... */ is MariaDB's own executable comment: MariaDB runs what is inside
// it, and a reader that is not MariaDB sees a comment. So a statement can be one
// thing to the guard and another to the database. It is taken out before
// anything reads it, so there is one text and it is the one that runs. This
// proves both halves: that the server really does execute it, and that what the
// governed tool sends carries nothing for it to execute.
func TestAVersionCommentCannotBeUsedToSmuggleAStatementPast(t *testing.T) {
	settings := governed(t)
	const smuggled = "SELECT id FROM orders /*M!100000 UNION SELECT ssn FROM customers */"

	// Governed, the statement runs as the one it appeared to be: one row from
	// orders, and no sign of the field the policy keeps back.
	res := rows(t, settings, smuggled)
	if res.RowCount != 1 {
		t.Fatalf("expected the comment to have been taken out, got %d rows: %v", res.RowCount, res.Rows)
	}
	for _, row := range res.Rows {
		if fmt.Sprint(row[0]) == "123-45-6789" {
			t.Fatalf("the hidden value came through: %v", res.Rows)
		}
	}

	// And here is why that matters. The same statement on a tool with no policy
	// is sent as it was written, and the server runs the union inside it: the
	// ungoverned tool is how we find out what the database does with the text.
	ungoverned := settings
	ungoverned.Policy = sqlguard.Policy{}
	version := fmt.Sprint(cell(t, rows(t, ungoverned, "SELECT VERSION() AS v"), "v"))
	raw := rows(t, ungoverned, smuggled)

	if strings.Contains(strings.ToLower(version), "mariadb") {
		// Two rows, the second the value the policy exists to keep back. That is
		// the leak the cleaning closes.
		if raw.RowCount != 2 {
			t.Fatalf("expected %s to expand the comment into a union, got %d row(s): %v", version, raw.RowCount, raw.Rows)
		}
		found := false
		for _, row := range raw.Rows {
			if fmt.Sprint(row[0]) == "123-45-6789" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the hidden value to come through ungoverned, got %v", raw.Rows)
		}
	} else if raw.RowCount != 1 {
		// Anything else treats it as a comment. The governed tool takes it out
		// there too, because which server is on the other end is not something it
		// should have to know.
		t.Fatalf("expected %s to ignore the comment, got %d rows", version, raw.RowCount)
	}
}

// The complex end of the language, end to end.
//
// Three things have to hold for each of these, and only the first is about
// reading them: the statement that comes out is the one intended, the DATABASE
// accepts it (a rewrite that produces SQL the server rejects is a broken tool,
// however sound its reasoning), and the value the policy keeps back appears in
// no cell of the answer.
func complexFixture(t *testing.T) Settings {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the complex-query suite")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("read the test DSN: %v", err)
	}
	host, portText, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portText)

	const database = "flexie_sag_policy_complex"
	admin := datasource.Config{
		Driver: "mysql", Host: host, Port: port, Database: "information_schema",
		Username: parsed.User, Password: parsed.Passwd, TLS: datasource.TLS{Mode: "disable"},
	}
	exec := func(cfg datasource.Config, statements ...string) {
		t.Helper()
		conn, err := datasource.Open(cfg)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = conn.Close() }()
		for _, s := range statements {
			if _, err := conn.Exec(context.Background(), s, nil); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}

	exec(admin, "DROP DATABASE IF EXISTS "+database, "CREATE DATABASE "+database)
	t.Cleanup(func() { exec(admin, "DROP DATABASE IF EXISTS "+database) })

	target := admin
	target.Database = database
	exec(target,
		"CREATE TABLE customers (id INT PRIMARY KEY, email VARCHAR(100), ssn VARCHAR(20), profile VARCHAR(100))",
		"CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, total DECIMAL(10,2), note VARCHAR(100))",
		"CREATE TABLE staff (id INT PRIMARY KEY, email VARCHAR(100), ssn VARCHAR(20))",
		"CREATE TABLE scratch (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(100))",
		"CREATE TABLE secret_keys (id INT PRIMARY KEY, value VARCHAR(100))",
		"CREATE VIEW customer_view AS SELECT id, email, ssn AS code FROM customers",
		"CREATE VIEW leaky_view AS SELECT id, value FROM secret_keys",
		"INSERT INTO customers VALUES (1,'a@x.test','123-45-6789','p1'),(2,'b@x.test','222-33-4444','p2'),(3,'c@x.test','333-44-5555','p3')",
		"INSERT INTO orders VALUES (1,1,10.00,'first'),(2,1,20.00,'second'),(3,2,30.00,'third'),(4,3,40.00,'fourth')",
		// Staff 99 shares customer 1's ssn, so a join ON the hidden field actually
		// matches and the test proves something. Its id matches no customer, so
		// the cases that join on id are untouched by it.
		"INSERT INTO staff VALUES (1,'s1@x.test','999-88-7777'),(2,'s2@x.test','888-77-6666'),(99,'s3@x.test','123-45-6789')",
		"INSERT INTO secret_keys VALUES (1,'the-key')",
	)
	return Settings{
		Connection: target,
		Access:     AccessRead,
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		},
	}
}

// secrets are the values that must never appear in any answer: the hidden field
// of every row, and the contents of the table nobody may reach.
var secrets = []string{"123-45-6789", "222-33-4444", "333-44-5555", "the-key"}

func TestComplexQueriesComeOutProper(t *testing.T) {
	settings := complexFixture(t)

	for _, c := range []struct {
		name string
		sql  string
		want string // the statement that should run; "" means "unchanged"
	}{
		{"a plain hidden field", "SELECT id, ssn FROM customers ORDER BY id",
			"SELECT `id`,'[hidden]' AS `ssn` FROM `customers` ORDER BY `id`"},

		{"a star over one table", "SELECT * FROM customers ORDER BY id",
			"SELECT `customers`.`id`,`customers`.`email`,'[hidden]' AS `ssn`,`customers`.`profile` FROM `customers` ORDER BY `id`"},

		{"a star over a join, both sides expanded", "SELECT * FROM customers c JOIN orders o ON o.customer_id = c.id ORDER BY o.id",
			"SELECT `c`.`id`,`c`.`email`,'[hidden]' AS `ssn`,`c`.`profile`,`o`.`id`,`o`.`customer_id`,`o`.`total`,`o`.`note` " +
				"FROM `customers` AS `c` JOIN `orders` AS `o` ON `o`.`customer_id`=`c`.`id` ORDER BY `o`.`id`"},

		{"a left join keeps its outer-ness", "SELECT c.*, o.total FROM customers c LEFT JOIN orders o ON o.customer_id = c.id ORDER BY c.id, o.id",
			"SELECT `c`.`id`,`c`.`email`,'[hidden]' AS `ssn`,`c`.`profile`,`o`.`total` " +
				"FROM `customers` AS `c` LEFT JOIN `orders` AS `o` ON `o`.`customer_id`=`c`.`id` ORDER BY `c`.`id`,`o`.`id`"},

		{"a CTE chain that renames it twice",
			"WITH a AS (SELECT id, ssn FROM customers), b AS (SELECT id, ssn AS code FROM a) SELECT id, code FROM b ORDER BY id",
			"WITH `a` AS (SELECT `id`,'[hidden]' AS `ssn` FROM `customers`), `b` AS (SELECT `id`,`ssn` AS `code` FROM `a`) " +
				"SELECT `id`,`code` FROM `b` ORDER BY `id`"},

		{"a derived table three deep",
			"SELECT x FROM (SELECT y AS x FROM (SELECT ssn AS y FROM customers) inner1) inner2",
			"SELECT `x` FROM (SELECT `y` AS `x` FROM (SELECT '[hidden]' AS `y` FROM `customers`) AS `inner1`) AS `inner2`"},

		{"both branches of a union", "SELECT ssn FROM customers UNION ALL SELECT ssn FROM customers",
			"SELECT '[hidden]' AS `ssn` FROM `customers` UNION ALL SELECT '[hidden]' AS `ssn` FROM `customers`"},

		{"a correlated subquery in the select list",
			"SELECT o.id, (SELECT ssn FROM customers c WHERE c.id = o.customer_id) AS s FROM orders o ORDER BY o.id",
			"SELECT `o`.`id`,(SELECT '[hidden]' AS `ssn` FROM `customers` AS `c` WHERE `c`.`id`=`o`.`customer_id`) AS `s` " +
				"FROM `orders` AS `o` ORDER BY `o`.`id`"},

		{"through a view that renames it", "SELECT id, code FROM customer_view ORDER BY id",
			"SELECT `id`,'[hidden]' AS `code` FROM `customer_view` ORDER BY `id`"},

		{"a star through that view", "SELECT * FROM customer_view ORDER BY id",
			"SELECT `customer_view`.`id`,`customer_view`.`email`,'[hidden]' AS `code` FROM `customer_view` ORDER BY `id`"},

		{"a self-join", "SELECT a.id, a.ssn FROM customers a JOIN customers b ON b.id = a.id ORDER BY a.id",
			"SELECT `a`.`id`,'[hidden]' AS `ssn` FROM `customers` AS `a` JOIN `customers` AS `b` ON `b`.`id`=`a`.`id` ORDER BY `a`.`id`"},

		{"another table's field of the same name is untouched",
			"SELECT s.id, s.ssn FROM staff s JOIN customers c ON c.id = s.id ORDER BY s.id", ""},

		// A hidden field may be joined ON. It is hidden where it is selected, and
		// which rows line up with which is a different question from what a value
		// is. Nothing about a join needs rewriting.
		{"joined on, and not selected",
			"SELECT c.id, c.email FROM customers c JOIN staff s ON s.ssn = c.ssn ORDER BY c.id", ""},
		{"joined on with USING",
			"SELECT c.id FROM customers c JOIN staff s USING (ssn) ORDER BY c.id", ""},

		// Used to decide, to order, to group: none of that returns it.
		{"used as a condition, deep in a subquery",
			"SELECT id FROM (SELECT id FROM customers WHERE ssn LIKE '123%') d", ""},
		{"ordered by, through the view that renames it",
			"SELECT id FROM customer_view ORDER BY code", ""},

		// Returned inside an expression: the whole item is replaced, and the rows
		// are still ordered and grouped by the real value underneath.
		{"returned from inside a window frame",
			"SELECT id, ROW_NUMBER() OVER (ORDER BY ssn) AS n FROM customers ORDER BY id",
			"SELECT `id`,'[hidden]' AS `n` FROM `customers` ORDER BY `id`"},
		{"returned from inside an aggregate",
			"SELECT customer_id, GROUP_CONCAT(ssn) AS g FROM customers JOIN orders ON orders.customer_id = customers.id GROUP BY customer_id ORDER BY customer_id",
			"SELECT `customer_id`,'[hidden]' AS `g` FROM `customers` JOIN `orders` ON `orders`.`customer_id`=`customers`.`id` GROUP BY `customer_id` ORDER BY `customer_id`"},

		// Nothing to hide anywhere: sent on exactly as written.
		{"a window function", "SELECT id, SUM(total) OVER (PARTITION BY customer_id ORDER BY id) AS running FROM orders ORDER BY id", ""},
		{"an aggregate with grouping", "SELECT customer_id, COUNT(*) AS n, SUM(total) AS t FROM orders GROUP BY customer_id HAVING COUNT(*) > 0 ORDER BY customer_id", ""},
		{"a recursive CTE", "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 5) SELECT i FROM n", ""},
		{"an EXISTS subquery", "SELECT c.id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id) ORDER BY c.id", ""},
		{"a CASE over visible fields", "SELECT id, CASE WHEN total > 20 THEN 'big' ELSE 'small' END AS size FROM orders ORDER BY id", ""},
		{"a star over a table with nothing hidden", "SELECT * FROM orders ORDER BY id", ""},
		{"limit and offset", "SELECT id, email FROM customers ORDER BY id LIMIT 2 OFFSET 1", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// First: exactly what statement comes out.
			want := c.want
			if want == "" {
				want = c.sql // nothing to hide here, so nothing should be touched
			}
			decision := decisionFor(t, settings, c.sql)
			if decision.SQL != want {
				t.Fatalf("the statement that would run is not the one intended\n  in   %s\n  out  %s\n  want %s", c.sql, decision.SQL, want)
			}
			t.Logf("in  : %s\nout : %s", c.sql, decision.SQL)

			// Then: the database accepts it, and the answer carries no secret.
			res, reason, err := run(context.Background(), settings, c.sql, nil, 0)
			if err != nil {
				t.Fatalf("the rewritten statement was not accepted by the database: %v", err)
			}
			if reason != "" {
				t.Fatalf("refused: %s", reason)
			}

			// A rewrite is only correct if the database agrees it is SQL, which is
			// what running it just proved, and the answer must carry no secret.
			for _, row := range res.Rows {
				for _, cell := range row {
					for _, secret := range secrets {
						if fmt.Sprint(cell) == secret {
							t.Fatalf("a value the policy keeps back came out: %v", res.Rows)
						}
					}
				}
			}
			if res.RowCount == 0 {
				t.Fatalf("no rows came back, so this proved nothing about the answer")
			}
		})
	}
}

// The same complex shapes, where the statement must not run at all.
func TestComplexQueriesThatMustBeRefused(t *testing.T) {
	settings := complexFixture(t)
	for _, c := range []struct{ name, sql string }{
		{"a CTE that reads the table nobody may reach",
			"WITH k AS (SELECT value FROM secret_keys) SELECT * FROM k"},
		{"a denied table three levels down",
			"SELECT x FROM (SELECT y AS x FROM (SELECT value AS y FROM secret_keys) i1) i2"},
		{"a denied table in one branch of a union",
			"SELECT id FROM orders UNION SELECT id FROM secret_keys"},
		{"a denied table behind a view",
			"SELECT * FROM leaky_view"},
		// The hidden field is refused in two places, and neither is about the
		// policy: writing INTO it would store the stand-in over what is really
		// there, and a SET has no select list to put a stand-in in.
		{"writing into the hidden field",
			"UPDATE customers SET ssn = 'x' WHERE id = 1"},
		{"copying it out through a SET",
			"UPDATE customers SET email = ssn WHERE id = 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, reason, err := run(context.Background(), settings, c.sql, nil, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reason == "" {
				t.Fatalf("it ran, and should not have: %+v", res)
			}
			t.Logf("refused: %s", reason)
		})
	}
}

// decisionFor is what the guard would send to the database for a statement, so a
// test can hold the rewrite itself to account rather than only its results.
func decisionFor(t *testing.T, settings Settings, sql string) sqlguard.Decision {
	t.Helper()
	conn, err := datasource.Open(settings.Connection)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()

	guard, err := guards.get(context.Background(), settings, conn)
	if err != nil {
		t.Fatalf("build the guard: %v", err)
	}
	decision, reason, err := guard.Check(sql)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if reason != "" {
		t.Fatalf("refused: %s", reason)
	}
	return decision
}

// A snapshot of what a database holds is minutes old, and a database can change
// inside those minutes. The one that matters: a VIEW made after the snapshot was
// taken, over a table nobody may reach. Nothing in the snapshot knows that name,
// so without asking again it reads as a table nobody kept back, whatever it
// reads underneath.
func TestAViewMadeSinceTheSnapshotIsStillDecidedProperly(t *testing.T) {
	settings := governed(t)

	// Warm the snapshot, so what follows is genuinely made after it.
	rows(t, settings, "SELECT id FROM orders")

	conn, err := datasource.Open(settings.Connection)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	for _, ddl := range []string{
		"CREATE VIEW made_later AS SELECT id, value FROM secret_keys",
		"CREATE TABLE plain_later (id INT, note VARCHAR(50))",
		"INSERT INTO plain_later VALUES (1, 'fine')",
	} {
		if _, err := conn.Exec(context.Background(), ddl, nil); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	// The view reads the table that is out of reach, so it is out of reach too,
	// though the snapshot in hand has never heard of it.
	refusal(t, settings, "SELECT value FROM made_later")
	// And it is not listed either.
	for _, row := range rows(t, settings, "SHOW TABLES").Rows {
		if fmt.Sprint(row[0]) == "made_later" {
			t.Fatal("a view over a table nobody may reach was listed")
		}
	}
	// An ordinary table made at the same moment works straight away: asking again
	// is what makes both of these right, and it costs one read of the catalog.
	if res := rows(t, settings, "SELECT note FROM plain_later"); res.RowCount != 1 {
		t.Fatalf("a table made since the snapshot should be usable at once: %+v", res)
	}
}
