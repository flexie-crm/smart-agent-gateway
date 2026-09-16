package query

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// A routine built out of other routines, on real servers.
//
// A routine that called another used to be refused at the first link, on the
// reasoning that its real work happened somewhere this side never saw. Every
// body is in the same catalogue, so it does not: the chain is followed to the
// end and each link is held to the policy like the first.
//
// This is the half the analyzers' own tests cannot check. There the bodies are
// what a test wrote; here they are what the SERVER stored and handed back
// through the catalogue query, in each engine's own shape, and what the server
// would really have returned had the statement run.

const chainSecret = "sk-chain-4d02"

type chainEngine struct {
	driver, env string
	setup       []string
	// clean is three routines deep and reads nothing anybody may not see.
	// denied ends at a table the policy keeps back, hidden at a field it hides,
	// and each is reached through two links that are themselves clean.
	clean, denied, hidden string
	// make builds a routine out of a clean chain, and makeBad out of a dirty one.
	make, makeBad string
}

func chainEngines() []chainEngine {
	return []chainEngine{{
		driver: "mysql", env: "SAG_TEST_DSN",
		setup: []string{
			"CREATE TABLE c_orders (id int, total int)",
			"CREATE TABLE c_secret (id int, code varchar(20))",
			"CREATE TABLE c_people (id int, ssn varchar(20))",
			"INSERT INTO c_orders VALUES (1, 99)",
			"INSERT INTO c_secret VALUES (1, '" + chainSecret + "')",
			"INSERT INTO c_people VALUES (1, '123-45-6789')",
			// On this engine the chain is between FUNCTIONS. A procedure body
			// holding a CALL is not something this parser can read at all, so a
			// chain of procedures is refused one link earlier; see the note on
			// TestLiveAChainOfRoutines.
			"CREATE FUNCTION c_leaf() RETURNS int READS SQL DATA RETURN (SELECT total FROM c_orders LIMIT 1)",
			"CREATE FUNCTION c_mid() RETURNS int READS SQL DATA RETURN c_leaf()",
			"CREATE FUNCTION c_top() RETURNS int READS SQL DATA RETURN c_mid()",
			"CREATE FUNCTION d_leaf() RETURNS varchar(20) READS SQL DATA RETURN (SELECT code FROM c_secret LIMIT 1)",
			"CREATE FUNCTION d_mid() RETURNS varchar(20) READS SQL DATA RETURN d_leaf()",
			"CREATE FUNCTION d_top() RETURNS varchar(20) READS SQL DATA RETURN d_mid()",
			"CREATE FUNCTION h_leaf() RETURNS varchar(20) READS SQL DATA RETURN (SELECT ssn FROM c_people LIMIT 1)",
			"CREATE FUNCTION h_top() RETURNS varchar(20) READS SQL DATA RETURN h_leaf()",
		},
		clean:   "SELECT c_top() AS v",
		denied:  "SELECT d_top() AS v",
		hidden:  "SELECT h_top() AS v",
		make:    "CREATE PROCEDURE m_ok() BEGIN SELECT c_top(); END",
		makeBad: "CREATE PROCEDURE m_bad() BEGIN SELECT d_top(); END",
	}, {
		driver: "postgres", env: "SAG_TEST_PG_DSN",
		setup: []string{
			"CREATE TABLE c_orders (id int, total int)",
			"CREATE TABLE c_secret (id int, code varchar(20))",
			"CREATE TABLE c_people (id int, ssn varchar(20))",
			"INSERT INTO c_orders VALUES (1, 99)",
			"INSERT INTO c_secret VALUES (1, '" + chainSecret + "')",
			"INSERT INTO c_people VALUES (1, '123-45-6789')",
			"CREATE FUNCTION c_leaf() RETURNS int LANGUAGE sql AS $$ SELECT total FROM c_orders LIMIT 1 $$",
			"CREATE FUNCTION c_mid() RETURNS int LANGUAGE sql AS $$ SELECT c_leaf() $$",
			"CREATE FUNCTION c_top() RETURNS int LANGUAGE sql AS $$ SELECT c_mid() $$",
			"CREATE FUNCTION d_leaf() RETURNS varchar LANGUAGE sql AS $$ SELECT code FROM c_secret LIMIT 1 $$",
			"CREATE FUNCTION d_mid() RETURNS varchar LANGUAGE sql AS $$ SELECT d_leaf() $$",
			"CREATE FUNCTION d_top() RETURNS varchar LANGUAGE sql AS $$ SELECT d_mid() $$",
			"CREATE FUNCTION h_leaf() RETURNS varchar LANGUAGE sql AS $$ SELECT ssn FROM c_people LIMIT 1 $$",
			"CREATE FUNCTION h_top() RETURNS varchar LANGUAGE sql AS $$ SELECT h_leaf() $$",
		},
		clean:   "SELECT c_top() AS v",
		denied:  "SELECT d_top() AS v",
		hidden:  "SELECT h_top() AS v",
		make:    "CREATE FUNCTION m_ok() RETURNS int LANGUAGE sql AS $$ SELECT c_top() $$",
		makeBad: "CREATE FUNCTION m_bad() RETURNS varchar LANGUAGE sql AS $$ SELECT d_top() $$",
	}, {
		driver: "sqlserver", env: "SAG_TEST_MSSQL_DSN",
		setup: []string{
			"CREATE TABLE c_orders (id int, total int)",
			"CREATE TABLE c_secret (id int, code varchar(20))",
			"CREATE TABLE c_people (id int, ssn varchar(20))",
			"INSERT INTO c_orders VALUES (1, 99)",
			"INSERT INTO c_secret VALUES (1, '" + chainSecret + "')",
			"INSERT INTO c_people VALUES (1, '123-45-6789')",
			// Here the chain is between PROCEDURES, which is the shape EXEC takes
			// and the one the other engines cannot show.
			"CREATE PROCEDURE c_leaf AS BEGIN SELECT id, total FROM c_orders; END",
			"CREATE PROCEDURE c_mid AS BEGIN EXEC dbo.c_leaf; END",
			"CREATE PROCEDURE c_top AS BEGIN EXEC dbo.c_mid; END",
			"CREATE PROCEDURE d_leaf AS BEGIN SELECT id, code FROM c_secret; END",
			"CREATE PROCEDURE d_mid AS BEGIN EXEC dbo.d_leaf; END",
			"CREATE PROCEDURE d_top AS BEGIN EXEC dbo.d_mid; END",
			"CREATE PROCEDURE h_leaf AS BEGIN SELECT id, ssn FROM c_people; END",
			"CREATE PROCEDURE h_top AS BEGIN EXEC dbo.h_leaf; END",
		},
		clean:   "EXEC dbo.c_top",
		denied:  "EXEC dbo.d_top",
		hidden:  "EXEC dbo.h_top",
		make:    "CREATE PROCEDURE m_ok AS BEGIN EXEC dbo.c_top; END",
		makeBad: "CREATE PROCEDURE m_bad AS BEGIN EXEC dbo.d_top; END",
	}}
}

// chainDatabase is this suite's own, per engine, for the same reason every other
// live suite here has one: the packages' tests run beside each other.
const chainDatabase = "flexie_sag_chain_test"

func chainSettings(t *testing.T, e chainEngine) Settings {
	t.Helper()
	dsn := os.Getenv(e.env)
	if dsn == "" {
		t.Skipf("%s not set", e.env)
	}
	// The database below is about to be remade, so a guard cached from a run
	// that described the old one would be describing nothing.
	guards.forget()

	exec := func(cfg datasource.Config, statements []string, strict bool) {
		t.Helper()
		conn, err := datasource.Open(cfg)
		if err != nil {
			if strict {
				t.Fatalf("open: %v", err)
			}
			return
		}
		defer func() { _ = conn.Close() }()
		for _, stmt := range statements {
			if _, err := conn.Exec(context.Background(), stmt, nil); err != nil && strict {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}

	// Each engine's DSN is written in its own spelling, and the two readers for
	// them already exist beside this file.
	admin := liveConfig(e.driver, dsn)
	if e.driver == "sqlserver" {
		cfg, err := msConfig(dsn)
		if err != nil {
			t.Fatalf("%s: %v", e.env, err)
		}
		admin = cfg
	}
	exec(admin, []string{"DROP DATABASE IF EXISTS " + chainDatabase}, false)
	exec(admin, []string{"CREATE DATABASE " + chainDatabase}, true)
	t.Cleanup(func() { exec(admin, []string{"DROP DATABASE IF EXISTS " + chainDatabase}, false) })

	cfg := admin
	cfg.Database = chainDatabase
	exec(cfg, e.setup, true)
	return Settings{
		Connection: cfg, Access: AccessBoth,
		Allowed: map[string]bool{
			datasource.CapCallRoutines:   true,
			datasource.CapCreateRoutines: true,
		},
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "c_secret",
			FieldMode: sqlguard.ModeDenylist, Fields: "c_people.ssn",
		},
	}
}

// TestLiveAChainOfRoutines runs the whole of it against each real server.
//
// A note on MySQL and MariaDB, measured rather than assumed: the parser this
// package carries has no rule for CALL inside a procedure body, so a body
// holding one fails to parse and the routine is refused as unreadable, one link
// before the chain walk is reached. The chain there follows stored FUNCTIONS,
// which is what an expression calls, and that is the shape the fixture uses. The
// other two engines are shown with the shape each of them takes.
func TestLiveAChainOfRoutines(t *testing.T) {
	for _, e := range chainEngines() {
		t.Run(e.driver, func(t *testing.T) {
			governed := chainSettings(t, e)

			// Three routines deep, every link clean. Without this the refusals
			// below could not be told from a feature that never works.
			res, reason, err := run(context.Background(), governed, e.clean, nil, 0)
			if err != nil || reason != "" {
				t.Fatalf("a clean chain was refused: reason=%q err=%v", reason, err)
			}
			if res == nil || res.RowCount == 0 {
				t.Fatalf("%s ran but returned nothing", e.clean)
			}
			t.Logf("  %-9s ran     %-24s -> %v", e.driver, e.clean, res.Rows)

			for _, c := range []struct{ sql, link, because string }{
				{e.denied, "d_leaf", "the third link reads a table the policy keeps back"},
				{e.hidden, "h_leaf", "the second link returns a field the policy hides"},
			} {
				res, reason, err := run(context.Background(), governed, c.sql, nil, 0)
				if err != nil {
					t.Errorf("%s: %v", c.sql, err)
					continue
				}
				if reason == "" {
					t.Errorf("%s RAN, and should not have: %s (rows %v)", c.sql, c.because, res.Rows)
					continue
				}
				if strings.Contains(fmt.Sprint(res), chainSecret) {
					t.Errorf("%s was refused and the secret came back anyway", c.sql)
				}
				// Naming the link is the point of walking the chain rather than
				// refusing at the top: otherwise somebody has to guess which of
				// three routines is the problem.
				if !strings.Contains(reason, c.link) {
					t.Errorf("%s was refused without naming the link that broke it.\n  said: %s\n  wanted it to name: %s",
						c.sql, reason, c.link)
				}
				t.Logf("  %-9s refused %-24s (%s)", e.driver, c.sql, c.because)
			}
		})
	}
}

// And the same walk at the moment a routine is MADE, so one can be written out
// of the routines already in the database.
func TestLiveMakingARoutineOutOfRoutines(t *testing.T) {
	for _, e := range chainEngines() {
		t.Run(e.driver, func(t *testing.T) {
			governed := chainSettings(t, e)

			if _, reason, err := run(context.Background(), governed, e.make, nil, 0); err != nil || reason != "" {
				t.Fatalf("making a routine out of a clean chain was refused: reason=%q err=%v", reason, err)
			}
			res, reason, err := run(context.Background(), governed, e.makeBad, nil, 0)
			if err != nil {
				t.Fatalf("%s: %v", e.makeBad, err)
			}
			if reason == "" {
				t.Errorf("%s WAS MADE, and it wraps a chain ending at a table the policy keeps back (%v)",
					e.makeBad, res)
			} else {
				t.Logf("  %-9s refused %s", e.driver, e.makeBad)
			}
		})
	}
}

// The control for the whole file: ungoverned, that chain really does hand over
// the value the policy exists to keep back. A refusal is worth something only if
// what it refused was going to happen.
func TestLiveUngovernedTheChainReallyDoesLeak(t *testing.T) {
	for _, e := range chainEngines() {
		t.Run(e.driver, func(t *testing.T) {
			open := chainSettings(t, e)
			open.Policy = sqlguard.Policy{}
			res, reason, err := run(context.Background(), open, e.denied, nil, 0)
			if err != nil || reason != "" {
				t.Fatalf("%s: reason=%q err=%v", e.denied, reason, err)
			}
			if !strings.Contains(fmt.Sprint(res.Rows), chainSecret) {
				t.Fatalf("with no policy the chain returned %v, and this file's refusals prove nothing",
					res.Rows)
			}
			t.Logf("  %-9s ungoverned, the chain returns %v", e.driver, res.Rows)
		})
	}
}
