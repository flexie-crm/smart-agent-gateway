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

// The policy against a real PostgreSQL.
//
// The same suite the MySQL side has, plus the routes PostgreSQL adds. It exists
// for the reason that one does: everything else is decided from a snapshot a
// test wrote by hand, and this is where it comes out whether the snapshot
// matches what a database actually says about itself, and whether what the
// rewrite writes back is SQL the server will run.
//
// Several of these assert twice: once that the governed tool holds, and once
// that an ungoverned one leaks. A test that only shows the guard refusing cannot
// tell a real defence from a statement the database was never going to run.

func pgFixture(t *testing.T) datasource.Config {
	t.Helper()
	// This fixture remakes the database, so a guard cached from a run that
	// described the old one would be describing nothing.
	guards.forget()
	dsn := os.Getenv("SAG_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_PG_DSN not set; skipping the PostgreSQL policy suite")
	}
	admin, err := pgConfig(dsn)
	if err != nil {
		t.Fatalf("read SAG_TEST_PG_DSN: %v", err)
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

	run(admin, "DROP DATABASE IF EXISTS "+database, "CREATE DATABASE "+database)
	t.Cleanup(func() { run(admin, "DROP DATABASE IF EXISTS "+database) })

	target := admin
	target.Database = database
	run(target,
		"CREATE TABLE customers (id int PRIMARY KEY, email text, ssn text, profile text)",
		"CREATE TABLE orders (id int PRIMARY KEY, customer_id int, total numeric(10,2), note text)",
		"CREATE TABLE secret_keys (id int PRIMARY KEY, value text)",
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

func pgConfig(dsn string) (datasource.Config, error) {
	rest, ok := strings.CutPrefix(dsn, "postgres://")
	if !ok {
		return datasource.Config{}, fmt.Errorf("expected postgres://user:password@host:port/database")
	}
	credentials, address, ok := strings.Cut(rest, "@")
	if !ok {
		return datasource.Config{}, fmt.Errorf("no @ in the DSN")
	}
	user, password, _ := strings.Cut(credentials, ":")
	hostPort, database, _ := strings.Cut(address, "/")
	database, _, _ = strings.Cut(database, "?")
	host, portText, _ := strings.Cut(hostPort, ":")
	port := 5432
	if portText != "" {
		if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
			return datasource.Config{}, fmt.Errorf("port: %w", err)
		}
	}
	if database == "" {
		database = "postgres"
	}
	return datasource.Config{
		Driver: "postgres", Host: host, Port: port, Database: database,
		Username: user, Password: password, TLS: datasource.TLS{Mode: "disable"},
	}, nil
}

// pgGoverned is the tool these tests run as: one table out of reach, one field
// hidden.
func pgGoverned(t *testing.T) Settings {
	t.Helper()
	return Settings{
		Connection: pgFixture(t),
		Access:     AccessRead,
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
		},
	}
}

// ungoverned is the same connection with no policy on it, which is how these
// tests show that what the guard stopped was going to happen.
func ungoverned(settings Settings) Settings {
	settings.Policy = sqlguard.Policy{}
	return settings
}

const realSSN = "123-45-6789"

func TestPGAHiddenFieldNeverLeavesTheDatabase(t *testing.T) {
	settings := pgGoverned(t)
	for _, sql := range []string{
		"SELECT ssn FROM customers",
		"SELECT * FROM customers",
		"SELECT c.ssn FROM customers c",
		"SELECT upper(ssn) AS ssn FROM customers",
		"SELECT ssn FROM (SELECT ssn FROM customers) x",
		"WITH q AS (SELECT * FROM customers) SELECT * FROM q",
		"SELECT code FROM customer_view",
		"SELECT * FROM customer_view",
		"SELECT (c).ssn FROM customers c",
		"SELECT ssn FROM customers UNION ALL SELECT ssn FROM customers",
	} {
		res := rows(t, settings, sql)
		if found := strings.Contains(fmt.Sprint(res.Rows), realSSN); found {
			t.Errorf("%s: the real value came back: %+v", sql, res.Rows)
		}
		if !strings.Contains(fmt.Sprint(res.Rows), sqlguard.Hidden) {
			t.Errorf("%s: nothing stood in for the hidden value: %+v", sql, res.Rows)
		}
	}
	// And the same statements do hand it over when nothing is governing them,
	// which is what makes the assertions above worth anything.
	open := ungoverned(settings)
	for _, sql := range []string{"SELECT ssn FROM customers", "SELECT code FROM customer_view"} {
		res := rows(t, open, sql)
		if !strings.Contains(fmt.Sprint(res.Rows), realSSN) {
			t.Fatalf("%s: without a policy the value should come back, got %+v", sql, res.Rows)
		}
	}
}

func TestPGAHiddenFieldIsStillUsableForAsking(t *testing.T) {
	settings := pgGoverned(t)
	// The value decides which rows come back, and none of it comes back.
	res := rows(t, settings, "SELECT id FROM customers WHERE ssn = $1", realSSN)
	if res.RowCount != 1 || fmt.Sprint(cell(t, res, "id")) != "1" {
		t.Fatalf("a condition over a hidden field should still select: %+v", res.Rows)
	}
	res = rows(t, settings, "SELECT id FROM customers ORDER BY ssn")
	if res.RowCount != 1 {
		t.Fatalf("an order over a hidden field should still order: %+v", res.Rows)
	}
}

func TestPGATableOutOfReachIsOutOfReachAndUnlisted(t *testing.T) {
	settings := pgGoverned(t)
	for _, sql := range []string{
		"SELECT * FROM secret_keys",
		"SELECT * FROM leaky_view",
		"SELECT id FROM orders WHERE id IN (SELECT id FROM secret_keys)",
	} {
		refusal(t, settings, sql)
	}
	// It is not in the catalog either: a tool that answers "which tables are
	// there" with the name has already said where to look. Nor is the view that
	// reads it.
	res := rows(t, settings, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'")
	listed := fmt.Sprint(res.Rows)
	for _, name := range []string{"secret_keys", "leaky_view"} {
		if strings.Contains(listed, name) {
			t.Errorf("%s is listed in the catalog: %s", name, listed)
		}
	}
	for _, name := range []string{"customers", "orders", "customer_view"} {
		if !strings.Contains(listed, name) {
			t.Errorf("%s should be listed: %s", name, listed)
		}
	}
	// Ungoverned, the same read names them, so the narrowing is doing the work.
	res = rows(t, ungoverned(settings), "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'")
	if !strings.Contains(fmt.Sprint(res.Rows), "secret_keys") {
		t.Fatalf("without a policy the catalog should name everything: %+v", res.Rows)
	}
}

// A row is a value in PostgreSQL, so a table can be handed back whole without a
// single column being named. This is the route that does not exist on the MySQL
// side, checked against a server that really would hand it over.
func TestPGAWholeRowCannotCarryAHiddenValueOut(t *testing.T) {
	settings := pgGoverned(t)
	for _, sql := range []string{
		"SELECT c FROM customers c",
		"SELECT row_to_json(c) FROM customers c",
		"SELECT to_jsonb(c) FROM customers c",
		"SELECT c::text FROM customers c",
	} {
		reason := refusal(t, settings, sql)
		if !strings.Contains(reason, "whole row") {
			t.Errorf("%s: %q", sql, reason)
		}
		// The same statement, ungoverned, hands the value over in full.
		res := rows(t, ungoverned(settings), sql)
		if !strings.Contains(fmt.Sprint(res.Rows), realSSN) {
			t.Errorf("%s: this was supposed to be a real route out, got %+v", sql, res.Rows)
		}
	}
}

// pg_catalog is on every connection's search path whether anybody put it there
// or not, so the server's own catalog is reachable without a schema in front of
// it to refuse.
func TestPGTheServersOwnCatalogIsNotReadable(t *testing.T) {
	settings := pgGoverned(t)
	for _, sql := range []string{
		"SELECT relname FROM pg_class",
		"SELECT tablename FROM pg_tables",
		"SELECT definition FROM pg_views WHERE viewname = 'customer_view'",
		"SELECT * FROM pg_catalog.pg_class",
	} {
		refusal(t, settings, sql)
	}
	// Ungoverned, pg_views hands over the definition of a view, which names the
	// hidden column and the table it belongs to.
	res := rows(t, ungoverned(settings), "SELECT definition FROM pg_views WHERE viewname = 'customer_view'")
	if !strings.Contains(fmt.Sprint(res.Rows), "ssn") {
		t.Fatalf("this was supposed to be a real route out, got %+v", res.Rows)
	}
}

func TestPGAGovernedToolStillAnswersOrdinaryQuestions(t *testing.T) {
	settings := pgGoverned(t)
	res := rows(t, settings, "SELECT id, email FROM customers WHERE id = $1", 1)
	if fmt.Sprint(cell(t, res, "email")) != "a@example.com" {
		t.Fatalf("%+v", res.Rows)
	}
	res = rows(t, settings, "SELECT o.id, o.total, c.email FROM orders o JOIN customers c ON c.id = o.customer_id")
	if res.RowCount != 1 {
		t.Fatalf("a join over governed tables should still run: %+v", res.Rows)
	}
	res = rows(t, settings, "SELECT count(*) AS n FROM customers")
	if fmt.Sprint(cell(t, res, "n")) != "1" {
		t.Fatalf("%+v", res.Rows)
	}
	// A star over a table with nothing hidden in it is sent exactly as written.
	res = rows(t, settings, "SELECT * FROM orders")
	if strings.Join(res.Columns, ",") != "id,customer_id,total,note" {
		t.Fatalf("columns = %v", res.Columns)
	}
}

func TestPGAWriteToolIsGovernedToo(t *testing.T) {
	settings := pgGoverned(t)
	settings.Access = AccessWrite

	// Returned by a write, so taken out on the way back.
	res := rows(t, settings, "UPDATE customers SET profile = 'x' WHERE id = 1 RETURNING id, ssn")
	if strings.Contains(fmt.Sprint(res.Rows), realSSN) {
		t.Fatalf("a RETURNING handed the value over: %+v", res.Rows)
	}
	// Copied into a column that could be read back afterwards.
	refusal(t, settings, "UPDATE customers SET profile = ssn")
	// Written into, where the stand-in would go over the real value.
	refusal(t, settings, "UPDATE customers SET ssn = 'x'")
	// The real value is still there: nothing above wrote over it.
	settings.Access = AccessRead
	open := ungoverned(settings)
	res = rows(t, open, "SELECT ssn FROM customers WHERE id = 1")
	if fmt.Sprint(cell(t, res, "ssn")) != realSSN {
		t.Fatalf("the real value was overwritten: %+v", res.Rows)
	}
}

// A view made since the snapshot was taken looks like a table nobody kept back,
// whatever it reads underneath. The tool notices a name it has not heard of and
// reads the catalog again before deciding.
func TestPGAViewMadeSinceTheSnapshotIsStillDecidedProperly(t *testing.T) {
	settings := pgGoverned(t)
	// Warm the snapshot, so the view below is genuinely made after it.
	rows(t, settings, "SELECT id FROM customers")

	conn, err := datasource.Open(settings.Connection)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	for _, ddl := range []string{
		"CREATE VIEW late_view AS SELECT id, value FROM secret_keys",
		"CREATE VIEW late_rename AS SELECT id, ssn AS code FROM customers",
	} {
		if _, err := conn.Exec(context.Background(), ddl, nil); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	refusal(t, settings, "SELECT * FROM late_view")
	res := rows(t, settings, "SELECT code FROM late_rename")
	if strings.Contains(fmt.Sprint(res.Rows), realSSN) {
		t.Fatalf("a view made since the snapshot handed the value over: %+v", res.Rows)
	}
}
