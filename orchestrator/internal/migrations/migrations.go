// Package migrations embeds the SQL schema migrations and runs them with
// goose. Migrations ship inside the sag binary, so `sag migrate up` works
// identically in dev, cloud, and on-prem deployments.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed sql/*.sql
var files embed.FS

// Run executes a goose command ("up", "down", "status", "version") against
// the given database.
func Run(ctx context.Context, db *sql.DB, command string) error {
	goose.SetBaseFS(files)
	// Product-named version table, internal names never advertise the
	// underlying library.
	goose.SetTableName("sag_db_version")
	if err := goose.SetDialect("mysql"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	switch command {
	case "up":
		return goose.UpContext(ctx, db, "sql")
	case "down":
		return goose.DownContext(ctx, db, "sql")
	case "status":
		return goose.StatusContext(ctx, db, "sql")
	case "version":
		return goose.VersionContext(ctx, db, "sql")
	default:
		return fmt.Errorf("unknown migrate command %q (use up|down|status|version)", command)
	}
}

// DownTo rolls the database back to a given version.
//
// It exists for the tests that prove a DATA migration does what it says. Those
// have to put the database into the state the migration was written for, which
// means going back to the version just before it and coming forward again
// through the real file. Counting steps ("down once") does that only until the
// next migration is added, and then it silently tests the wrong one.
func DownTo(ctx context.Context, db *sql.DB, version int64) error {
	goose.SetBaseFS(files)
	goose.SetTableName("sag_db_version")
	if err := goose.SetDialect("mysql"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	return goose.DownToContext(ctx, db, "sql", version)
}
