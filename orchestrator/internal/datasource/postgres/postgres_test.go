package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
)

// The driver registers itself, and the registry exposes it by key, in the driver
// list, and by its secret fields, which is everything a caller (the query tool,
// a workflow, the admin form) needs to find and configure it.
func TestDriverIsRegistered(t *testing.T) {
	d, ok := datasource.Get("postgres")
	if !ok {
		t.Fatal("the postgres driver did not register itself")
	}
	if d.Label() == "" || d.DefaultPort() != 5432 {
		t.Fatalf("driver metadata wrong: label=%q port=%d", d.Label(), d.DefaultPort())
	}
	if dialect := d.Dialect(); !strings.Contains(dialect.SQLNote, "$1") {
		t.Errorf("the dialect must tell the model this database takes $1 placeholders, not ?: %q", dialect.SQLNote)
	}
	var listed bool
	for _, other := range datasource.All() {
		listed = listed || other.Key() == "postgres"
	}
	if !listed {
		t.Error("the driver is not in the list the admin form is built from")
	}
}

func TestFieldsAndSecrets(t *testing.T) {
	d, _ := datasource.Get("postgres")
	keys := map[string]datasource.Field{}
	for _, f := range d.Fields() {
		keys[f.Key] = f
	}
	for _, want := range []string{"host", "port", "database", "username", "password"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("the form is missing %q", want)
		}
	}
	if !keys["password"].Secret {
		t.Error("the password must be sealed at rest and write-only in the form")
	}
	if keys["password"].Secret != (len(datasource.SecretFieldKeys("postgres")) == 1) {
		t.Error("the registry and the form disagree about which fields are secret")
	}
	if !keys["host"].Endpoint || !keys["port"].Endpoint {
		t.Error("host and port are the endpoint, which is how a template weaves its own fields in after them")
	}
}

// live is a connection to a real PostgreSQL, or a skip. Everything below it is
// about what a database actually does, which is the only place some of these
// answers exist.
func live(t *testing.T) datasource.Config {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_PG_DSN not set; skipping the live PostgreSQL suite")
	}
	cfg, err := parseTestDSN(dsn)
	if err != nil {
		t.Fatalf("read SAG_TEST_PG_DSN: %v", err)
	}
	return cfg
}

// parseTestDSN reads the postgres://user:pass@host:port/db form the suite is
// pointed at. It is deliberately small: this is a test setting, not a
// connection string the product accepts.
func parseTestDSN(dsn string) (datasource.Config, error) {
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
	return datasource.Config{
		Driver: "postgres", Host: host, Port: port, Database: database,
		Username: user, Password: password, TLS: datasource.TLS{Mode: "disable"},
	}, nil
}

func fixture(t *testing.T) *datasource.Conn {
	t.Helper()
	conn, err := datasource.Open(live(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for _, ddl := range []string{
		"DROP VIEW IF EXISTS drv_view",
		"DROP TABLE IF EXISTS drv_customers",
		"CREATE TABLE drv_customers (id int primary key, email text, ssn text)",
		"CREATE VIEW drv_view AS SELECT id, ssn AS code FROM drv_customers",
		"INSERT INTO drv_customers VALUES (1, 'a@x.test', '123-45-6789')",
	} {
		if _, err := conn.Exec(context.Background(), ddl, nil); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS drv_view", nil)
		_, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS drv_customers", nil)
	})
	return conn
}

func TestReadsWithBoundArguments(t *testing.T) {
	conn := fixture(t)
	res, err := conn.Query(context.Background(), "SELECT id, email, ssn FROM drv_customers WHERE id = $1", []any{1}, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount != 1 || fmt.Sprint(res.Rows[0][1]) != "a@x.test" {
		t.Fatalf("unexpected rows: %+v", res.Rows)
	}
	if strings.Join(res.Columns, ",") != "id,email,ssn" {
		t.Fatalf("columns = %v", res.Columns)
	}
}

// The floor the query tool's one-statement rule stands on. pgx uses the simple
// protocol whenever a statement has no arguments, and the simple protocol runs
// everything in the string, so this is checked rather than assumed: it was true
// once, and a table went away.
func TestASecondStatementNeverRuns(t *testing.T) {
	conn := fixture(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "CREATE TABLE drv_target (i int)", nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "DROP TABLE IF EXISTS drv_target", nil) })

	for _, statement := range []string{
		"SELECT 1; DROP TABLE drv_target",
		"INSERT INTO drv_target VALUES (1); DROP TABLE drv_target",
		"CREATE TABLE drv_other (i int); DROP TABLE drv_target",
	} {
		if _, err := conn.Exec(ctx, statement, nil); err == nil {
			t.Errorf("%s: two statements in one call were accepted", statement)
		}
	}
	res, err := conn.Query(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'drv_target'", nil, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if fmt.Sprint(res.Rows[0][0]) != "1" {
		t.Fatal("the second statement ran: the table it named is gone")
	}
}

// A read runs inside a read-only transaction, which holds even a credential that
// could write to reading for the length of the query.
func TestAReadCannotWrite(t *testing.T) {
	conn := fixture(t)
	_, err := conn.Query(context.Background(), "INSERT INTO drv_customers VALUES (2, 'b@x.test', 'x') RETURNING id", nil, 0)
	if err == nil {
		t.Fatal("a write went through the read path")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestReadsAreCapped(t *testing.T) {
	conn := fixture(t)
	res, err := conn.Query(context.Background(), "SELECT generate_series(1, 100) AS n", nil, 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount != 5 || !res.Truncated {
		t.Fatalf("expected 5 rows marked truncated, got %d truncated=%v", res.RowCount, res.Truncated)
	}
}

// The catalog read is what the policy is enforced against, so it has to describe
// the database as it is: every table and view the connection can see without
// qualifying, the columns in the order a SELECT * stands for, and the statement
// a view is defined by.
func TestSchemaDescribesTablesColumnsAndViews(t *testing.T) {
	conn := fixture(t)
	schema, err := conn.Schema(context.Background())
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	byName := map[string]datasource.TableSchema{}
	for _, table := range schema.Tables {
		byName[table.Name] = table
	}

	customers, ok := byName["drv_customers"]
	if !ok {
		t.Fatalf("the table is missing from the catalog: %+v", byName)
	}
	if customers.View {
		t.Error("a table came back marked as a view")
	}
	if strings.Join(customers.Columns, ",") != "id,email,ssn" {
		t.Fatalf("columns are not in the order a * stands for: %v", customers.Columns)
	}

	view, ok := byName["drv_view"]
	if !ok {
		t.Fatal("the view is missing from the catalog")
	}
	if !view.View {
		t.Error("a view came back marked as a table")
	}
	// Its definition is what the guard reads to learn that "code" is somebody's
	// ssn. Without it a view is a table nobody can vouch for.
	if !strings.Contains(view.Definition, "ssn") || !strings.Contains(view.Definition, "drv_customers") {
		t.Fatalf("the view's definition did not come back: %q", view.Definition)
	}
}

// Asking for TLS has to mean getting TLS, or not connecting.
//
// pgx defaults to sslmode=prefer, which tries TLS and then connects WITHOUT it
// if that fails, carrying the second attempt in Fallbacks where setting
// TLSConfig does not reach. So "require" against a server with ssl off
// connected, unencrypted, and said nothing. The test server has TLS off, which
// is what makes it the right server to ask.
//
// This is a security path, so it is the failure mode that is tested: not that
// TLS works where it is available, but that its absence is refused.
func TestRequiringTLSRefusesAPlaintextServer(t *testing.T) {
	cfg := live(t)
	cfg.TLS = datasource.TLS{Mode: "require"}

	conn, err := datasource.Open(cfg)
	if err != nil {
		return // refused before a pool was built: also correct
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(context.Background()); err == nil {
		t.Fatal("TLS was required and a server with it switched off accepted the connection: " +
			"the connection is not encrypted and nothing said so")
	}

	// And the same server is reachable when nobody asked for TLS, so the refusal
	// above is about TLS rather than about the server being unreachable.
	cfg.TLS = datasource.TLS{Mode: "disable"}
	plain, err := datasource.Open(cfg)
	if err != nil {
		t.Fatalf("open without TLS: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if err := plain.Ping(context.Background()); err != nil {
		t.Fatalf("ping without TLS: %v", err)
	}
}
