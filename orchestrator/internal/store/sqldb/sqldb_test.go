package sqldb_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/internal/store/sqldb"
	"flexie.io/sag/internal/testdb"
)

// The guarantee this package exists for is enforced by the compiler, so the
// most important test is not in this file: it is that the project builds.
//
//	query := "SELECT * FROM users WHERE name = '" + name + "'"
//	db.QueryContext(ctx, query)
//
// That does not compile. A string variable is not a Statement, and only a
// compile-time constant becomes one. There is no test to write for a program
// that cannot exist.
//
// What is left to check is that the safe path is actually safe: that a value
// which looks like SQL is treated as a value, and that the statement cache
// behaves under concurrency.

// dbSuffix names this package's scratch database. Open makes it, TestMain
// takes it away, and they read it from here so they cannot drift apart.
const dbSuffix = "sqldb"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }

func open(t *testing.T) *sqldb.DB {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set")
	}
	st, _ := testdb.Open(t, dsn, dbSuffix)
	return sqldb.New(st.DB())
}

// The injection that would work against string concatenation must do nothing
// here: the value is a value, however much it looks like code.
func TestValuesAreValuesEvenWhenTheyLookLikeSQL(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO workspaces (slug, name, status, created_at, updated_at)
		 VALUES (?, ?, 'active', NOW(3), NOW(3))`,
		"victim", "Victim"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The classic payload. Against an assembled query this ends the string and
	// drops the table. Bound, it is simply a slug that nothing matches.
	payload := "x'; DROP TABLE workspaces; --"

	var found string
	err := db.QueryRowContext(ctx,
		`SELECT slug FROM workspaces WHERE slug = ?`, payload).Scan(&found)
	if err == nil {
		t.Fatalf("a workspace named %q should not exist", payload)
	}

	// The table is still there, and so is the row.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE slug = ?`, "victim").Scan(&count); err != nil {
		t.Fatalf("the table did not survive: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the row to survive, found %d", count)
	}
}

// A value containing a quote is a value, not a syntax error.
func TestQuotesInValuesAreHarmless(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	name := `O'Brien "the fixer" \ 100%`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workspaces (slug, name, status, created_at, updated_at)
		 VALUES (?, ?, 'active', NOW(3), NOW(3))`,
		"quotes", name); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM workspaces WHERE slug = ?`, "quotes").Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if stored != name {
		t.Fatalf("the value was mangled: %q", stored)
	}
}

// The statement cache must hand every caller the same prepared statement, and
// must not race while doing it.
func TestStatementCacheIsSafeUnderConcurrency(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO workspaces (slug, name, status, created_at, updated_at)
		 VALUES (?, ?, 'active', NOW(3), NOW(3))`,
		"concurrent", "Concurrent"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var slug string
			if err := db.QueryRowContext(ctx,
				`SELECT slug FROM workspaces WHERE slug = ?`, "concurrent").Scan(&slug); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent query failed: %v", err)
	}
}

// A statement the database rejects must surface as an error the caller sees,
// not as a silent fallback to an unprepared query.
func TestABrokenStatementReportsAnError(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `SELECT * FROM a_table_that_does_not_exist`); err == nil {
		t.Fatal("a broken statement must report an error")
	}

	// The same through QueryRow, where the error can only arrive on Scan.
	var scanned string
	err := db.QueryRowContext(ctx, `SELECT nonexistent_column FROM workspaces`).Scan(&scanned)
	if err == nil {
		t.Fatal("a broken statement must report an error on scan")
	}
	if !strings.Contains(err.Error(), "prepare") && !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("the error does not say what went wrong: %v", err)
	}
}

// A transaction that fails rolls back, and one that returns cleanly commits.
// The caller never writes either, which is why neither can be forgotten.
func TestTransactionsCommitAndRollBack(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// Rolled back.
	wantErr := context.Canceled
	err := db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workspaces (slug, name, status, created_at, updated_at)
			 VALUES (?, ?, 'active', NOW(3), NOW(3))`, "rolled-back", "Rolled back"); err != nil {
			return err
		}
		return wantErr
	})
	if err != wantErr { //nolint:errorlint // the sentinel is returned verbatim, on purpose
		t.Fatalf("the caller's error must come back unchanged: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE slug = ?`, "rolled-back").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("a failed transaction was not rolled back")
	}

	// Committed.
	if err := db.Tx(ctx, func(ctx context.Context, tx *sqldb.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO workspaces (slug, name, status, created_at, updated_at)
			 VALUES (?, ?, 'active', NOW(3), NOW(3))`, "committed", "Committed")
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE slug = ?`, "committed").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatal("a successful transaction was not committed")
	}
}
