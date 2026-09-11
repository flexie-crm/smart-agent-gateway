package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/datasource"
	driver "github.com/go-sql-driver/mysql"
)

// defaultRows caps a read that did not ask for a limit, so a careless
// SELECT does not stream an unbounded result back.
const defaultRows = 500

// The MySQL / MariaDB driver. It registers itself, so adding a driver to a build
// is importing its file, and the admin's driver picker and the connection code
// both learn about it from one place.
func init() { datasource.Register(Driver{}) }

// Driver is the MySQL / MariaDB driver.
type Driver struct{}

func (Driver) Key() string      { return "mysql" }
func (Driver) Label() string    { return "MySQL / MariaDB" }
func (Driver) DefaultPort() int { return 3306 }

func (Driver) Dialect() datasource.Dialect {
	return datasource.Dialect{
		Name:    "MySQL / MariaDB",
		SQLNote: "Write MySQL/MariaDB SQL: backtick-quote identifiers, and introspect with SHOW or information_schema, not sqlite_Gateway or PRAGMA.",
		Introspection: "List tables with `SHOW TABLES`. See a table's columns with `DESCRIBE <table>` or `SHOW CREATE TABLE <table>`. " +
			"Read schema metadata from `information_schema.tables` and `information_schema.columns`.",
	}
}

// Fields is what a MySQL connection needs. The admin form is built from this, so
// the query template does not hard-code MySQL's shape: it asks the driver.
func (Driver) Fields() []datasource.Field {
	return []datasource.Field{
		// The endpoint (host, port) leads, on its own row, port narrower than
		// host; the template weaves the access selector in after it, so the form
		// reads host | port, then access | database, then username | password.
		{Key: "host", Label: "Host", Type: datasource.FieldText, Required: true, Span: 4, Endpoint: true, Help: "Hostname or IP of the database server."},
		{Key: "port", Label: "Port", Type: datasource.FieldNumber, Default: "3306", Span: 2, Endpoint: true},
		{Key: "database", Label: "Database", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "username", Label: "Username", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "password", Label: "Password", Type: datasource.FieldPassword, Secret: true, Span: 3},
	}
}

// registerTLS turns the connection's datasource.TLS settings into the driver's TLSConfig
// name. Plain modes map to the driver's built-in names; anything with a CA, a
// client certificate, or a server name is built into a *tls.Config and
// registered under a name derived from its contents (so the same material
// registers once and different material never collides).
func registerTLS(t datasource.TLS) (string, error) {
	switch t.Mode {
	case "", "disable":
		return "", nil
	case "require", "verify":
	default:
		return "", fmt.Errorf("unknown datasource.TLS mode %q", t.Mode)
	}
	if !datasource.CustomTLS(t) {
		if t.Mode == "require" {
			return "skip-verify", nil
		}
		return "true", nil
	}
	cfg, err := datasource.TLSConfig(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(t.Mode + "\x00" + t.ServerName + "\x00" + t.CACert + "\x00" + t.ClientCert + "\x00" + t.ClientKey))
	name := "sag-" + hex.EncodeToString(sum[:8])
	if err := driver.RegisterTLSConfig(name, cfg); err != nil {
		return "", fmt.Errorf("register datasource.TLS: %w", err)
	}
	return name, nil
}

func (Driver) Connect(cfg datasource.Config) (*sql.DB, error) {
	dsn := driver.NewConfig()
	dsn.User = cfg.Username
	dsn.Passwd = cfg.Password
	dsn.Net = "tcp"
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	dsn.Addr = fmt.Sprintf("%s:%d", cfg.Host, port)
	dsn.DBName = cfg.Database
	dsn.Loc = time.UTC
	dsn.ParseTime = true
	// Stacked statements off at the driver: the hard floor under the query
	// tool's own gate. InterpolateParams off keeps arguments as bound parameters.
	dsn.MultiStatements = false
	dsn.InterpolateParams = false

	name, err := registerTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	if name != "" {
		dsn.TLSConfig = name
	}

	db, err := sql.Open("mysql", dsn.FormatDSN())
	if err != nil {
		return nil, err
	}
	// A query tool is not a high-throughput path, and a bounded pool cannot
	// exhaust the target's connections.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(2 * time.Minute)
	return db, nil
}

// query runs a read statement inside a read-only transaction and returns its
// rows, capped. The read-only transaction holds even a writable credential to
// reading for the length of the query.
func (Driver) Query(ctx context.Context, db *sql.DB, statement string, args []any, limit int) (*datasource.Result, error) {
	if limit <= 0 {
		limit = defaultRows
	}

	// A dedicated connection, so its id is known: if the read outlives its
	// deadline it is killed on the server by that id, rather than left running
	// and holding the database while the caller has already walked away.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var connID int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err != nil {
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
			return nil, cancelled(db, connID, statement, args)
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
		ptrs := make([]any, len(cols))
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
			return nil, cancelled(db, connID, statement, args)
		}
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return res, nil
}

// cancelled stops a read that ran past its deadline and explains it: the
// statement is killed on the server by its connection id (so it stops holding
// the database), then EXPLAIN is run for the same statement so the caller learns
// why it was slow. Both use a fresh, short-lived context, because the read's own
// is already done.
func cancelled(db *sql.DB, connID int64, statement string, args []any) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	// KILL QUERY stops the running statement but keeps the connection alive. The
	// id is ours (from SELECT CONNECTION_ID()), never user input, so it is
	// formatted in directly; a query that already finished is an ignorable error.
	_, _ = db.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", connID))

	return &datasource.QueryTimeout{Plan: explain(ctx, db, statement, args)}
}

// explain returns the statement's query plan as a compact text table, or a
// note when the plan itself could not be read.
func explain(ctx context.Context, db *sql.DB, statement string, args []any) string {
	rows, err := db.QueryContext(ctx, "EXPLAIN "+statement, args...)
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	var b strings.Builder
	b.WriteString(strings.Join(cols, " | "))
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			break
		}
		vals := make([]string, len(cols))
		for i, c := range normalizeRow(cells) {
			if c == nil {
				vals[i] = "NULL"
			} else {
				vals[i] = fmt.Sprint(c)
			}
		}
		b.WriteString("\n")
		b.WriteString(strings.Join(vals, " | "))
	}
	if err := rows.Err(); err != nil {
		b.WriteString("\n(the plan may be incomplete: ")
		b.WriteString(err.Error())
		b.WriteString(")")
	}
	return b.String()
}

// exec runs a write statement and returns what it changed.
func (Driver) Exec(ctx context.Context, db *sql.DB, statement string, args []any) (*datasource.Result, error) {
	out, err := db.ExecContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("run statement: %w", err)
	}
	res := &datasource.Result{}
	if n, err := out.RowsAffected(); err == nil {
		res.Affected = &n
	}
	if id, err := out.LastInsertId(); err == nil && id > 0 {
		res.InsertID = &id
	}
	return res, nil
}

// normalizeRow turns MySQL driver values into JSON-friendly ones: the
// driver hands text and decimals back as []byte, which becomes a string; NULL
// stays nil; everything else passes through.
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

// schemaTimeout bounds reading the catalog. It is not a statement somebody
// wrote, so it gets its own ceiling rather than a caller's.
const schemaTimeout = 15 * time.Second

// schema reads the database's own account of itself: every table and view, the
// columns of each in the order a SELECT * stands for, and the statement a view
// is defined by.
//
// Two reads rather than one. The columns come back joined to their table so a
// table with no columns visible to this account simply does not appear, and the
// view definitions come separately because an account without the privilege to
// see them gets an empty string rather than an error: a view whose definition
// could not be read is a view nobody here can vouch for, and the caller is left
// able to tell that apart from a view that reads nothing.
func (Driver) Schema(ctx context.Context, db *sql.DB, database string) (*datasource.Schema, error) {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()

	const listing = `
		SELECT t.TABLE_NAME, t.TABLE_TYPE, c.COLUMN_NAME
		FROM information_schema.TABLES t
		JOIN information_schema.COLUMNS c
		  ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_SCHEMA = ?
		ORDER BY t.TABLE_NAME, c.ORDINAL_POSITION`

	rows, err := db.QueryContext(ctx, listing, database)
	if err != nil {
		return nil, fmt.Errorf("read the database's tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	schema := &datasource.Schema{Database: database}
	at := map[string]int{}
	for rows.Next() {
		var name, kind, column string
		if err := rows.Scan(&name, &kind, &column); err != nil {
			return nil, fmt.Errorf("read the database's tables: %w", err)
		}
		i, seen := at[strings.ToLower(name)]
		if !seen {
			schema.Tables = append(schema.Tables, datasource.TableSchema{Name: name, View: !strings.EqualFold(kind, "BASE TABLE")})
			i = len(schema.Tables) - 1
			at[strings.ToLower(name)] = i
		}
		schema.Tables[i].Columns = append(schema.Tables[i].Columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the database's tables: %w", err)
	}

	const definitions = `SELECT TABLE_NAME, VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?`
	views, err := db.QueryContext(ctx, definitions, database)
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
