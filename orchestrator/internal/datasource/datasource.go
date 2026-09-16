// Package datasource is the shared way anything in the project connects to an
// external database. It is not tied to the query tool: a workflow that reads or
// writes a database uses the same drivers, the same connection config, and the
// same execution helpers. A driver knows one kind of database (how to build its
// pool, and what settings it needs); the registry holds the drivers this build
// ships; Open dispatches to the right one.
//
// This is deliberately separate from internal/store/sqldb, which is how the
// product talks to its OWN database. datasource is how it talks to a customer's.
package datasource

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// Config is a connection to an external database. Secret fields (a password, a
// client key) travel here in the clear only in memory: at rest they are sealed
// inside the owning tool's config, opened just before a Config is built.
type Config struct {
	Driver   string `json:"driver"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
	TLS      TLS    `json:"tls"`
	// SSH, when set, reaches the database through a bastion: the connection is
	// tunnelled and the driver dials a local address knowing nothing about SSH.
	SSH *SSHConfig `json:"ssh,omitempty"`
	// Reach, when set, carries the connection through somewhere this server
	// cannot go: today the chat application, on a computer that can see the
	// database. Not stored with the rest: it is a live capability of whoever is
	// asking, not a setting somebody typed.
	Reach *Reach `json:"-"`
}

// TLS is how a connection is secured. Mode "disable" is plain, "require"
// encrypts without verifying the server, "verify" encrypts and verifies it. A
// CA certificate verifies a server signed by a private authority; a client
// certificate and key authenticate this end (mutual TLS); a server name
// overrides the name checked against the certificate when it differs from the
// host. ClientKey is a secret, sealed at rest.
type TLS struct {
	Mode       string `json:"mode"`
	ServerName string `json:"server_name,omitempty"`
	CACert     string `json:"ca_cert,omitempty"`
	ClientCert string `json:"client_cert,omitempty"`
	ClientKey  string `json:"client_key,omitempty"`
}

// FieldType tells the admin form how to render a config field.
type FieldType string

const (
	FieldText     FieldType = "text"
	FieldNumber   FieldType = "number"
	FieldPassword FieldType = "password"
	FieldSelect   FieldType = "select"
	FieldTextarea FieldType = "textarea"
)

// Field describes one configuration input a driver needs, so the admin form can
// be built from the driver rather than hard-coded per driver. Secret fields are
// sealed at rest and write-only in the form.
//
// Span is the field's width in a six-column row, so the driver decides the
// layout (a port sits narrow beside a wide host) rather than the console
// guessing. Zero means a sensible default; a textarea always takes the full row.
type Field struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	Secret   bool      `json:"secret,omitempty"`
	// Options are the choices in a select: what is stored, and what a person
	// reads. Two different things, and they used to be one string.
	Options []Option `json:"options,omitempty"`
	Default string   `json:"default,omitempty"`
	Help    string   `json:"help,omitempty"`
	Span    int      `json:"span,omitempty"`
	// Endpoint marks a field as part of the connection address (host, port), so a
	// template can weave its own fields in right after them without knowing the
	// driver's field names. Internal to form composition; never sent to the client.
	Endpoint bool `json:"-"`
}

// Option is one choice in a select: the value stored, and the words shown.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Choice pairs a stored value with what it says to a person.
func Choice(value, label string) Option { return Option{Value: value, Label: label} }

// Driver is one kind of database, end to end: how to connect to it, how to
// query it, and what settings it needs. Connecting and querying are both on the
// driver, not generalized in the shared package, because databases genuinely
// differ (how a pool is built, how types come back, what a read-only run means).
// A driver registers itself from an init(). Callers do not use a Driver
// directly; they Open a Conn, which routes to it.
type Driver interface {
	Key() string
	Label() string
	DefaultPort() int
	// Dialect describes the engine's SQL flavour, so a tool can tell the model
	// which database it is talking to and how to introspect it.
	Dialect() Dialect
	// Fields are the connection settings this driver needs, in form order.
	Fields() []Field
	// Capabilities are the things this engine can be ALLOWED to do beyond
	// reading and writing rows, each one off until an administrator turns it on.
	//
	// It is on the driver because the answer differs per engine and nowhere
	// else: calling a stored routine is CALL on MySQL and EXEC on SQL Server,
	// and an engine with none returns nothing and shows no checkboxes. A new
	// database declares its own here and the form grows by itself, the same way
	// its connection fields already do.
	Capabilities() []Capability
	// Connect builds a pool for a config. It does not verify the connection;
	// Conn.Ping does.
	//
	// This and the three below are exported because a driver is a package of its
	// own (one database, one folder), and a package cannot satisfy an interface
	// whose methods are unexported elsewhere. Callers still reach a database
	// through Open and a Conn; nothing outside this package has reason to hold a
	// Driver and call these directly.
	Connect(cfg Config) (*sql.DB, error)
	// query runs a read statement and returns its rows, capped at limit. A read
	// cancelled by its context (a deadline the caller set) is stopped on the
	// server and returned as a *QueryTimeout carrying the statement's plan.
	Query(ctx context.Context, db *sql.DB, statement string, args []any, limit int) (*Result, error)
	// exec runs a write statement and returns what it changed.
	Exec(ctx context.Context, db *sql.DB, statement string, args []any) (*Result, error)
	// schema reads what the database holds: its tables and views, and the columns
	// of each. Where the answer lives is the engine's own business, which is why
	// this is on the driver rather than a query somebody writes once.
	Schema(ctx context.Context, db *sql.DB, database string) (*Schema, error)
}

// Schema is what a database says it holds. It is the one shared shape a driver
// fills, so a caller reads a schema the same way whatever is underneath.
type Schema struct {
	Database string
	Tables   []TableSchema
	// Routines are the stored procedures and functions this database holds, with
	// the body of each. They are here for one reason: a statement that CALLS one
	// says nothing about what it does, so the only way to hold a routine to a
	// policy is to read what it was written as. A body the connected account may
	// not see comes back empty, which leaves the routine unaccounted for and so
	// refused, rather than assumed harmless.
	Routines []Routine
}

// Routine is one stored procedure or function, as the database reports it.
type Routine struct {
	Name string
	// Namespace is the schema it lives in, on an engine that has them.
	Namespace string
	// Body is the statement it was created with. Empty when the account may not
	// read it, or the engine encrypted it: both mean unaccountable, not safe.
	Body string
}

// TableSchema is one table or view, as the database reports it.
type TableSchema struct {
	Name string
	// Namespace is the schema the table lives in, for a database that has more
	// than one. It is empty where the engine does not separate the two.
	Namespace string
	View      bool
	// Columns are in the order the database returns them, which is the order a
	// SELECT * stands for.
	Columns []string
	// Definition is the statement a view is defined by, and is empty for a real
	// table. It can also be empty for a view the connected account is not allowed
	// to see the definition of, which is a different thing entirely and is why a
	// caller that cares treats an empty one as unknown rather than as harmless.
	Definition string
}

// Dialect describes a driver's SQL flavour, for a tool to teach the model which
// database it is querying and how to work with it.
type Dialect struct {
	// Name is the engine as a person and the model would name it, e.g.
	// "MySQL / MariaDB".
	Name string
	// SQLNote is a one-line reminder of the flavour, for a tool's description:
	// enough that the model does not guess the wrong dialect.
	SQLNote string
	// Introspection is how to list tables and columns in this engine, for the
	// tool's deeper guide.
	Introspection string
}

// Capability is something an administrator may switch on for one tool, beyond
// the rows it reads and writes.
//
// Everything here is OFF unless it is ticked. That direction is the whole point:
// every tool that existed before a capability was invented goes on behaving
// exactly as it did, and the safe answer is the one nobody has to remember.
type Capability struct {
	// Key is what the setting is stored as, and must not change once shipped.
	Key   string
	Label string
	Help  string
	// Verbs are the leading keywords this unlocks in the access gate. A
	// capability with none gates something the gate cannot see by its first
	// word, and is enforced by whatever does see it.
	Verbs []string
	// Objects are the kinds of thing CREATE, ALTER and DROP may name under this
	// capability. CREATE is already a write, so the leading word cannot separate
	// CREATE TABLE from CREATE TRIGGER; this is what does. An object kind no
	// capability claims is left exactly as it was.
	Objects []string
	// Warn is said in the form when ticking this costs something an
	// administrator should decide deliberately rather than discover.
	Warn string
}

// HasCapability reports whether a driver offers one, so a caller can ask before
// acting on a setting an engine may not have.
func HasCapability(driverKey, capability string) bool {
	d, ok := Get(driverKey)
	if !ok {
		return false
	}
	for _, c := range d.Capabilities() {
		if c.Key == capability {
			return true
		}
	}
	return false
}

// The capabilities this product knows how to reason about. A driver names the
// ones it can offer; these constants are what the rest of the code matches on,
// so a typo is a build failure rather than a setting that silently does
// nothing.
const (
	// CapCallRoutines allows calling a stored procedure or function.
	CapCallRoutines = "call_routines"
	// CapCreateRoutines allows creating or altering one.
	CapCreateRoutines = "create_routines"
	// CapCreateTriggers allows creating a trigger, which is the one capability
	// that puts work beyond any reading of a statement: a trigger runs on
	// somebody else's write, and no policy can see what it does.
	CapCreateTriggers = "create_triggers"
)

// QueryTimeout is returned by a read that ran past its deadline and was
// cancelled on the server, so it could not keep holding the database. Plan is
// the statement's EXPLAIN output, so a caller can show why it was slow (a full
// scan, a missing index) and narrow it, rather than only that it was slow.
type QueryTimeout struct {
	Plan string
}

func (e *QueryTimeout) Error() string { return "the query timed out and was cancelled" }

// Result is the outcome of a statement: for a read, the columns and rows (with a
// flag when they were capped); for a write, how many rows changed. It is the one
// shared shape a driver fills, so a tool and a workflow read a result the same
// way whatever the database underneath.
type Result struct {
	Columns   []string `json:"columns,omitempty"`
	Rows      [][]any  `json:"rows,omitempty"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated,omitempty"`
	Affected  *int64   `json:"rows_affected,omitempty"`
	InsertID  *int64   `json:"insert_id,omitempty"`
	// MoreResults says the statement produced another result set that is not
	// here. One statement per call is the rule and one result set follows from
	// it; what does not follow is losing the others in silence.
	MoreResults bool `json:"more_results,omitempty"`
	// BinaryColumns are the columns whose bytes were not returned. Named so a
	// caller can say it once, rather than leaving a model to work it out from
	// every cell of every row.
	BinaryColumns []string `json:"binary_columns,omitempty"`
	// Note is what a caller must be TOLD rather than left to infer from a flag.
	// A model that does not read `truncated` reads a capped answer as the whole
	// one, and nothing anywhere says otherwise.
	Note string `json:"note,omitempty"`
}

// Conn is an open connection to a datasource: query or execute through it, and
// Close it when done, which also tears down any SSH tunnel behind it. It hides
// which driver is underneath, so a caller writes the same code for every
// database.
type Conn struct {
	db       *sql.DB
	driver   Driver
	database string
	cleanup  func() error
}

// Query runs a read statement, capped at limit (limit <= 0 means the driver's
// own default). Exec runs a write statement.
func (c *Conn) Query(ctx context.Context, statement string, args []any, limit int) (*Result, error) {
	return c.driver.Query(ctx, c.db, statement, args, limit)
}

func (c *Conn) Exec(ctx context.Context, statement string, args []any) (*Result, error) {
	return c.driver.Exec(ctx, c.db, statement, args)
}

// Ping verifies the connection is usable before a statement runs.
func (c *Conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// Schema reads what this connection's database holds, for a caller that has to
// know its tables and columns rather than guess at them.
func (c *Conn) Schema(ctx context.Context) (*Schema, error) {
	return c.driver.Schema(ctx, c.db, c.database)
}

// Close closes the pool and the tunnel behind it.
func (c *Conn) Close() error {
	dbErr := c.db.Close()
	tunErr := c.cleanup()
	if dbErr != nil {
		return dbErr
	}
	return tunErr
}

var (
	mu      sync.RWMutex
	drivers = map[string]Driver{}
)

// Register adds a driver. Driver packages call it from init().
func Register(d Driver) {
	mu.Lock()
	defer mu.Unlock()
	drivers[d.Key()] = d
}

// Get returns a driver by key.
func Get(key string) (Driver, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[key]
	return d, ok
}

// All returns the registered drivers, for the admin's driver picker.
func All() []Driver {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Driver, 0, len(drivers))
	for _, d := range drivers {
		out = append(out, d)
	}
	return out
}

// SecretFieldKeys returns the config keys a driver seals, so the app knows what
// to encrypt before storing and open before connecting.
func SecretFieldKeys(driverKey string) []string {
	d, ok := Get(driverKey)
	if !ok {
		return nil
	}
	var keys []string
	for _, f := range d.Fields() {
		if f.Secret {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

// Open connects to a datasource, dispatching to its driver and going through an
// SSH tunnel when one is configured. Close the returned Conn when done: it drops
// the pool and the tunnel together, so a bastion connection is never left open.
func Open(cfg Config) (*Conn, error) {
	d, ok := Get(cfg.Driver)
	if !ok {
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}

	// The two ways of reaching a database COMPOSE, and that is not a detail: a
	// bastion on somebody's office network is exactly as unreachable from here
	// as the database behind it. So a tool set to both signs in to the jump host
	// through the chat application, and tunnels to the database from there.
	cleanup := func() error { return nil }
	switch {
	case cfg.SSH != nil && strings.TrimSpace(cfg.SSH.Host) != "":
		// The reach, when there is one, carries the SSH connection itself.
		t, host, port, err := openTunnel(*cfg.SSH, cfg.Host, cfg.Port, cfg.Reach)
		if err != nil {
			return nil, err
		}
		// The driver now dials the local end of the tunnel.
		cfg.Host, cfg.Port, cfg.SSH, cfg.Reach = host, port, nil, nil
		cleanup = t.Close
	case cfg.Reach != nil:
		f, host, port, err := openReach(*cfg.Reach, cfg.Host, cfg.Port)
		if err != nil {
			return nil, err
		}
		cfg.Host, cfg.Port, cfg.Reach = host, port, nil
		cleanup = f.Close
	}

	db, err := d.Connect(cfg)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	return &Conn{db: db, driver: d, database: cfg.Database, cleanup: cleanup}, nil
}
