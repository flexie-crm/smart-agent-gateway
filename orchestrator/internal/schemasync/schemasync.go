// Package schemasync brings a database to the schema we declared.
//
// The schema lives in schema/: one CREATE TABLE per file, and the file IS the
// definition. To learn what a table looks like, you read the table.
//
// The database is brought to it by DIFFERENCE: read what the database currently
// is, read what we declared, and work out the DDL between them. That works from
// any starting point, an empty database included, so the same command creates a
// new deployment and updates an old one, with no bookkeeping about which change
// went where.
//
// # How the comparison is made honest
//
// Both sides are read back FROM the server. The declared files are executed into
// a throwaway database and inspected there; the live database is inspected as it
// stands. That matters more than it sounds: MariaDB does not store a table the
// way you typed it. It widens `int` to `int(10)`, rewrites `NULL` as `DEFAULT
// NULL`, canonicalises an index, and picks a collation you never mentioned.
// Comparing the text you wrote against the table the server built would report a
// difference on every line, cry wolf on every run, and be switched off within a
// week. Comparing two tables the SERVER built compares like with like, and a
// difference means a difference.
//
// # What it will not do quietly
//
// Dropping a table or a column destroys data. Those statements are found by
// looking at the CHANGE rather than at the text of the SQL, named, and refused
// unless the caller says it means it. A tool that silently drops a column
// because somebody deleted a line in a file is a tool that will one day delete
// production.
package schemasync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/mysql"
	atlas "ariga.io/atlas/sql/schema"
	driver "github.com/go-sql-driver/mysql"
)

// Charset and collation are declared here rather than inherited from whatever a
// server happens to default to. A table that inherits the wrong collation
// compares text differently from every table around it, and nothing tells you.
const (
	charset   = "utf8mb4"
	collation = "utf8mb4_unicode_ci"
)

// The migration runner keeps its own bookkeeping in the database. It is not a
// table we designed and it is not in schema/, so a comparison that did not know
// about it would cheerfully propose dropping it, and take the record of which
// migrations have run with it.
var notOurs = []string{"sag_db_version"}

// Plan is the difference between a database and the schema we declared.
type Plan struct {
	// Statements bring the database to the declared schema, in the order they
	// must run.
	Statements []string
	// Destructive names the ones that would lose data. They are part of
	// Statements; they are listed apart because they are the ones somebody has
	// to say yes to.
	Destructive []string
}

// InSync reports that the database already is what we declared.
func (p Plan) InSync() bool { return len(p.Statements) == 0 }

// Compute reads the database, reads the declaration, and works out the DDL
// between them.
//
// The declared schema is built on the same server, in a throwaway database that
// is dropped afterwards, because the only way to compare like with like is to
// let the server build both.
func Compute(ctx context.Context, dsn, schemaDir string) (Plan, error) {
	admin, target, err := connect(ctx, dsn)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = admin.Close() }()

	// The database may not exist yet: on an empty one the difference is the
	// whole schema, which is how a deployment is created.
	if err := ensure(ctx, admin, target); err != nil {
		return Plan{}, err
	}

	declared := target + "_declared"
	defer drop(ctx, admin, declared)

	if err := buildDeclared(ctx, admin, declared, schemaDir); err != nil {
		return Plan{}, err
	}
	return diff(ctx, admin, target, declared)
}

// Apply runs the plan against the database.
//
// Destructive statements are refused unless the caller has said, in as many
// words, that it means to lose the data.
func Apply(ctx context.Context, dsn string, plan Plan, allowDestructive bool) error {
	if len(plan.Destructive) > 0 && !allowDestructive {
		return fmt.Errorf("this would destroy data, and nothing here does that unless told to:\n  %s\n\n"+
			"Add --allow-destructive if that is what you mean",
			strings.Join(plan.Destructive, "\n  "))
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = db.Close() }()

	for _, statement := range plan.Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%s\n  %w", statement, err)
		}
	}
	return nil
}

// Dump writes a database's schema out as one CREATE TABLE per file. It is how
// the declaration is rebuilt FROM a database, rather than the other way round.
func Dump(ctx context.Context, dsn, schemaDir string) error {
	admin, target, err := connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()

	tables, err := tableNames(ctx, admin, target)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return fmt.Errorf("%s has no tables to write down", target)
	}

	existing, err := filepath.Glob(filepath.Join(schemaDir, "*.sql"))
	if err != nil {
		return err
	}
	for _, file := range existing {
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("remove stale %s: %w", filepath.Base(file), err)
		}
	}

	for _, table := range tables {
		if slices.Contains(notOurs, table) {
			continue
		}
		ddl, err := showCreate(ctx, admin, target, table)
		if err != nil {
			return err
		}
		path := filepath.Join(schemaDir, table+".sql")
		if err := os.WriteFile(path, []byte(ddl+";\n"), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", table, err)
		}
	}
	return nil
}

// --- the declared side ------------------------------------------------------------

// buildDeclared executes the schema files into an empty database, so the server
// can tell us what they actually mean.
//
// Foreign key checks are lifted while it runs: a directory of tables has no
// order, and a table may reference one whose file comes later in the alphabet.
func buildDeclared(ctx context.Context, admin *sql.DB, name, schemaDir string) error {
	files, err := filepath.Glob(filepath.Join(schemaDir, "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no schema is declared in %s", schemaDir)
	}
	sort.Strings(files)

	if err := create(ctx, admin, name); err != nil {
		return err
	}
	if _, err := admin.ExecContext(ctx, "USE `"+name+"`"); err != nil {
		return fmt.Errorf("use %s: %w", name, err)
	}
	if _, err := admin.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return fmt.Errorf("lift foreign key checks: %w", err)
	}
	for _, file := range files {
		statement, err := os.ReadFile(file) //nolint:gosec // our own schema directory
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(file), err)
		}
		if _, err := admin.ExecContext(ctx, string(statement)); err != nil {
			return invalidDeclaration(file, err)
		}
	}
	if _, err := admin.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		return fmt.Errorf("restore foreign key checks: %w", err)
	}
	return nil
}

// invalidDeclaration explains a schema file the server refused.
//
// The declaration is SQL, and SQL can be wrong. When it is, the server says so
// in a sentence written for someone debugging a query, not for someone who has
// just edited a table definition, so the two most common ways to break a file
// are named outright: the server's answer is kept, and the likely cause is
// added to it.
func invalidDeclaration(file string, err error) error {
	name := filepath.Base(file)

	var me *driver.MySQLError
	if !errors.As(err, &me) {
		return fmt.Errorf("schema/%s is not valid SQL, and the database refused it:\n  %w", name, err)
	}

	explanation := ""
	switch me.Number {
	case errKeyColumnMissing:
		explanation = "An index, a unique key or a primary key still names a column that is not in the table.\n" +
			"  Renaming a column means renaming it everywhere it is mentioned, not only where it is defined."
	case errCannotAddForeignKey, errForeignKeyIncorrect:
		explanation = "A foreign key does not line up with what it points at.\n" +
			"  The two columns must have the same type, the same signedness and the same collation,\n" +
			"  and the column it references must be indexed."
	case errUnknownColumn:
		explanation = "Something in the table names a column that does not exist in it."
	case errSyntax:
		explanation = "The statement does not parse. A missing comma or a stray one is the usual reason."
	case errDuplicateKeyName:
		explanation = "Two keys in this table have the same name."
	}

	message := fmt.Sprintf("schema/%s is not valid, and the database refused it:\n\n  %s\n", name, me.Message)
	if explanation != "" {
		message += "\n  " + explanation + "\n"
	}
	message += "\nThis file IS the schema, so nothing can be compared against it until it is a table\n" +
		"the database would accept. Fix the file, not the database."
	return errors.New(message)
}

// The ways a hand-edited table definition usually breaks.
const (
	errSyntax              = 1064
	errCannotAddForeignKey = 1005
	errUnknownColumn       = 1054
	errKeyColumnMissing    = 1072
	errDuplicateKeyName    = 1061
	errForeignKeyIncorrect = 1215
)

// --- the comparison ---------------------------------------------------------------

func diff(ctx context.Context, admin *sql.DB, target, declared string) (Plan, error) {
	driver, err := mysql.Open(admin)
	if err != nil {
		return Plan{}, fmt.Errorf("open driver: %w", err)
	}

	options := &atlas.InspectOptions{Exclude: notOurs}
	current, err := driver.InspectSchema(ctx, target, options)
	if err != nil {
		return Plan{}, fmt.Errorf("read the database: %w", err)
	}
	desired, err := driver.InspectSchema(ctx, declared, options)
	if err != nil {
		return Plan{}, fmt.Errorf("read the declared schema: %w", err)
	}

	// They live under two names, and a name is not a difference we are asking
	// about: the DDL runs against the database it is pointed at.
	current.Name, desired.Name = target, target

	changes, err := driver.SchemaDiff(current, desired)
	if err != nil {
		return Plan{}, fmt.Errorf("compare: %w", err)
	}
	if len(changes) == 0 {
		return Plan{}, nil
	}

	plan, err := driver.PlanChanges(ctx, "sync", changes, func(o *migrate.PlanOptions) {
		qualifier := ""
		o.SchemaQualifier = &qualifier
	})
	if err != nil {
		return Plan{}, fmt.Errorf("plan: %w", err)
	}

	out := Plan{}
	for _, change := range plan.Changes {
		statement := strings.TrimSpace(change.Cmd)
		if statement == "" {
			continue
		}
		if !strings.HasSuffix(statement, ";") {
			statement += ";"
		}
		out.Statements = append(out.Statements, statement)
		if destroysData(change.Source) {
			out.Destructive = append(out.Destructive, statement)
		}
	}
	return out, nil
}

// destroysData reports whether a change loses rows or columns.
//
// It is answered from the CHANGE, not from the text of the statement: reading
// SQL back to work out whether it is dangerous is guessing, and this is not a
// question to guess at.
func destroysData(change atlas.Change) bool {
	switch c := change.(type) {
	case *atlas.DropTable, *atlas.DropSchema:
		return true
	case *atlas.ModifyTable:
		for _, inner := range c.Changes {
			if _, ok := inner.(*atlas.DropColumn); ok {
				return true
			}
		}
	}
	return false
}

// --- the database -------------------------------------------------------------------

// connect opens a connection to the SERVER and reports the database the DSN
// names. The throwaway schema is made and unmade through it, which is why it is
// not bound to a database of its own.
func connect(ctx context.Context, dsn string) (*sql.DB, string, error) {
	target := schemaOf(dsn)
	if target == "" {
		return nil, "", fmt.Errorf("the connection names no database")
	}

	db, err := sql.Open("mysql", swapSchema(dsn, ""))
	if err != nil {
		return nil, "", fmt.Errorf("connect: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("connect: %w", err)
	}
	// One connection: USE and the foreign key switch are per-connection state,
	// and a pool would hand them out at random.
	db.SetMaxOpenConns(1)
	return db, target, nil
}

// ensure creates the database if it is not there, with the charset and collation
// the schema expects. A database on the server's defaults compares text
// differently from the one next to it, and nothing tells you.
func ensure(ctx context.Context, admin *sql.DB, name string) error {
	_, err := admin.ExecContext(ctx,
		"CREATE DATABASE IF NOT EXISTS `"+name+"` CHARACTER SET "+charset+" COLLATE "+collation)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return nil
}

func create(ctx context.Context, admin *sql.DB, name string) error {
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	return ensure(ctx, admin, name)
}

func drop(ctx context.Context, admin *sql.DB, names ...string) {
	for _, name := range names {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
	}
}

func tableNames(ctx context.Context, admin *sql.DB, name string) ([]string, error) {
	rows, err := admin.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = ? AND table_type = 'BASE TABLE'
		 ORDER BY table_name`, name)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

func showCreate(ctx context.Context, admin *sql.DB, schemaName, table string) (string, error) {
	var name, ddl string
	err := admin.QueryRowContext(ctx,
		"SHOW CREATE TABLE `"+schemaName+"`.`"+table+"`").Scan(&name, &ddl)
	if err != nil {
		return "", fmt.Errorf("read the definition of %s: %w", table, err)
	}
	// The server reports its own AUTO_INCREMENT counter, which is a fact about
	// the rows in a database and not about the schema.
	return stripAutoIncrement(ddl), nil
}

func stripAutoIncrement(ddl string) string {
	const marker = " AUTO_INCREMENT="
	start := strings.Index(ddl, marker)
	if start == -1 {
		return ddl
	}
	end := start + len(marker)
	for end < len(ddl) && ddl[end] >= '0' && ddl[end] <= '9' {
		end++
	}
	return ddl[:start] + ddl[end:]
}

// --- the DSN -------------------------------------------------------------------------

// swapSchema points a DSN at a different database, keeping everything else.
func swapSchema(dsn, name string) string {
	prefix, suffix := splitSchema(dsn)
	return prefix + name + suffix
}

func schemaOf(dsn string) string {
	prefix, suffix := splitSchema(dsn)
	return dsn[len(prefix) : len(dsn)-len(suffix)]
}

// splitSchema finds the database name in user:pass@tcp(host:port)/name?params,
// which is where a DSN keeps it: after the last slash, before any question mark.
func splitSchema(dsn string) (prefix, suffix string) {
	slash := strings.LastIndex(dsn, "/")
	if slash == -1 {
		return dsn, ""
	}
	rest := dsn[slash+1:]
	if question := strings.Index(rest, "?"); question != -1 {
		return dsn[:slash+1], rest[question:]
	}
	return dsn[:slash+1], ""
}
