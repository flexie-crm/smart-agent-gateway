package testdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/store/sqlstore"
)

// The drop is what keeps a developer's database list from filling up with our
// leftovers, so it is worth proving that it removes the database rather than
// merely returning without an error.

func TestDropRemovesTheScratchDatabase(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping scratch database teardown suite")
	}
	const suffix = "droptest"

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse SAG_TEST_DSN: %v", err)
	}
	dbName := cfg.DBName + "_" + suffix

	// The database is made by hand, not by Open: this is a test of the teardown,
	// and paying for the migrations would tell us nothing more about it.
	exec(t, dsn, "CREATE DATABASE IF NOT EXISTS `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	if !databaseExists(t, dsn, dbName) {
		t.Fatalf("precondition failed: %s was not created", dbName)
	}

	if err := drop(suffix); err != nil {
		t.Fatalf("drop: %v", err)
	}

	if databaseExists(t, dsn, dbName) {
		t.Errorf("the scratch database %s survived the drop", dbName)
	}
}

// A skipped suite creates no database, and the teardown of a database that was
// never made must not fail the package it runs in.
func TestDropWithoutADSNDoesNothing(t *testing.T) {
	t.Setenv("SAG_TEST_DSN", "")

	if err := drop("never-created"); err != nil {
		t.Errorf("drop without a DSN: got %v, want no error", err)
	}
}

// A database that was never there is already in the state the drop wants it in.
func TestDropIsIdempotent(t *testing.T) {
	if os.Getenv("SAG_TEST_DSN") == "" {
		t.Skip("SAG_TEST_DSN not set; skipping scratch database teardown suite")
	}

	if err := drop("never-created"); err != nil {
		t.Errorf("drop of an absent database: got %v, want no error", err)
	}
}

// exec runs one statement against the server, with no database selected.
func exec(t *testing.T, dsn, statement string) {
	t.Helper()
	admin := server(t, dsn)
	defer func() { _ = admin.Close() }()

	if _, err := admin.DB().ExecContext(context.Background(), statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// databaseExists asks the server, which is the only opinion that counts here.
func databaseExists(t *testing.T, dsn, name string) bool {
	t.Helper()
	admin := server(t, dsn)
	defer func() { _ = admin.Close() }()

	var found string
	err := admin.DB().QueryRowContext(context.Background(),
		"SELECT schema_name FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false
	case err != nil:
		t.Fatalf("look for database %s: %v", name, err)
	}
	return found == name
}

// server connects to the database server itself, with no database selected.
func server(t *testing.T, dsn string) *sqlstore.SQLStore {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse SAG_TEST_DSN: %v", err)
	}
	cfg.DBName = ""

	st, err := sqlstore.Open(context.Background(), cfg.FormatDSN())
	if err != nil {
		t.Fatalf("connect to database server: %v", err)
	}
	return st
}
