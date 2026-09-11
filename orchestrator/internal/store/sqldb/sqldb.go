// Package sqldb is the only way this codebase talks to the database.
//
// Every query goes through here, and every query is a real prepared statement
// with bound parameters. That is not a convention anybody has to remember: it
// is enforced by the type system.
//
// # Why a Statement type
//
// The methods take a Statement, not a string. Go converts an untyped constant
// to a named string type implicitly, but it will NOT convert a string
// variable. So this compiles:
//
//	db.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ?`, name, id)
//
// and this does not:
//
//	query := "UPDATE users SET name = '" + name + "' WHERE id = 1"
//	db.ExecContext(ctx, query, ...)   // compile error: cannot use query (string)
//
// The second line is SQL injection. It is now a build failure rather than a
// code review someone has to catch. Values reach the database as bound
// parameters or they do not reach it at all.
//
// Concatenating constants still works (`SELECT ` + columns + ` FROM users`,
// where columns is a const), because a constant expression is still a
// constant. Nothing a user typed can ever become one.
//
// # Why prepared statements
//
// Parameter binding alone would already be safe. Preparing on top of it means
// the database parses each query once and we send only the values afterwards,
// which is faster under load and makes the safety property visible in the wire
// protocol rather than a promise in a comment. Statements are cached per
// query, so the preparation happens once per process, not once per call.
package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Statement is a SQL query. Only a compile-time constant can become one, which
// is what makes a runtime-assembled query impossible to pass.
type Statement string

// DB is a connection pool with a cache of prepared statements.
type DB struct {
	db *sql.DB

	mu    sync.RWMutex
	stmts map[Statement]*sql.Stmt
}

func New(db *sql.DB) *DB {
	return &DB{db: db, stmts: make(map[Statement]*sql.Stmt)}
}

// Raw exposes the pool for the two callers that legitimately need it: the
// migration runner, which executes schema DDL that is not a parameterized
// query, and tests that need to inspect the database directly. Application
// code must never reach for it: everything it wants is above.
func (d *DB) Raw() *sql.DB { return d.db }

func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Close releases the cached statements before the pool they belong to.
func (d *DB) Close() error {
	d.mu.Lock()
	for _, stmt := range d.stmts {
		_ = stmt.Close()
	}
	d.stmts = make(map[Statement]*sql.Stmt)
	d.mu.Unlock()

	return d.db.Close()
}

func (d *DB) QueryContext(ctx context.Context, q Statement, args ...any) (*sql.Rows, error) {
	stmt, err := d.prepare(ctx, q)
	if err != nil {
		return nil, err
	}
	return stmt.QueryContext(ctx, args...)
}

// QueryRowContext mirrors database/sql: a Row carries exactly one error, and it
// surfaces on Scan, so callers keep the one-liner they are used to.
func (d *DB) QueryRowContext(ctx context.Context, q Statement, args ...any) *sql.Row {
	stmt, err := d.prepare(ctx, q)
	if err != nil {
		// The prepare failed, which means the SQL itself is wrong. Running it
		// through the pool fails the same way and puts THAT error on the Row,
		// where the caller already handles it, instead of inventing one that
		// hides what actually happened. The parameters are still bound: the
		// safety property never depended on the cache.
		return d.db.QueryRowContext(ctx, string(q), args...)
	}
	return stmt.QueryRowContext(ctx, args...)
}

func (d *DB) ExecContext(ctx context.Context, q Statement, args ...any) (sql.Result, error) {
	stmt, err := d.prepare(ctx, q)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}

// Tx runs fn inside a transaction, and cannot leak one.
//
// The caller does not commit or roll back: fn returns an error and the
// transaction is rolled back, or it returns nil and the transaction is
// committed. A forgotten rollback is one of the easiest ways to wedge a
// connection pool, and it is not possible from here.
func (d *DB) Tx(ctx context.Context, fn func(context.Context, *Tx) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = d.tx(ctx, fn)
		if !transientConflict(err) || attempt >= txRetries {
			return err
		}
		// Somebody else changed what this transaction read. Nothing it wrote
		// survived the rollback, so running it again is running it for the first
		// time; that is the whole reason a retry is sound here and would not be
		// outside a transaction.
		select {
		case <-ctx.Done():
			return err
		case <-time.After(txRetryDelay << attempt):
		}
	}
}

func (d *DB) tx(ctx context.Context, fn func(context.Context, *Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqldb: begin transaction: %w", err)
	}

	if err := fn(ctx, &Tx{db: d, tx: tx}); err != nil {
		// The rollback error is deliberately not returned: the caller's error
		// is why we are here, and it is the one worth reading.
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqldb: commit: %w", err)
	}
	return nil
}

// How hard a transaction tries again when it lost a race rather than failed.
//
// Three attempts and a doubling wait: a conflict is resolved by whoever else was
// writing finishing, which takes about as long as a statement, so the answer is
// to wait a moment rather than to wait long. Past three, something is contending
// that a retry will not fix and the caller should hear about it.
const (
	txRetries    = 3
	txRetryDelay = 5 * time.Millisecond
)

// The two ways the database says "you lost a race", as opposed to "you were
// wrong". Both roll the whole transaction back, so both are safe to run again,
// and neither means anything about the caller's data.
//
// They stopped being rare when a batch of agents began writing one conversation
// at once: thirteen of them saving a step each into the same transcript is a
// normal Tuesday now, and it used to surface as a tool call that had really run
// being recorded as failed.
const (
	mysqlRecordChanged = 1020 // ER_CHECKREAD: a row moved under an open read
	mysqlDeadlock      = 1213 // ER_LOCK_DEADLOCK: this one was chosen to give way
)

func transientConflict(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlRecordChanged || me.Number == mysqlDeadlock
}

// Tx is a transaction. It offers the same three methods, and the same
// guarantee: only a constant statement, only bound parameters.
type Tx struct {
	db *DB
	tx *sql.Tx
}

func (t *Tx) QueryContext(ctx context.Context, q Statement, args ...any) (*sql.Rows, error) {
	stmt, err := t.stmt(ctx, q)
	if err != nil {
		return nil, err
	}
	return stmt.QueryContext(ctx, args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, q Statement, args ...any) *sql.Row {
	stmt, err := t.stmt(ctx, q)
	if err != nil {
		// Same reasoning as on the pool: let the real error reach Scan.
		return t.tx.QueryRowContext(ctx, string(q), args...)
	}
	return stmt.QueryRowContext(ctx, args...)
}

func (t *Tx) ExecContext(ctx context.Context, q Statement, args ...any) (sql.Result, error) {
	stmt, err := t.stmt(ctx, q)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}

// stmt binds the process-wide prepared statement to this transaction, so a
// transaction reuses the preparation rather than paying for its own.
func (t *Tx) stmt(ctx context.Context, q Statement) (*sql.Stmt, error) {
	stmt, err := t.db.prepare(ctx, q)
	if err != nil {
		return nil, err
	}
	return t.tx.StmtContext(ctx, stmt), nil
}

// prepare returns the cached statement, preparing it on first use.
func (d *DB) prepare(ctx context.Context, q Statement) (*sql.Stmt, error) {
	d.mu.RLock()
	stmt, ok := d.stmts[q]
	d.mu.RUnlock()
	if ok {
		return stmt, nil
	}

	prepared, err := d.db.PrepareContext(ctx, string(q))
	if err != nil {
		return nil, fmt.Errorf("sqldb: prepare: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Another goroutine may have prepared the same statement while this one
	// was waiting. Keep theirs and close ours, so the cache holds exactly one
	// statement per query.
	if existing, ok := d.stmts[q]; ok {
		_ = prepared.Close()
		return existing, nil
	}
	d.stmts[q] = prepared
	return prepared, nil
}

// Rows turns a constant INSERT into a multi-row one.
//
//	const insert Statement = `INSERT INTO t (a, b) VALUES `
//	db.ExecContext(ctx, insert.Rows(n, 2), args...)
//
// It is the one thing that may be added to a statement at runtime, and it is
// safe for a reason worth stating rather than assuming: what it appends is
// PLACEHOLDERS. "(?, ?), (?, ?)" and nothing else. It takes two counts and no
// data, so there is no value for it to get wrong, and the values still travel
// as bound parameters exactly as they do for a single row.
//
// Anything else is still impossible: this returns a Statement, and only a
// constant or this can become one.
//
// Rows are inserted in CHUNKS by callers rather than all at once, because a
// prepared statement is cached by its text: a batch of a thousand and a batch of
// nine hundred are two statements, and unbounded batch sizes would be unbounded
// cached statements. A fixed chunk means two or three, forever.
func (s Statement) Rows(rows, cols int) Statement {
	if rows <= 0 || cols <= 0 {
		return s
	}
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	return s + Statement(strings.TrimSuffix(strings.Repeat(group+",", rows), ","))
}

// RowChunk is how many rows one multi-row insert carries.
//
// Big enough that a thousand is ten statements rather than a thousand, small
// enough that the prepared-statement cache holds a handful of shapes rather than
// one per batch size anybody ever asks for.
const RowChunk = 100
