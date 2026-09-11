// Package postgres is how this product reaches a PostgreSQL database: how to
// connect to one, run a statement on it, and ask it what it holds. One database
// is one package, so what is true of Postgres and nothing else lives here.
package postgres

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/datasource"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// defaultRows caps a read that did not ask for a limit, so a careless SELECT
// does not stream an unbounded result back.
const defaultRows = 500

// schemaTimeout bounds reading the catalog. It is not a statement somebody
// wrote, so it gets its own ceiling rather than a caller's.
const schemaTimeout = 15 * time.Second

// The PostgreSQL driver. It registers itself, so a build can reach a Postgres
// database only if something imports this package.
func init() { datasource.Register(Driver{}) }

// Driver is the PostgreSQL driver.
type Driver struct{}

func (Driver) Key() string      { return "postgres" }
func (Driver) Label() string    { return "PostgreSQL" }
func (Driver) DefaultPort() int { return 5432 }

func (Driver) Dialect() datasource.Dialect {
	return datasource.Dialect{
		Name: "PostgreSQL",
		SQLNote: "Write PostgreSQL SQL: double-quote identifiers that need quoting, put values in $1, $2 placeholders " +
			"rather than ?, and introspect with information_schema or pg_catalog, not SHOW TABLES.",
		Introspection: "List tables with `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`. " +
			"See a table's columns with `SELECT column_name, data_type FROM information_schema.columns WHERE table_name = $1`. " +
			"A database holds many schemas; unqualified names resolve through the search path, and `current_schema()` is the one they land in.",
	}
}

// Fields is what a Postgres connection needs. The admin form is built from this,
// so the query template does not hard-code the shape: it asks the driver.
func (Driver) Fields() []datasource.Field {
	return []datasource.Field{
		// The endpoint leads, port narrower than host; the template weaves its
		// access selector in after it.
		{Key: "host", Label: "Host", Type: datasource.FieldText, Required: true, Span: 4, Endpoint: true, Help: "Hostname or IP of the database server."},
		{Key: "port", Label: "Port", Type: datasource.FieldNumber, Default: "5432", Span: 2, Endpoint: true},
		{Key: "database", Label: "Database", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "username", Label: "Username", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "password", Label: "Password", Type: datasource.FieldPassword, Secret: true, Span: 3},
	}
}

// transport turns the connection's TLS settings into the config pgx dials with,
// or nil for a plain connection. It is a function of its own so it can be tested
// without a server: what these settings become is the whole of what an
// administrator asking for TLS gets, and it is not visible from a *sql.DB.
//
// The settings themselves, and the meaning of the three modes, are shared with
// every other driver (internal/datasource/tls.go). Only the way a driver is
// handed the result differs: this one takes a *tls.Config directly, where the
// MySQL driver has to register one under a name and put the name in its DSN.
func transport(cfg datasource.Config) (*tls.Config, error) {
	switch cfg.TLS.Mode {
	case "", "disable":
		return nil, nil
	case "require", "verify":
		// require encrypts without checking who answered; verify checks the
		// certificate against the CA. Both are settled in the shared builder, so
		// "require" means the same thing here as it does on any other driver.
		out, err := datasource.TLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		if out.ServerName == "" {
			// The name to check the certificate against, when nobody said one.
			// Through an SSH bastion the host by then is the local end of the
			// tunnel, so verify needs the Server name field filled in with the
			// real one: this fails closed rather than quietly checking the
			// certificate against 127.0.0.1 and accepting it.
			out.ServerName = cfg.Host
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown TLS mode %q", cfg.TLS.Mode)
	}
}

// Connect builds a pool for a config.
//
// The connection is built as a pgx config rather than a URL, so the TLS settings
// an administrator gave us (a CA, a client certificate, a server name) become a
// real tls.Config instead of being written into a string and parsed back out.
//
// Two floors are set here, under whatever the query tool decides above. The
// extended protocol is used for every statement, which is what makes a call one
// statement: the simple protocol would let a semicolon carry a second one.
func (Driver) Connect(cfg datasource.Config) (*sql.DB, error) {
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	conn, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d dbname=%s user=%s",
		cfg.Host, port, cfg.Database, cfg.Username))
	if err != nil {
		return nil, fmt.Errorf("read the connection settings: %w", err)
	}
	conn.Password = cfg.Password

	tlsConfig, err := transport(cfg)
	if err != nil {
		return nil, err
	}
	conn.TLSConfig = tlsConfig

	// And nothing else may be tried instead.
	//
	// pgx defaults to sslmode=prefer, which does not mean "encrypt": it means
	// try TLS and, if that does not work, connect WITHOUT it. It carries the
	// second attempt in Fallbacks, which setting TLSConfig does not touch. So an
	// administrator who chose require got a plaintext connection to any server
	// with TLS switched off, and nothing anywhere said so. Found by pointing
	// require at a server with ssl=off and watching it connect.
	//
	// One tool is one database and one decision about how to reach it. There is
	// nothing to fall back to, so there are no fallbacks: what transport
	// returned is what this connects with, or it does not connect.
	conn.Fallbacks = nil

	// Said explicitly because the alternative is dangerous rather than merely
	// different: this is the EXTENDED protocol, which keeps every argument a
	// bound parameter and carries exactly one statement per message. The simple
	// protocol writes arguments into the text and will happily run two statements
	// separated by a semicolon, which is the floor the query tool's own
	// one-statement rule stands on.
	conn.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement

	// Say how a string literal is to be read, rather than inheriting it.
	//
	// With this off, a backslash in an ordinary '...' literal escapes the next
	// character; with it on, it is a backslash. The policy layer reads a
	// statement before the server does, and it reads it the standard way, so a
	// server set the other way would be running something this had understood
	// differently. That gap is exactly the one a MariaDB executable comment
	// opened on the other driver. It is closed here by making the server agree
	// rather than by hoping it does: on is the default and has been since 9.1,
	// and this is only saying so out loud.
	if conn.RuntimeParams == nil {
		conn.RuntimeParams = map[string]string{}
	}
	conn.RuntimeParams["standard_conforming_strings"] = "on"

	db := stdlib.OpenDB(*conn)
	// A query tool is not a high-throughput path, and a bounded pool cannot
	// exhaust the target's connections.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(2 * time.Minute)
	return db, nil
}

// Query runs a read statement inside a read-only transaction and returns its
// rows, capped. The read-only transaction holds even a writable credential to
// reading for the length of the query.
func (Driver) Query(ctx context.Context, db *sql.DB, statement string, args []any, limit int) (*datasource.Result, error) {
	if limit <= 0 {
		limit = defaultRows
	}

	// A dedicated connection, so the backend running the read is known: if it
	// outlives its deadline it is cancelled on the server by that id, rather than
	// left running and holding the database while the caller has walked away.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var backend int32
	if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backend); err != nil {
		return nil, fmt.Errorf("identify connection: %w", err)
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, cancelled(db, backend, statement, args)
		}
		return nil, fmt.Errorf("run query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	res := &datasource.Result{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if res.RowCount >= limit {
			res.Truncated = true
			break
		}
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cells))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		res.Rows = append(res.Rows, normalizeRow(cells))
		res.RowCount++
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, cancelled(db, backend, statement, args)
		}
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return res, nil
}

// cancelled stops a read that ran past its deadline and explains it: the
// statement is cancelled on the server by the backend running it (so it stops
// holding the database), then EXPLAIN is run for the same statement so the
// caller learns why it was slow. Both use a fresh, short-lived context, because
// the read's own is already done.
func cancelled(db *sql.DB, backend int32, statement string, args []any) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	// pg_cancel_backend stops the running statement and leaves the connection
	// alive. The id is ours, from pg_backend_pid, never anything a caller wrote.
	_, _ = db.ExecContext(ctx, "SELECT pg_cancel_backend($1)", backend)

	return &datasource.QueryTimeout{Plan: explain(ctx, db, statement, args)}
}

// explain returns the statement's query plan as text, or a note when the plan
// itself could not be read. It is EXPLAIN without ANALYZE: the plan is wanted,
// not another run of a statement that was already too slow.
func explain(ctx context.Context, db *sql.DB, statement string, args []any) string {
	rows, err := db.QueryContext(ctx, "EXPLAIN "+statement, args...)
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
	}
	if err := rows.Err(); err != nil {
		b.WriteString("\n(the plan may be incomplete: ")
		b.WriteString(err.Error())
		b.WriteString(")")
	}
	return b.String()
}

// Exec runs a write statement and returns what it changed.
//
// The statement is PREPARED first, and that is the point rather than an
// optimisation. pgx falls back to the simple protocol whenever a statement has
// no arguments ("Always use simple protocol when there are no arguments", its
// own comment), and the simple protocol runs everything in the string: a call
// asking to run "SELECT 1" would also run whatever followed a semicolon. Found
// by trying it, and a table went away.
//
// Preparing is Postgres saying it for us. A prepared statement holds exactly one
// command, and it refuses two with "cannot insert multiple commands into a
// prepared statement" before anything runs. It costs one round trip and covers
// DDL as well, which the SQL-level PREPARE would not.
//
// This is the floor the query tool's own one-statement rule stands on, the same
// way MultiStatements=false is on the MySQL side: the tool refuses two
// statements, and if it ever failed to, the driver still would.
//
// Postgres has no last insert id: a caller that wants the row back asks for it
// with RETURNING, which comes back as rows rather than as a number here.
func (Driver) Exec(ctx context.Context, db *sql.DB, statement string, args []any) (*datasource.Result, error) {
	stmt, err := db.PrepareContext(ctx, statement)
	if err != nil {
		return nil, fmt.Errorf("run statement: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	out, err := stmt.ExecContext(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("run statement: %w", err)
	}
	res := &datasource.Result{}
	if n, err := out.RowsAffected(); err == nil {
		res.Affected = &n
	}
	return res, nil
}

// Schema reads the database's own account of itself: every table and view in the
// schemas this connection can actually see, the columns of each in the order a
// SELECT * stands for, and the statement a view is defined by.
//
// "The schemas this connection can see" is the search path, not the whole
// database. A Postgres database holds many schemas and a connection resolves
// unqualified names through its search path; the tables a statement can name
// without qualifying are the ones this returns.
func (Driver) Schema(ctx context.Context, db *sql.DB, database string) (*datasource.Schema, error) {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()

	const listing = `
		SELECT t.table_name, t.table_schema, t.table_type, c.column_name
		FROM information_schema.tables t
		JOIN information_schema.columns c
		  ON c.table_schema = t.table_schema AND c.table_name = t.table_name
		WHERE t.table_schema = ANY (current_schemas(false))
		ORDER BY t.table_name, c.ordinal_position`

	rows, err := db.QueryContext(ctx, listing)
	if err != nil {
		return nil, fmt.Errorf("read the database's tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	schema := &datasource.Schema{Database: database}
	at := map[string]int{}
	for rows.Next() {
		var name, namespace, kind, column string
		if err := rows.Scan(&name, &namespace, &kind, &column); err != nil {
			return nil, fmt.Errorf("read the database's tables: %w", err)
		}
		i, seen := at[strings.ToLower(name)]
		if !seen {
			schema.Tables = append(schema.Tables, datasource.TableSchema{
				Name: name, Namespace: namespace, View: !strings.EqualFold(kind, "BASE TABLE"),
			})
			i = len(schema.Tables) - 1
			at[strings.ToLower(name)] = i
		}
		schema.Tables[i].Columns = append(schema.Tables[i].Columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the database's tables: %w", err)
	}

	const definitions = `
		SELECT table_name, view_definition
		FROM information_schema.views
		WHERE table_schema = ANY (current_schemas(false))`

	views, err := db.QueryContext(ctx, definitions)
	if err != nil {
		return nil, fmt.Errorf("read the database's views: %w", err)
	}
	defer func() { _ = views.Close() }()

	for views.Next() {
		var name string
		var definition sql.NullString
		if err := views.Scan(&name, &definition); err != nil {
			return nil, fmt.Errorf("read the database's views: %w", err)
		}
		if i, ok := at[strings.ToLower(name)]; ok {
			schema.Tables[i].View = true
			schema.Tables[i].Definition = definition.String
		}
	}
	if err := views.Err(); err != nil {
		return nil, fmt.Errorf("read the database's views: %w", err)
	}
	return schema, nil
}

// normalizeRow turns driver values into JSON-friendly ones: text and numerics
// arrive as []byte, which becomes a string; NULL stays nil; everything else
// passes through.
func normalizeRow(cells []any) []any {
	out := make([]any, len(cells))
	for i, c := range cells {
		if b, ok := c.([]byte); ok {
			out[i] = string(b)
			continue
		}
		out[i] = c
	}
	return out
}
