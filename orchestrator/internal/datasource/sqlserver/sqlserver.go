// Package sqlserver is how this product reaches a Microsoft SQL Server
// database: how to connect to one, run a statement on it, and ask it what it
// holds. One database is one package, so what is true of SQL Server and nothing
// else lives here.
package sqlserver

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"flexie.io/sag/internal/datasource"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// defaultRows caps a read that did not ask for a limit, so a careless SELECT
// does not stream an unbounded result back.
const defaultRows = 500

// schemaTimeout bounds reading the catalog. It is not a statement somebody
// wrote, so it gets its own ceiling rather than a caller's.
const schemaTimeout = 15 * time.Second

// The SQL Server driver. It registers itself, so a build can reach a SQL Server
// database only if something imports this package.
func init() { datasource.Register(Driver{}) }

// Driver is the Microsoft SQL Server driver.
type Driver struct{}

func (Driver) Key() string      { return "sqlserver" }
func (Driver) Label() string    { return "Microsoft SQL Server" }
func (Driver) DefaultPort() int { return 1433 }

func (Driver) Dialect() datasource.Dialect {
	return datasource.Dialect{
		Name: "Microsoft SQL Server",
		SQLNote: "Write Transact-SQL: bracket-quote identifiers that need quoting ([Order Details]), put values in " +
			"@p1, @p2 placeholders rather than ? or $1, limit rows with TOP or OFFSET/FETCH rather than LIMIT, " +
			"and introspect with INFORMATION_SCHEMA, not SHOW TABLES.",
		Introspection: "List tables with `SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = SCHEMA_NAME()`. " +
			"See a table's columns with `SELECT COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = @p1`. " +
			"A database holds many schemas; an unqualified name resolves to your default schema and then to dbo, " +
			"which is where most tables live.",
	}
}

// Fields is what a SQL Server connection needs. The admin form is built from
// this, so the query template does not hard-code the shape: it asks the driver.
func (Driver) Fields() []datasource.Field {
	return []datasource.Field{
		// The endpoint leads, port narrower than host; the template weaves its
		// access selector in after it.
		{Key: "host", Label: "Host", Type: datasource.FieldText, Required: true, Span: 4, Endpoint: true, Help: "Hostname or IP of the database server."},
		{Key: "port", Label: "Port", Type: datasource.FieldNumber, Default: "1433", Span: 2, Endpoint: true},
		{Key: "database", Label: "Database", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "username", Label: "Username", Type: datasource.FieldText, Required: true, Span: 3},
		{Key: "password", Label: "Password", Type: datasource.FieldPassword, Secret: true, Span: 3},
	}
}

// Capabilities is what a SQL Server tool may be allowed beyond rows.
//
// The verbs are this engine's: a routine is called with EXEC or EXECUTE, where
// MySQL and PostgreSQL say CALL.
func (Driver) Capabilities() []datasource.Capability {
	return []datasource.Capability{
		{
			Key: datasource.CapCallRoutines, Label: "Call stored procedures and functions",
			Verbs: []string{"EXEC", "EXECUTE"},
			Help:  "The tool may run a stored procedure or function. Its body is read first and held to the rules above: one that reads a table you keep back, or returns a hidden field, is refused.",
		},
		{
			Key: datasource.CapCreateRoutines, Label: "Create procedures and functions",
			// PROC is T-SQL's own short spelling, and it is a different word.
			Objects: []string{"PROCEDURE", "PROC", "FUNCTION"},
			Help:    "The tool may create, change and remove stored procedures and functions. The body is checked against the rules above before it is created.",
		},
		{
			Key: datasource.CapCreateTriggers, Label: "Create triggers",
			Objects: []string{"TRIGGER"},
			Warn:    "A trigger runs on somebody else's write, where these rules cannot see it. Tick this only where the tool is trusted with the whole database.",
		},
	}
}

// transport turns the connection's TLS settings into what this driver connects
// with: the encryption level, and the tls.Config to do it with. It is a function
// of its own so it can be tested without a server, because what these settings
// become is the whole of what an administrator asking for TLS gets, and it is
// not visible from a *sql.DB.
//
// The three modes mean the same here as on every other driver
// (internal/datasource/tls.go). What is specific to this one is that the
// encryption level and the certificate settings are TWO separate things, and
// getting either wrong is silent.
//
// The level is always said out loud, and that is the point rather than a
// formality. msdsn.Config's zero value is EncryptionOff, and EncryptionOff does
// not mean off: the driver performs a TLS handshake for the login packet and
// then puts the plain socket back underneath (go-mssqldb tds.go, "if encrypt ==
// encryptOff { outbuf.afterFirst = ... }"). So a config that simply neglects to
// set this encrypts the password and sends every row of every answer in the
// clear. Nothing reports it. "disable" therefore maps to EncryptionDisabled,
// which is the value that really means none, and never to the zero value.
//
// Unlike pgx (whose sslmode=prefer would connect unencrypted when TLS failed,
// and had to be un-made by hand), asking for encryption here does fail closed:
// the driver refuses a server that answers encryptOff or encryptNotSup with
// "server does not support encryption". So there is no fallback to clear away,
// only a default to refuse to inherit.
func transport(cfg datasource.Config) (msdsn.Encryption, *tls.Config, error) {
	switch cfg.TLS.Mode {
	case "", "disable":
		return msdsn.EncryptionDisabled, nil, nil
	case "require", "verify":
		out, err := datasource.TLSConfig(cfg.TLS)
		if err != nil {
			return msdsn.EncryptionDisabled, nil, err
		}
		if out.ServerName == "" {
			// The name to check the certificate against, when nobody said one.
			// Through an SSH bastion the host by then is the local end of the
			// tunnel, so verify needs the Server name field filled in with the
			// real one: this fails closed rather than quietly checking the
			// certificate against 127.0.0.1 and accepting it.
			out.ServerName = cfg.Host
		}
		return msdsn.EncryptionRequired, out, nil
	default:
		return msdsn.EncryptionDisabled, nil, fmt.Errorf("unknown TLS mode %q", cfg.TLS.Mode)
	}
}

// Connect builds a pool for a config.
//
// The TLS settings an administrator gave us (a CA, a client certificate, a
// server name) become a real tls.Config set as a field, rather than being
// written into a connection string and parsed back out of it. Only the address
// travels as text, for the reason the body explains.
func (Driver) Connect(cfg datasource.Config) (*sql.DB, error) {
	port := cfg.Port
	if port == 0 {
		port = 1433
	}

	encryption, tlsConfig, err := transport(cfg)
	if err != nil {
		return nil, err
	}

	// The config is PARSED from an address and then overridden, never built as a
	// struct literal, and that is not style. A literal leaves Protocols and
	// ProtocolParameters nil, which the library fills only in Parse, and the
	// first write on such a connection dereferences nil and takes the process
	// down. Found by connecting to a real server; nothing about the struct says
	// which of its fields are load-bearing.
	//
	// Only the address goes in the string. The credentials and the TLS settings
	// are set as fields afterwards, so a password with a % or an @ in it is never
	// something that has to survive being escaped into a URL and parsed back out.
	address := &url.URL{Scheme: "sqlserver", Host: net.JoinHostPort(cfg.Host, strconv.Itoa(port))}
	address.RawQuery = url.Values{"database": {cfg.Database}}.Encode()
	conn, err := msdsn.Parse(address.String())
	if err != nil {
		return nil, fmt.Errorf("read the connection settings: %w", err)
	}
	conn.User = cfg.Username
	conn.Password = cfg.Password
	conn.Encryption = encryption
	conn.TLSConfig = tlsConfig
	// Said explicitly rather than left to whatever the parser inferred from an
	// address with no encrypt setting on it, which is to trust any certificate.
	conn.TrustServerCertificate = cfg.TLS.Mode == "require"
	// Named after the product, not the library, the same as everywhere else: this
	// is what a DBA sees in sys.dm_exec_sessions when they ask who is asking.
	conn.AppName = "Flexie SAG"
	// database/sql retries a query that started on a connection the pool had
	// already lost. A retry of a statement we have decided about is a second run
	// of it, and this tool's statements are not all reads.
	conn.DisableRetry = true

	db := sql.OpenDB(settled{mssql.NewConnectorConfig(conn)})
	// A query tool is not a high-throughput path, and a bounded pool cannot
	// exhaust the target's connections.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(2 * time.Minute)
	return db, nil
}

// dialect is said on every connection before anything runs on it.
//
// QUOTED_IDENTIFIER decides what "x" IS. With it on, a double-quoted word is an
// identifier; with it off, it is a string literal. The policy layer reads a
// statement before the server does, and it reads "x" as an identifier, because
// that is what the grammar says (lib/tsql, DOUBLE_QUOTE_ID is an id_). A server
// with the setting off would therefore be running something this side had
// understood differently, the same shape as the MariaDB executable comment that
// cost a hole on the MySQL driver, and as standard_conforming_strings on the
// Postgres one.
//
// The divergence here happens to lean safe (this side reads an identifier where
// the server would read a harmless literal, so it over-hides rather than
// under-hides), but that is a property of today's grammar rather than a promise,
// and leaving a known disagreement in place to be re-derived later is how the
// first one survived. It is closed by making the server agree.
//
// It may well be on already: go-mssqldb sets the ODBC flag at login, which is
// documented to bring ANSI defaults with it. Whether that includes this one is
// UNVERIFIED (the specification was not to hand), and one round trip per
// connection is a cheap price for not needing to know.
const dialect = "SET QUOTED_IDENTIFIER ON"

// settled wraps the driver's connector so a connection is in a known state
// before the pool hands it out. A session setting cannot be set once for a pool:
// it belongs to a connection, and a pool makes new ones whenever it likes.
type settled struct{ driver.Connector }

func (s settled) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := s.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := settle(ctx, conn); err != nil {
		// A connection that could not be put into a known state is not handed
		// out. Using it anyway would mean reading statements one way while the
		// server read them another, which is the whole thing this prevents.
		_ = conn.Close()
		return nil, fmt.Errorf("settle how the database reads a statement: %w", err)
	}
	return conn, nil
}

// settle runs the session setting on a raw driver connection.
//
// It goes through Prepare rather than ExecContext because this driver's
// connection does not implement driver.ExecerContext: the shortest path to
// running a statement here is to prepare it. The interface checks are in the
// order of preference rather than assumed, so a future version of the library
// that grows the faster path is taken automatically.
func settle(ctx context.Context, conn driver.Conn) error {
	if execer, ok := conn.(driver.ExecerContext); ok {
		_, err := execer.ExecContext(ctx, dialect, nil)
		return err
	}
	var stmt driver.Stmt
	var err error
	if preparer, ok := conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, dialect)
	} else {
		stmt, err = conn.Prepare(dialect)
	}
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	if execer, ok := stmt.(driver.StmtExecContext); ok {
		_, err = execer.ExecContext(ctx, nil)
		return err
	}
	_, err = stmt.Exec(nil) //nolint:staticcheck // the fallback when the driver has no context path
	return err
}

// Query runs a read statement and returns its rows, capped.
//
// There is no read-only transaction here, and that is a real difference from the
// other two drivers rather than an omission: go-mssqldb refuses one outright
// ("read-only transactions are not supported", mssql.go BeginTx), because SQL
// Server has no session-level read-only mode to put it in. ReadOnlyIntent in the
// connection config is not one either; it routes to a replica in an availability
// group, and its own comment says it "does not make queries to most databases
// read-only". So what keeps a read a read here is the tool's access setting,
// checked before anything opens, and the permissions of the account it connects
// as. Both were always the real controls; on this engine they are the only ones.
//
// A read that outlives its deadline needs nothing done to it either, which is
// the other difference. Cancelling the context makes the driver send a TDS
// attention signal (go-mssqldb token.go, on ctx.Done), and the server stops the
// statement on the connection it is running on. MySQL and Postgres both need a
// second connection and a KILL to achieve that, and the account needs the
// permission to do it; here the protocol carries it.
func (Driver) Query(ctx context.Context, db *sql.DB, statement string, args []any, limit int) (*datasource.Result, error) {
	if limit <= 0 {
		limit = defaultRows
	}

	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &datasource.QueryTimeout{Plan: plan(db, statement, args)}
		}
		return nil, fmt.Errorf("run query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	types := columnTypeNames(rows, len(cols))

	res := &datasource.Result{Columns: cols, Rows: [][]any{}}
	for i, name := range types {
		if datasource.IsBinaryColumn(name) && i < len(cols) {
			res.BinaryColumns = append(res.BinaryColumns, cols[i])
		}
	}
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
		res.Rows = append(res.Rows, normalizeRow(cells, types))
		res.RowCount++
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, &datasource.QueryTimeout{Plan: plan(db, statement, args)}
		}
		return nil, fmt.Errorf("read rows: %w", err)
	}
	// A routine can end in several SELECTs. Only the first is returned, which is
	// the rule, but losing the rest without a word is not: say there were more.
	res.MoreResults = rows.NextResultSet()
	return res, nil
}

// plan returns the statement's query plan as text, so a caller shown a timeout
// can see why it was slow rather than only that it was. It runs on a fresh,
// short-lived context, because the read's own is already done.
//
// SHOWPLAN_ALL is SQL Server's EXPLAIN, and it is a session setting rather than
// a prefix: switched on, the connection stops running statements and starts
// describing them. That is why this takes a connection of its own and puts the
// setting back on the way out. Left on, it would turn the next statement on that
// pooled connection into a plan instead of an answer.
func plan(db *sql.DB, statement string, args []any) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_ALL ON"); err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	// Putting the setting back is not best-effort. A connection left with
	// SHOWPLAN on goes back into the pool looking like every other one, and the
	// next statement to pick it up is described instead of run: an answer that
	// is not wrong so much as not an answer. If it cannot be put back, the
	// connection is thrown away instead: returning driver.ErrBadConn from Raw is
	// how database/sql is told never to hand this one out again.
	defer func() {
		if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_ALL OFF"); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	rows, err := conn.QueryContext(ctx, statement, args...)
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return "(the query plan could not be read: " + err.Error() + ")"
	}
	// SHOWPLAN_ALL answers with a wide row per plan operator. StmtText is the
	// readable one and is the first column; the rest are costs nobody asked for.
	var b strings.Builder
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cells))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			break
		}
		line, _ := normalizeRow(cells, columnTypeNames(rows, len(cols)))[0].(string)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimRight(line, " \t"))
	}
	if err := rows.Err(); err != nil {
		b.WriteString("\n(the plan may be incomplete: " + err.Error() + ")")
	}
	return b.String()
}

// Exec runs a write statement and returns what it changed.
//
// The statement is PREPARED first, the same as on the Postgres driver, so the
// arguments stay bound parameters rather than being written into the text.
//
// What preparing does NOT do here is enforce one statement per call. Postgres
// refuses that outright ("cannot insert multiple commands into a prepared
// statement"); T-SQL is a batch language and sp_prepare takes a batch, so a
// semicolon is not a wall on this engine. The one-statement rule is therefore
// the guard's alone on SQL Server, where on the other two drivers it has a floor
// underneath it. That is worth knowing rather than assuming: a statement reaches
// this function only after internal/sqlguard has read it and found exactly one.
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
	// SQL Server has no last insert id of its own: a caller that wants the row
	// back asks for it with OUTPUT, which comes back as rows rather than as a
	// number here.
	return res, nil
}

// Schema reads the database's own account of itself: every table and view this
// connection can name without qualifying, the columns of each in the order a
// SELECT * stands for, and the statement a view is defined by.
//
// "Without qualifying" is two schemas, not the whole database. SQL Server
// resolves an unqualified name against the connected user's default schema and
// then against dbo, so those are the tables a statement can reach by name alone.
func (Driver) Schema(ctx context.Context, db *sql.DB, database string) (*datasource.Schema, error) {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()

	const listing = `
		SELECT t.TABLE_NAME, t.TABLE_SCHEMA, t.TABLE_TYPE, c.COLUMN_NAME
		FROM INFORMATION_SCHEMA.TABLES t
		JOIN INFORMATION_SCHEMA.COLUMNS c
		  ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.TABLE_SCHEMA IN (SCHEMA_NAME(), 'dbo')
		ORDER BY t.TABLE_NAME, c.ORDINAL_POSITION`

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

	// A view's definition comes from sys.sql_modules, NOT from
	// INFORMATION_SCHEMA.VIEWS, and the difference is load-bearing.
	// VIEW_DEFINITION there is nvarchar(4000): Microsoft's own documentation says
	// a longer definition is truncated at 4000, and directs you here instead.
	// A truncated definition is the dangerous shape, worse than none at all,
	// because it can still parse, and what it parses to is a view that reads
	// fewer tables than the real one does. Absent is safe: the guard leaves such
	// a view unaccounted for and refuses it. Nearly right is not.
	//
	// A view this account may not read the definition of comes back NULL, which
	// is that same safe absence rather than an error.
	const definitions = `
		SELECT v.TABLE_NAME, m.definition
		FROM INFORMATION_SCHEMA.VIEWS v
		LEFT JOIN sys.sql_modules m
		  ON m.object_id = OBJECT_ID(QUOTENAME(v.TABLE_SCHEMA) + '.' + QUOTENAME(v.TABLE_NAME))
		WHERE v.TABLE_SCHEMA IN (SCHEMA_NAME(), 'dbo')`

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
	// The routines, with their bodies, for the same reason the view definitions
	// above are read: a statement that calls one says nothing about what it
	// does. sys.sql_modules is the only place the full text lives; an encrypted
	// or unreadable body comes back NULL, which leaves the routine
	// unaccountable and therefore refused.
	const routines = `
		SELECT s.name, o.name, m.definition
		FROM sys.objects o
		JOIN sys.schemas s ON s.schema_id = o.schema_id
		LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
		WHERE o.type IN ('P', 'FN', 'IF', 'TF')
		  AND s.name IN (SCHEMA_NAME(), 'dbo')`

	routineRows, err := db.QueryContext(ctx, routines)
	if err != nil {
		return nil, fmt.Errorf("read the database's routines: %w", err)
	}
	defer func() { _ = routineRows.Close() }()

	for routineRows.Next() {
		var namespace, name string
		var body sql.NullString
		if err := routineRows.Scan(&namespace, &name, &body); err != nil {
			return nil, fmt.Errorf("read the database's routines: %w", err)
		}
		schema.Routines = append(schema.Routines, datasource.Routine{
			Name: name, Namespace: namespace, Body: body.String,
		})
	}
	if err := routineRows.Err(); err != nil {
		return nil, fmt.Errorf("read the database's routines: %w", err)
	}

	return schema, nil
}

// normalizeRow turns driver values into JSON-friendly ones: text arrives as
// []byte, which becomes a string; NULL stays nil; everything else passes
// through.
// normalizeRow turns driver values into ones fit to put in front of a model,
// using the type the database declared for each column. datasource.Cell is
// shared by every driver so that a BLOB and a DATE mean the same thing whichever
// engine they came from.
func normalizeRow(cells []any, types []string) []any {
	out := make([]any, len(cells))
	for i, c := range cells {
		name := ""
		if i < len(types) {
			name = types[i]
		}
		out[i] = datasource.Cell(c, name)
	}
	return out
}

// columnTypeNames is what the database calls each column, or nothing when the
// driver will not say.
func columnTypeNames(rows *sql.Rows, n int) []string {
	infos, err := rows.ColumnTypes()
	if err != nil || len(infos) != n {
		return nil
	}
	out := make([]string, n)
	for i, info := range infos {
		out[i] = info.DatabaseTypeName()
	}
	return out
}
