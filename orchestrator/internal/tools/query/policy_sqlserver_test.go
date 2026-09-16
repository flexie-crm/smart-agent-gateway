package query

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// The policy against a real Microsoft SQL Server.
//
// It exists for the reason the MySQL and PostgreSQL suites do: everything else
// is decided from a snapshot a test wrote by hand, and this is the only layer
// that can find out whether that snapshot matches what a database actually says
// about itself, and whether what the rewrite writes back is SQL the server will
// run. A token splice that produced something subtly unrunnable would pass every
// unit test in the analyzer package.
//
// Several of these assert TWICE: once that the governed tool holds, and once
// that an ungoverned one hands the value straight over. A test that only shows
// the guard refusing cannot tell a real defence from a statement the database
// was never going to run, and every route this dialect adds was confirmed to be
// a real route out before it was closed.
//
// Set SAG_TEST_MSSQL_DSN to run it, e.g.
//
//	sqlserver://sa:SagTest!2026pw@127.0.0.1:1434
func msFixture(t *testing.T) datasource.Config {
	t.Helper()
	// This fixture remakes the database, so a guard cached from a run that
	// described the old one would be describing nothing.
	guards.forget()
	dsn := os.Getenv("SAG_TEST_MSSQL_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_MSSQL_DSN not set; skipping the SQL Server policy suite")
	}
	admin, err := msConfig(dsn)
	if err != nil {
		t.Fatalf("read SAG_TEST_MSSQL_DSN: %v", err)
	}

	const database = "flexie_sag_policy_test"
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

	// A database in use cannot be dropped, so it is taken offline first, which
	// throws everybody off it.
	drop := "IF DB_ID('" + database + "') IS NOT NULL BEGIN ALTER DATABASE " + database +
		" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + database + "; END"
	run(admin, drop, "CREATE DATABASE "+database)
	t.Cleanup(func() { run(admin, drop) })

	target := admin
	target.Database = database
	run(target,
		"CREATE TABLE customers (id int PRIMARY KEY, email nvarchar(100), ssn nvarchar(20), profile nvarchar(100))",
		"CREATE TABLE orders (id int PRIMARY KEY, customer_id int, total decimal(10,2), note nvarchar(100))",
		"CREATE TABLE payroll (id int PRIMARY KEY, emp nvarchar(50), amount decimal(10,2))",
		"CREATE TABLE scratch (id int PRIMARY KEY, note nvarchar(100))",
	)
	// Each view is its own batch: CREATE VIEW must be the first statement in one.
	run(target, "CREATE VIEW customer_view AS SELECT id, email, ssn AS code FROM customers")
	run(target, "CREATE VIEW leaky_view AS SELECT id, amount FROM payroll")
	run(target,
		"INSERT INTO customers VALUES (1, 'a@example.com', '123-45-6789', 'notes')",
		"INSERT INTO orders VALUES (1, 1, 9.99, 'first')",
		"INSERT INTO payroll VALUES (1, 'someone', 1000.00)",
	)
	return target
}

// msConfig reads sqlserver://user:password@host:port[/database].
func msConfig(dsn string) (datasource.Config, error) {
	rest, ok := strings.CutPrefix(dsn, "sqlserver://")
	if !ok {
		return datasource.Config{}, fmt.Errorf("expected sqlserver://user:password@host:port")
	}
	credentials, address, ok := strings.Cut(rest, "@")
	if !ok {
		return datasource.Config{}, fmt.Errorf("no @ in the DSN")
	}
	user, password, _ := strings.Cut(credentials, ":")
	hostPort, database, _ := strings.Cut(address, "/")
	database, _, _ = strings.Cut(database, "?")
	host, portText, _ := strings.Cut(hostPort, ":")
	port := 1433
	if portText != "" {
		if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
			return datasource.Config{}, fmt.Errorf("port: %w", err)
		}
	}
	if database == "" {
		database = "master"
	}
	return datasource.Config{
		Driver: "sqlserver", Host: host, Port: port, Database: database,
		Username: user, Password: password, TLS: datasource.TLS{Mode: "disable"},
	}, nil
}

// msGoverned is the tool these tests run as: one table out of reach, one field
// hidden.
func msGoverned(t *testing.T) Settings {
	t.Helper()
	return Settings{
		Connection: msFixture(t),
		Access:     AccessRead,
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "payroll",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		},
	}
}

const msRealSSN = "123-45-6789"

// The whole promise, against a server: the value does not come back, and the
// stand-in does.
func TestMSAHiddenFieldNeverLeavesTheDatabase(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT ssn FROM customers",
		"SELECT * FROM customers",
		"SELECT c.ssn FROM customers c",
		"SELECT UPPER(ssn) AS ssn FROM customers",
		"SELECT ssn FROM (SELECT ssn FROM customers) x",
		"WITH q AS (SELECT * FROM customers) SELECT * FROM q",
		"SELECT CONCAT(ssn, '') FROM customers",
		// The renaming view, which no rule mentions by name.
		"SELECT code FROM customer_view",
		"SELECT * FROM customer_view",
		"SELECT ssn FROM customers UNION ALL SELECT ssn FROM customers",
	} {
		res := rows(t, settings, sql)
		if strings.Contains(fmt.Sprint(res.Rows), msRealSSN) {
			t.Errorf("%s: the real value came back: %+v", sql, res.Rows)
		}
		if !strings.Contains(fmt.Sprint(res.Rows), sqlguard.Hidden) {
			t.Errorf("%s: nothing stood in for the hidden value: %+v", sql, res.Rows)
		}
	}

	// And the same statements really do hand it over when nothing governs them,
	// which is what makes every assertion above worth anything.
	open := ungoverned(settings)
	for _, sql := range []string{
		"SELECT ssn FROM customers",
		"SELECT code FROM customer_view",
		"SELECT * FROM customers",
	} {
		res := rows(t, open, sql)
		if !strings.Contains(fmt.Sprint(res.Rows), msRealSSN) {
			t.Errorf("the control failed: %s returned no real value even ungoverned, "+
				"so this suite proves nothing: %+v", sql, res.Rows)
		}
	}
}

// The other half: a hidden field goes on working everywhere its value is not
// returned, and the statement reaches the server exactly as it was written.
func TestMSAHiddenFieldStillWorksEverywhereElse(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT id FROM customers WHERE ssn LIKE '123%'",
		"SELECT id FROM customers ORDER BY ssn",
		"SELECT COUNT(*) AS n FROM customers WHERE ssn IS NOT NULL",
		"SELECT COUNT(*) AS n FROM customers GROUP BY ssn",
	} {
		res := rows(t, settings, sql)
		if len(res.Rows) == 0 {
			t.Errorf("%s: came back empty, so the condition over the hidden field did not work", sql)
		}
		if strings.Contains(fmt.Sprint(res.Rows), msRealSSN) {
			t.Errorf("%s: the value came back anyway: %+v", sql, res.Rows)
		}
	}
}

// A view over a table nobody may see is refused though the statement names only
// the view. This is the assertion the whole definition-reading path exists for,
// and the only place it can be made against the text SQL Server really stores.
func TestMSAViewIsDecidedByWhatItReads(t *testing.T) {
	settings := msGoverned(t)
	if reason := refusal(t, settings, "SELECT * FROM leaky_view"); reason == "" {
		t.Fatal("a view over a table out of reach was not refused")
	}
	// The control: ungoverned, it really does return the rows.
	res := rows(t, ungoverned(settings), "SELECT * FROM leaky_view")
	if len(res.Rows) == 0 {
		t.Fatal("the control failed: leaky_view returns nothing even ungoverned")
	}
}

func TestMSATableOutOfReachIsRefused(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT * FROM payroll",
		"SELECT id FROM orders WHERE id IN (SELECT id FROM payroll)",
		"SELECT o.id FROM orders o JOIN payroll p ON p.id = o.id",
	} {
		if reason := refusal(t, settings, sql); reason == "" {
			t.Errorf("%s: was not refused", sql)
		}
	}
	// Control.
	if res := rows(t, ungoverned(settings), "SELECT * FROM payroll"); len(res.Rows) == 0 {
		t.Fatal("the control failed: payroll is empty even ungoverned")
	}
}

// What a rewrite writes back has to be SQL this server will actually run, and a
// token splice is the one thing the analyzer's own tests cannot check. Every
// statement here is rewritten; if any came back unrunnable, rows() fails.
func TestMSARewrittenStatementRuns(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT * FROM customers",
		"SELECT c.* FROM customers c",
		"SELECT id, ssn, email FROM customers",
		"SELECT TOP 1 ssn FROM customers ORDER BY id",
		"SELECT CONCAT(ssn, '-x') AS tagged FROM customers",
		"WITH q AS (SELECT ssn FROM customers) SELECT ssn AS y FROM q",
		"SELECT * FROM customers WHERE ssn LIKE '123%' ORDER BY id",
	} {
		res := rows(t, settings, sql)
		if len(res.Columns) == 0 {
			t.Errorf("%s: came back with no columns", sql)
		}
		if strings.Contains(fmt.Sprint(res.Rows), msRealSSN) {
			t.Errorf("%s: the value came back: %+v", sql, res.Rows)
		}
	}
}

// The shape of the answer does not change: a caller asking for three columns
// gets three, with the hidden one still under its own name.
func TestMSTheShapeOfTheAnswerIsKept(t *testing.T) {
	settings := msGoverned(t)
	res := rows(t, settings, "SELECT id, ssn, email FROM customers")
	if len(res.Columns) != 3 {
		t.Fatalf("expected three columns, got %v", res.Columns)
	}
	if !strings.EqualFold(res.Columns[1], "ssn") {
		t.Fatalf("the hidden column lost its name: %v", res.Columns)
	}

	// And a star expands to every column the table has, in order.
	res = rows(t, settings, "SELECT * FROM customers")
	if len(res.Columns) != 4 {
		t.Fatalf("expected the table's four columns, got %v", res.Columns)
	}
	want := []string{"id", "email", "ssn", "profile"}
	for i, name := range want {
		if !strings.EqualFold(res.Columns[i], name) {
			t.Fatalf("column %d should be %q, got %v", i, name, res.Columns)
		}
	}
}

// Introspection is the way round a policy if it is not narrowed: a tool that may
// not read payroll should not be able to say it exists.
func TestMSTheCatalogIsNarrowed(t *testing.T) {
	settings := msGoverned(t)
	res := rows(t, settings, "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES")
	listing := fmt.Sprint(res.Rows)
	if strings.Contains(strings.ToLower(listing), "payroll") {
		t.Errorf("a table out of reach was listed: %v", res.Rows)
	}
	if !strings.Contains(strings.ToLower(listing), "customers") {
		t.Errorf("the tables in reach were not listed: %v", res.Rows)
	}

	// The control: ungoverned, payroll is right there.
	open := rows(t, ungoverned(settings), "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES")
	if !strings.Contains(strings.ToLower(fmt.Sprint(open.Rows)), "payroll") {
		t.Fatal("the control failed: payroll is not listed even ungoverned")
	}
}

// sys.* is this engine's pg_catalog, and sys.sql_modules hands back the text of
// a view definition, which names the hidden column and the table it belongs to.
func TestMSTheServersOwnCatalogIsRefused(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT name FROM sys.tables",
		"SELECT definition FROM sys.sql_modules",
	} {
		if reason := refusal(t, settings, sql); reason == "" {
			t.Errorf("%s: was not refused", sql)
		}
	}
	// The control, and the reason this one matters: ungoverned, that definition
	// really does spell out the hidden column.
	res := rows(t, ungoverned(settings), "SELECT definition FROM sys.sql_modules")
	if !strings.Contains(strings.ToLower(fmt.Sprint(res.Rows)), "ssn") {
		t.Fatal("the control failed: sys.sql_modules did not expose a definition naming the hidden column")
	}
}

// A batch is refused, and on this engine the guard is the only thing refusing
// it: the driver cannot, because T-SQL is a batch language.
func TestMSOneStatementAtATime(t *testing.T) {
	settings := msGoverned(t)
	for _, sql := range []string{
		"SELECT id FROM orders; SELECT ssn FROM customers",
		"SELECT id FROM orders\nGO\nSELECT ssn FROM customers",
	} {
		reason := refusal(t, settings, sql)
		if reason == "" {
			t.Fatalf("%s: a batch was not refused", sql)
		}
		if !strings.Contains(reason, "one statement per call") {
			t.Errorf("%s: refused for the wrong reason: %s", sql, reason)
		}
	}
}

// OUTPUT hands rows back from a write, so everything true of a select list is
// true of it.
//
// This one goes around the tool rather than through it, and the reason is worth
// recording. A write reaches the database through Conn.Exec (run.go), which
// returns a row count and nothing else, so OUTPUT rows never surface today. That
// is true of every dialect, Postgres RETURNING included, and it makes the
// obvious version of this test worthless: the rows come back empty, empty
// contains no secret, and the assertion passes with the guard switched off.
//
// So this asks the guard what it would send, then runs THAT against the server
// as a query, where the rows really do come back. It is the only way to find out
// what an OUTPUT clause actually returns after a token splice, which is the
// thing worth knowing: the literal has to be valid in that position, and no unit
// test can say whether SQL Server agrees.
func TestMSOutputIsAProjectionLikeAnyOther(t *testing.T) {
	cfg := msFixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()

	schema, err := conn.Schema(context.Background())
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	guard, err := sqlguard.New("sqlserver", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, catalogOf("sqlserver", schema))
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}

	const statement = "UPDATE customers SET email = 'b@example.com' OUTPUT inserted.ssn WHERE id = 1"
	decision, reason, err := guard.Check(statement)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if reason != "" {
		t.Fatalf("the statement was refused rather than masked: %s", reason)
	}
	if !decision.Rewritten {
		t.Fatal("the OUTPUT clause was not rewritten at all")
	}

	// The control first, and it has to run first: ungoverned, the real value is
	// right there in the OUTPUT rows. Without this, the assertion below could not
	// tell masking from a statement that returns nothing.
	before, err := conn.Query(context.Background(), statement, nil, 10)
	if err != nil {
		t.Fatalf("the control could not run: %v", err)
	}
	if !strings.Contains(fmt.Sprint(before.Rows), msRealSSN) {
		t.Fatalf("the control failed: OUTPUT returned no real value even ungoverned: %+v", before.Rows)
	}

	// And now what the guard would have sent instead. That it runs at all is half
	// the point: the stand-in is spliced into an OUTPUT clause, and the server has
	// to accept a literal there.
	after, err := conn.Query(context.Background(), decision.SQL, nil, 10)
	if err != nil {
		t.Fatalf("the rewritten OUTPUT clause would not run: %v\n%s", err, decision.SQL)
	}
	if strings.Contains(fmt.Sprint(after.Rows), msRealSSN) {
		t.Errorf("OUTPUT handed the hidden value back: %+v", after.Rows)
	}
	if !strings.Contains(fmt.Sprint(after.Rows), sqlguard.Hidden) {
		t.Errorf("nothing stood in for it: %+v", after.Rows)
	}
}
