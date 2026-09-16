package datasource_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	_ "flexie.io/sag/internal/datasource/mysql"
	_ "flexie.io/sag/internal/datasource/postgres"
	_ "flexie.io/sag/internal/datasource/sqlserver"
)

// Does each engine really hand back its routines, with their bodies?
//
// Against real servers, because the three catalog queries behind this are the
// kind of thing that reads correctly and returns nothing: a wrong system view, a
// column that exists under another name, a filter that excludes the very rows
// wanted. Nothing here can be established by looking at the code.
//
// Each case creates a routine whose body mentions a table by name, then asks
// the driver what the database holds and looks for that name IN THE BODY. A
// routine listed without its body is the failure that matters, because an
// unreadable body is what the guard refuses on.

type engine struct {
	driver string
	env    string
	create []string
	drop   string
	want   string
}

func TestEachEngineReportsItsRoutinesWithBodies(t *testing.T) {
	engines := []engine{
		{
			driver: "mysql", env: "SAG_TEST_DSN",
			create: []string{
				"DROP PROCEDURE IF EXISTS sag_probe",
				"CREATE PROCEDURE sag_probe() BEGIN SELECT 1 FROM sag_probe_table; END",
			},
			drop: "DROP PROCEDURE IF EXISTS sag_probe",
			want: "sag_probe_table",
		},
		{
			driver: "postgres", env: "SAG_TEST_PG_DSN",
			create: []string{
				"DROP FUNCTION IF EXISTS sag_probe()",
				"CREATE FUNCTION sag_probe() RETURNS int AS $$ SELECT 1 FROM sag_probe_table $$ LANGUAGE sql",
			},
			drop: "DROP FUNCTION IF EXISTS sag_probe()",
			want: "sag_probe_table",
		},
		{
			driver: "sqlserver", env: "SAG_TEST_MSSQL_DSN",
			create: []string{
				"IF OBJECT_ID('dbo.sag_probe') IS NOT NULL DROP PROCEDURE dbo.sag_probe",
				"CREATE PROCEDURE dbo.sag_probe AS BEGIN SELECT 1 FROM sag_probe_table; END",
			},
			drop: "IF OBJECT_ID('dbo.sag_probe') IS NOT NULL DROP PROCEDURE dbo.sag_probe",
			want: "sag_probe_table",
		},
	}

	ran := 0
	for _, e := range engines {
		t.Run(e.driver, func(t *testing.T) {
			dsn := os.Getenv(e.env)
			if dsn == "" {
				t.Skipf("%s not set", e.env)
			}
			cfg, err := configFor(e.driver, dsn)
			if err != nil {
				t.Fatalf("read %s: %v", e.env, err)
			}
			conn, err := datasource.Open(cfg)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = conn.Close() }()

			// The table the body names need not exist for the body to be stored
			// on most engines, but SQL Server checks at creation, so make it.
			_, _ = conn.Exec(context.Background(), "CREATE TABLE sag_probe_table (id int)", nil)
			for _, stmt := range e.create {
				if _, err := conn.Exec(context.Background(), stmt, nil); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			t.Cleanup(func() {
				_, _ = conn.Exec(context.Background(), e.drop, nil)
				_, _ = conn.Exec(context.Background(), "DROP TABLE sag_probe_table", nil)
			})

			schema, err := conn.Schema(context.Background())
			if err != nil {
				t.Fatalf("schema: %v", err)
			}

			var found *datasource.Routine
			for i := range schema.Routines {
				if strings.EqualFold(schema.Routines[i].Name, "sag_probe") {
					found = &schema.Routines[i]
				}
			}
			if found == nil {
				var names []string
				for _, r := range schema.Routines {
					names = append(names, r.Name)
				}
				t.Fatalf("sag_probe is not among the %d routines reported: %v",
					len(schema.Routines), names)
			}
			if !strings.Contains(found.Body, e.want) {
				t.Fatalf("the body came back without %q in it: %q", e.want, found.Body)
			}
			t.Logf("  %s: %d routines, sag_probe body %d chars, names %q",
				e.driver, len(schema.Routines), len(found.Body), e.want)
			ran++
		})
	}
	if ran == 0 {
		t.Skip("no database was reachable; this proved nothing")
	}
}

// configFor turns a test DSN into a connection, per engine.
func configFor(driver, dsn string) (datasource.Config, error) {
	switch driver {
	case "mysql":
		// user:pass@tcp(host:port)/db?params
		creds, rest, _ := strings.Cut(dsn, "@tcp(")
		user, pass, _ := strings.Cut(creds, ":")
		hostPort, tail, _ := strings.Cut(rest, ")/")
		host, port, _ := strings.Cut(hostPort, ":")
		db, _, _ := strings.Cut(tail, "?")
		return datasource.Config{
			Driver: "mysql", Host: host, Port: atoi(port, 3306), Database: db,
			Username: user, Password: pass, TLS: datasource.TLS{Mode: "disable"},
		}, nil
	case "postgres", "sqlserver":
		scheme := "postgres://"
		def := 5432
		if driver == "sqlserver" {
			scheme, def = "sqlserver://", 1433
		}
		rest := strings.TrimPrefix(dsn, scheme)
		creds, address, _ := strings.Cut(rest, "@")
		user, pass, _ := strings.Cut(creds, ":")
		hostPort, db, _ := strings.Cut(address, "/")
		db, _, _ = strings.Cut(db, "?")
		host, port, _ := strings.Cut(hostPort, ":")
		if db == "" {
			db = map[string]string{"postgres": "postgres", "sqlserver": "master"}[driver]
		}
		return datasource.Config{
			Driver: driver, Host: host, Port: atoi(port, def), Database: db,
			Username: user, Password: pass, TLS: datasource.TLS{Mode: "disable"},
		}, nil
	}
	return datasource.Config{}, nil
}

func atoi(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
