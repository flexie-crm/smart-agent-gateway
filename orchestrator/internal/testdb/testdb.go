// Package testdb gives each test package its own scratch database.
//
// Go runs test packages in parallel, so two suites sharing one database
// would truncate each other's rows mid-test. Each caller passes a unique
// suffix and gets an isolated database derived from SAG_TEST_DSN, created
// and migrated on demand. The suite skips when SAG_TEST_DSN is unset.
//
// The database belongs to the package, not to the test: Open creates it once
// and the tests inside share it, truncating between themselves, so the
// migrations run once rather than once per test. Main is what takes it away
// again (see Main), and every package that calls Open must call Main.
package testdb

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/migrations"
	"flexie.io/sag/internal/store/sqlstore"
)

// notOurs is the one table the wipe leaves alone: the migration bookkeeping.
// Emptying it would tell the next test the database has never been migrated.
const notOurs = "sag_db_version"

// Open returns a migrated store on a database named after suffix, plus a
// reset function that empties it.
func Open(t *testing.T, dsn, suffix string) (*sqlstore.SQLStore, func(t *testing.T)) {
	t.Helper()
	ctx := context.Background()

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse SAG_TEST_DSN: %v", err)
	}
	dbName := cfg.DBName + "_" + suffix

	// Connect without a database to create the scratch one.
	serverCfg := *cfg
	serverCfg.DBName = ""
	admin, err := sqlstore.Open(ctx, serverCfg.FormatDSN())
	if err != nil {
		t.Fatalf("connect to database server: %v", err)
	}
	if _, err := admin.DB().ExecContext(ctx,
		"CREATE DATABASE IF NOT EXISTS `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}
	if err := admin.Close(); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}

	cfg.DBName = dbName
	st, err := sqlstore.Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open test database %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The schema is the package's, not the test's: it is built once and every
	// test in the package shares it. Running the migrations per test meant every
	// one of them paid for goose to connect, read the version table and compare
	// every migration, which for a package of a hundred tests is a hundred times
	// the work for the same answer.
	if err := prepare(ctx, dbName, st); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	// Emptying the database means emptying it, and the schema's foreign keys
	// rightly refuse to truncate a table something still points at. The checks
	// are lifted for the wipe and restored immediately: this is the one place
	// where "delete everything, order be damned" is the correct instruction, and
	// it is a test fixture rather than a code path the product has.
	// The tables are ASKED FOR, not listed here. A hand-written list is a list
	// somebody forgets to add a table to, and the rows that survive then surface
	// in the next test as a ghost: a user who belongs to a workspace nobody
	// created, a setting whose owner does not exist. The database knows what it
	// holds, so it is the one that says.
	tables := tablesOf(dbName)

	reset := func(t *testing.T) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
			t.Fatalf("lift foreign key checks: %v", err)
		}
		defer func() {
			if _, err := st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
				t.Fatalf("restore foreign key checks: %v", err)
			}
		}()
		// DELETE rather than TRUNCATE. They empty a table alike, but TRUNCATE
		// drops and rebuilds the table's storage, which costs the same whether
		// there was one row in it or none. Between tests these tables are empty
		// or nearly so, and there are dozens of them per test, so paying to
		// rebuild storage for rows that are not there is most of what a suite
		// spends its time on.
		for _, table := range tables {
			if _, err := st.DB().ExecContext(ctx, "DELETE FROM `"+table+"`"); err != nil {
				t.Fatalf("empty %s: %v", table, err)
			}
		}
	}
	reset(t)
	return st, reset
}

// prepared remembers the databases whose schema is already built, and the tables
// each one holds, so a package pays for both once instead of once per test.
var prepared sync.Map // dbName -> *schema

type schema struct {
	once   sync.Once
	tables []string
	err    error
}

func prepare(ctx context.Context, dbName string, st *sqlstore.SQLStore) error {
	entry, _ := prepared.LoadOrStore(dbName, &schema{})
	s := entry.(*schema)
	s.once.Do(func() {
		if s.err = migrations.Run(ctx, st.DB(), "up"); s.err != nil {
			return
		}
		s.tables, s.err = tableNames(ctx, st, dbName)
	})
	return s.err
}

// tablesOf is what prepare found, for a database it has already built.
func tablesOf(dbName string) []string {
	entry, ok := prepared.Load(dbName)
	if !ok {
		return nil
	}
	return entry.(*schema).tables
}

// forgetSchema drops what prepare remembered, so a database taken away and made
// again is migrated again rather than assumed.
func forgetSchema(dbName string) { prepared.Delete(dbName) }

// Main runs a package's tests and TAKES ITS SCRATCH DATABASE AWAY afterwards.
//
// Call it from TestMain, with the same suffix the package passes to Open:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }
//
// The drop cannot go in a t.Cleanup. The database is shared by every test in
// the package, so a cleanup would take it away after the first one and leave
// the second to rebuild it, migrations and all. TestMain is the only hook Go
// runs after the last test, which makes it the only place the drop fits.
//
// Without this, the scratch databases outlive the run that made them and pile
// up on the server, one per package, until a developer looking at their
// database list sees our leftovers instead of their work.
func Main(m *testing.M, suffix string) int {
	code := m.Run()
	if err := drop(suffix); err != nil {
		// Loudly, and fatally. A run that cannot clean up after itself has left
		// the next one a database in a state nobody chose, and a suite that
		// passes against a database like that has proved nothing.
		fmt.Fprintf(os.Stderr, "testdb: the scratch database was left behind: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// drop removes the database Open would have created for this suffix. It is a
// no-op when SAG_TEST_DSN is unset, which is the case where the suites skipped
// and there is nothing to take away.
func drop(suffix string) error {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		return nil
	}

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse SAG_TEST_DSN: %w", err)
	}
	dbName := cfg.DBName + "_" + suffix
	// The schema goes with the database: a run that makes it again migrates it
	// again rather than assuming what a previous one left behind.
	forgetSchema(dbName)

	// Connect without a database: the one being dropped cannot be the one in use.
	cfg.DBName = ""
	ctx := context.Background()
	admin, err := sqlstore.Open(ctx, cfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("connect to database server: %w", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.DB().ExecContext(ctx, "DROP DATABASE IF EXISTS `"+dbName+"`"); err != nil {
		return fmt.Errorf("drop test database %s: %w", dbName, err)
	}
	return nil
}

// tableNames asks the database what it holds, minus the migration bookkeeping.
func tableNames(ctx context.Context, st *sqlstore.SQLStore, dbName string) ([]string, error) {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = ? AND table_type = 'BASE TABLE' AND table_name <> ?
		 ORDER BY table_name`, dbName, notOurs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
