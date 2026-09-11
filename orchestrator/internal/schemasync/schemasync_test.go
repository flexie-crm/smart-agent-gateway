package schemasync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/migrations"
	"flexie.io/sag/internal/schemasync"
)

// The sync is what tells us a database matches the schema we declared. One that
// lies is worse than none at all, so it is tested against a real database built
// by the real migrations, and against every change it is supposed to notice.
//
// The two mechanisms are separate and both are real:
//
//   - the MIGRATIONS upgrade a database, progressively and reversibly, and they
//     do the things a differ never can: backfill a column, delete orphans before
//     a constraint can go on, transform rows.
//   - the SYNC converges a database onto the declared schema. It is DDL only.
//
// They meet in one assertion, and it is the first test below: what the
// migrations produce IS what we declared.

func dsn(t *testing.T) string {
	t.Helper()
	value := os.Getenv("SAG_TEST_DSN")
	if value == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the schema suite")
	}
	return value
}

// scratch makes an empty throwaway database and TAKES IT AWAY AFTERWARDS.
//
// The cleanup opens its own connection, because the one this function used is
// closed the moment it returns and t.Cleanup runs long after that. Dropping a
// database on a closed pool fails silently, and the databases pile up on the
// server, one per test, run after run. That is exactly what happened.
func scratch(t *testing.T, name string) string {
	t.Helper()
	base := dsn(t)
	target := swap(base, dbName(base)+"_sync_"+name)
	db := dbName(target)

	exec := func(statement string) error {
		admin, err := sql.Open("mysql", swap(base, ""))
		if err != nil {
			return err
		}
		defer func() { _ = admin.Close() }()
		_, err = admin.ExecContext(context.Background(), statement)
		return err
	}

	if err := exec("DROP DATABASE IF EXISTS `" + db + "`"); err != nil {
		t.Fatalf("drop %s: %v", db, err)
	}
	if err := exec("CREATE DATABASE `" + db + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create %s: %v", db, err)
	}
	t.Cleanup(func() {
		if err := exec("DROP DATABASE IF EXISTS `" + db + "`"); err != nil {
			// Loudly. A test that leaves a database behind is a test that will
			// pass for the wrong reason next time, on somebody else's machine.
			t.Errorf("the scratch database %s was left behind: %v", db, err)
		}
	})
	return target
}

// migrated is a scratch database brought up to date by the REAL migrations. It
// is what a database kept current by `sag migrate` looks like.
func migrated(t *testing.T, name string) string {
	t.Helper()
	target := scratch(t, name)

	conn, err := sql.Open("mysql", target)
	if err != nil {
		t.Fatalf("open %s: %v", target, err)
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	if err := migrations.Run(ctx, conn, "up"); err != nil {
		t.Fatalf("the migrations do not apply to an empty database: %v", err)
	}
	// The migration runner's own bookkeeping is not part of the schema we design.
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS sag_db_version"); err != nil {
		t.Fatalf("drop bookkeeping: %v", err)
	}
	return target
}

// readOnly is a database migrated ONCE for the whole package, for the tests that
// only compute a plan against it.
//
// Building one is the expensive thing this suite does: a database created and
// then brought up through every migration, which is nothing but the kind of
// statement a server flushes to disk. Most of these tests never write to it,
// they read its shape and compare, so they can all read the same one. Only a
// test that APPLIES something needs a database of its own, and those still get
// one.
var readOnly struct {
	once   sync.Once
	target string
	err    error
}

func readOnlyMigrated(t *testing.T) string {
	t.Helper()
	base := dsn(t) // skips the suite when there is no database to talk to
	readOnly.once.Do(func() {
		readOnly.target, readOnly.err = buildMigrated(base, "readonly")
	})
	if readOnly.err != nil {
		t.Fatalf("build the shared migrated database: %v", readOnly.err)
	}
	return readOnly.target
}

// TestMain takes the shared database away afterwards.
//
// The drop cannot go in a t.Cleanup: the database belongs to every test in the
// package, so a cleanup would take it away after the first one and leave the
// rest to rebuild it, migrations and all, which is the cost this exists to
// avoid. TestMain is the only hook Go runs after the last test.
func TestMain(m *testing.M) {
	code := m.Run()
	if readOnly.target != "" {
		if err := dropDatabase(readOnly.target); err != nil {
			// Loudly. A run that leaves a database behind is a run that will pass
			// for the wrong reason next time, on somebody else's machine.
			fmt.Fprintf(os.Stderr, "the shared schema database was left behind: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(code)
}

// buildMigrated makes a database and brings it up through every migration,
// without tying its life to one test.
func buildMigrated(base, name string) (string, error) {
	target := swap(base, dbName(base)+"_sync_"+name)
	db := dbName(target)

	if err := onServer(base, "DROP DATABASE IF EXISTS `"+db+"`"); err != nil {
		return "", err
	}
	if err := onServer(base, "CREATE DATABASE `"+db+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return "", err
	}

	conn, err := sql.Open("mysql", target)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	ctx := context.Background()
	if err := migrations.Run(ctx, conn, "up"); err != nil {
		return "", fmt.Errorf("the migrations do not apply to an empty database: %w", err)
	}
	// The migration runner's own bookkeeping is not part of the schema we design.
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS sag_db_version"); err != nil {
		return "", err
	}
	return target, nil
}

// onServer runs one statement against the server rather than a database, on a
// connection of its own, so it works before a database exists and after the
// caller's own connection is gone.
func onServer(base, statement string) error {
	admin, err := sql.Open("mysql", swap(base, ""))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()
	_, err = admin.ExecContext(context.Background(), statement)
	return err
}

func dropDatabase(target string) error {
	return onServer(target, "DROP DATABASE IF EXISTS `"+dbName(target)+"`")
}

func dbName(target string) string {
	slash := strings.LastIndex(target, "/")
	rest := target[slash+1:]
	if question := strings.Index(rest, "?"); question != -1 {
		return rest[:question]
	}
	return rest
}

func swap(target, name string) string {
	slash := strings.LastIndex(target, "/")
	rest := target[slash+1:]
	params := ""
	if question := strings.Index(rest, "?"); question != -1 {
		params = rest[question:]
	}
	return target[:slash+1] + name + params
}

// repoSchema is the real declaration, and it must match the real migrations.
// This is the assertion CI depends on, so it is also a test.
func TestTheDeclaredSchemaMatchesTheMigrations(t *testing.T) {
	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), "../../schema")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !plan.InSync() {
		t.Fatalf("the migrations do not produce the schema we declare:\n  %s",
			strings.Join(plan.Statements, "\n  "))
	}
}

// An empty database is not a special case: the difference between nothing and
// the declared schema is the whole schema, which is how a database is created.
func TestAnEmptyDatabaseIsBuiltFromNothing(t *testing.T) {
	empty := scratch(t, "empty")

	plan, err := schemasync.Compute(context.Background(), empty, "../../schema")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if plan.InSync() {
		t.Fatal("an empty database was reported as already matching the schema")
	}
	if err := schemasync.Apply(context.Background(), empty, plan, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// And now it does match, which is the only thing "in sync" can mean.
	plan, err = schemasync.Compute(context.Background(), empty, "../../schema")
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if !plan.InSync() {
		t.Fatalf("the database was not brought to the declared schema:\n  %s",
			strings.Join(plan.Statements, "\n  "))
	}
}

// Dropping a column destroys data, and nothing does that because somebody
// deleted a line in a file. It is named, and it is refused until somebody says
// they mean it.
func TestADestructiveChangeIsRefusedUnlessItIsMeant(t *testing.T) {
	target := migrated(t, "destructive")
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agents", "  `instructions` mediumtext DEFAULT NULL,\n", "")
	})

	plan, err := schemasync.Compute(context.Background(), target, dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(plan.Destructive) == 0 {
		t.Fatal("dropping a column was not recognised as destroying data")
	}

	err = schemasync.Apply(context.Background(), target, plan, false)
	if err == nil {
		t.Fatal("a column was dropped without anybody saying so")
	}
	if !strings.Contains(err.Error(), "destroy") {
		t.Fatalf("the refusal does not say why: %v", err)
	}

	// Said out loud, it goes through.
	if err := schemasync.Apply(context.Background(), target, plan, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

// declare copies the real schema into a directory the test can edit, so a test
// can change what is declared without touching the repository.
func declare(t *testing.T, edit func(dir string)) string {
	t.Helper()
	dir := t.TempDir()

	files, err := filepath.Glob("../../schema/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("the declared schema is missing: %v", err)
	}
	for _, file := range files {
		content, err := os.ReadFile(file) //nolint:gosec // our own schema directory
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(file)), content, 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	edit(dir)
	return dir
}

func rewrite(t *testing.T, dir, table, from, to string) {
	t.Helper()
	path := filepath.Join(dir, table+".sql")
	content, err := os.ReadFile(path) //nolint:gosec // a temporary directory this test made
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	updated := strings.Replace(string(content), from, to, 1)
	if updated == string(content) {
		t.Fatalf("%s does not contain %q, so this test is asserting nothing", table, from)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write %s: %v", table, err)
	}
}

// A column declared and never migrated is the whole failure this exists to
// catch: somebody edits the schema, forgets the migration, and the database
// quietly lacks the column their code expects.
func TestAColumnDeclaredButNotMigratedIsCaught(t *testing.T) {
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agents",
			"  `status` enum('active','disabled') NOT NULL DEFAULT 'active',",
			"  `description` varchar(255) DEFAULT NULL,\n  `status` enum('active','disabled') NOT NULL DEFAULT 'active',")
	})

	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if plan.InSync() {
		t.Fatal("a column that no migration creates was reported as in sync")
	}

	ddl := strings.Join(plan.Statements, "\n")
	if !strings.Contains(ddl, "`agents`") || !strings.Contains(ddl, "`description`") {
		t.Fatalf("the difference does not name the column that is missing:\n%s", ddl)
	}
	if !strings.Contains(ddl, "ADD COLUMN") {
		t.Fatalf("the difference is not the DDL that would fix it:\n%s", ddl)
	}
}

// An index is a schema change too, and one that is easy to add to a table and
// forget to migrate.
func TestAnIndexDeclaredButNotMigratedIsCaught(t *testing.T) {
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agents",
			"  UNIQUE KEY `uniq_ws_key` (`workspace_id`,`agent_key`),",
			"  UNIQUE KEY `uniq_ws_key` (`workspace_id`,`agent_key`),\n  KEY `idx_agent_status` (`status`),")
	})

	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if plan.InSync() {
		t.Fatal("an index that no migration creates was reported as in sync")
	}
	if ddl := strings.Join(plan.Statements, "\n"); !strings.Contains(ddl, "idx_agent_status") {
		t.Fatalf("the difference does not name the index that is missing:\n%s", ddl)
	}
}

// A foreign key is the rule that keeps the data honest (KB/14). Dropping one
// from the declaration and not from the migrations must not pass quietly.
func TestAForeignKeyDifferenceIsCaught(t *testing.T) {
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agents",
			"  CONSTRAINT `fk_agent_model` FOREIGN KEY (`model_id`) REFERENCES `ai_models` (`id`) ON DELETE SET NULL,\n",
			"")
	})

	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if plan.InSync() {
		t.Fatal("a missing foreign key was reported as in sync")
	}
	if ddl := strings.Join(plan.Statements, "\n"); !strings.Contains(ddl, "fk_agent_model") {
		t.Fatalf("the difference does not name the constraint:\n%s", ddl)
	}
}

// The comparison must not report a difference that is merely how the server
// writes a table down. MariaDB widens `int` to `int(10)`, canonicalises
// defaults, and picks collations nobody typed: a differ that reported those
// would cry wolf on every run and be switched off within a week.
func TestTheServersOwnFormattingIsNotADifference(t *testing.T) {
	dir := declare(t, func(dir string) {
		// Written the way a person would write it, not the way the server
		// stores it: no display width, no explicit NULL, keywords in a
		// different case.
		rewrite(t, dir, "agents",
			"  `approval_ttl_seconds` int(10) unsigned DEFAULT NULL,",
			"  `approval_ttl_seconds` INT UNSIGNED NULL,")
	})

	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !plan.InSync() {
		t.Fatalf("the same column written differently was reported as a change:\n  %s",
			strings.Join(plan.Statements, "\n  "))
	}
}

// Dump regenerates the declaration from the migrations. It is what you run when
// the migration is right and the declaration is what is stale.
func TestDumpWritesTheSchemaTheMigrationsProduce(t *testing.T) {
	dir := t.TempDir()
	target := readOnlyMigrated(t)

	if err := schemasync.Dump(context.Background(), target, dir); err != nil {
		t.Fatalf("dump: %v", err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the dump wrote no tables")
	}

	// The migration runner's own bookkeeping is not part of the schema we design.
	for _, file := range files {
		if strings.Contains(filepath.Base(file), "sag_db_version") {
			t.Fatal("the dump included the migration runner's bookkeeping table")
		}
	}

	agents, err := os.ReadFile(filepath.Join(dir, "agents.sql")) //nolint:gosec // a temporary directory this test made
	if err != nil {
		t.Fatalf("the dump did not write agents: %v", err)
	}
	for _, want := range []string{
		"CREATE TABLE `agents`",
		"`approval_ttl_seconds`",
		"CONSTRAINT `fk_agent_workspace`",
	} {
		if !strings.Contains(string(agents), want) {
			t.Fatalf("the dumped table is missing %q:\n%s", want, agents)
		}
	}
	// AUTO_INCREMENT is a fact about the rows in a scratch database, not about
	// the schema, and it would change the file on every run.
	if strings.Contains(string(agents), "AUTO_INCREMENT=") {
		t.Fatalf("the dump wrote a row counter into the schema:\n%s", agents)
	}

	// And what it wrote is, of course, in sync with what it was dumped from.
	plan, err := schemasync.Compute(context.Background(), target, dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !plan.InSync() {
		t.Fatalf("the dump does not agree with what it was dumped from:\n  %s",
			strings.Join(plan.Statements, "\n  "))
	}
}

// A schema file is SQL, and SQL can be wrong. When the server refuses it, the
// message has to be about the FILE, because that is what the person just edited:
// the server's own wording is about a query, and it leaves them staring at the
// database wondering what is wrong with it.
func TestAnInvalidSchemaFileExplainsItself(t *testing.T) {
	// The classic: rename a column and forget the unique key that names it.
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agent_runs",
			"  `uid` varchar(32) NOT NULL,",
			"  `uids` varchar(32) NOT NULL,")
	})

	_, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err == nil {
		t.Fatal("a table the database cannot create was accepted")
	}

	message := err.Error()
	for _, want := range []string{
		"agent_runs.sql",    // which file
		"Key column 'uid'",  // what the database said
		"renaming a column", // and why it is likely to have happened
		"Fix the file",      // and what to do about it
	} {
		if !strings.Contains(strings.ToLower(message), strings.ToLower(want)) {
			t.Fatalf("the error does not say %q:\n%s", want, message)
		}
	}
}

// A rename is NOT inferred, and it must not be. A differ compares two states: a
// column gone and a column added look exactly like a rename, and nothing tells
// it which. Guessing would silently destroy the data in a column, so it does the
// literal thing and flags it as destructive. A rename belongs in a migration,
// where somebody says CHANGE and the rows survive.
func TestARenameIsNotInferred(t *testing.T) {
	dir := declare(t, func(dir string) {
		rewrite(t, dir, "agent_runs",
			"  `uid` varchar(32) NOT NULL,",
			"  `uids` varchar(32) NOT NULL,")
		rewrite(t, dir, "agent_runs",
			"UNIQUE KEY `uniq_uid` (`uid`)",
			"UNIQUE KEY `uniq_uid` (`uids`)")
	})

	plan, err := schemasync.Compute(context.Background(), readOnlyMigrated(t), dir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	ddl := strings.Join(plan.Statements, "\n")
	if strings.Contains(strings.ToUpper(ddl), "CHANGE ") || strings.Contains(strings.ToUpper(ddl), "RENAME COLUMN") {
		t.Fatalf("a rename was inferred, which means a differ guessed at what a person meant:\n%s", ddl)
	}
	if !strings.Contains(ddl, "DROP COLUMN `uid`") {
		t.Fatalf("the difference is not the literal one:\n%s", ddl)
	}
	// And because it destroys data, it cannot happen by accident.
	if len(plan.Destructive) == 0 {
		t.Fatal("dropping the old column was not flagged as destroying data")
	}
	if err := schemasync.Apply(context.Background(), migrated(t, "rename2"), plan, false); err == nil {
		t.Fatal("a column was dropped without anybody saying so")
	}
}
