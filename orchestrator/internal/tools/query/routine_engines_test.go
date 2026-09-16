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

// Running the database's own stored code, against real MySQL and PostgreSQL
// servers. The SQL Server suite sits next door.
//
// The analyzers' own tests read bodies a test wrote. These read what the SERVER
// stored and handed back through the driver's catalogue query, which is the half
// nothing else can check: each engine keeps a body in its own shape, and a rule
// about what a routine reads is worth nothing if what comes back is not the body.
//
// Two routes in, and both are covered, because they reach the same code: CALL
// says what it is doing, and a stored function in a select list does not.

const liveSecret = "sk-live-9f31"

type liveEngine struct {
	driver, env string
	setup       []string
	teardown    []string
	// clean runs and returns rows; denied and hidden are refused for what their
	// bodies read; leak is the same thing reached as a function.
	clean, denied, hidden, leak string
	// trigger is a statement the trigger capability governs, and dropTrigger
	// takes it away again once it has been shown to run.
	trigger, dropTrigger string
}

func liveEngines() []liveEngine {
	return []liveEngine{{
		driver: "mysql", env: "SAG_TEST_DSN",
		setup: []string{
			"DROP FUNCTION IF EXISTS r_leak",
			"DROP PROCEDURE IF EXISTS r_clean", "DROP PROCEDURE IF EXISTS r_denied",
			"DROP PROCEDURE IF EXISTS r_hidden",
			"DROP TABLE IF EXISTS r_orders", "DROP TABLE IF EXISTS r_secret", "DROP TABLE IF EXISTS r_people",
			"CREATE TABLE r_orders (id int, total int)",
			"CREATE TABLE r_secret (id int, code varchar(20))",
			"CREATE TABLE r_people (id int, ssn varchar(20))",
			"INSERT INTO r_orders VALUES (1, 99)",
			"INSERT INTO r_secret VALUES (1, '" + liveSecret + "')",
			"INSERT INTO r_people VALUES (1, '123-45-6789')",
			"CREATE PROCEDURE r_clean() SELECT id, total FROM r_orders",
			"CREATE PROCEDURE r_denied() SELECT id, code FROM r_secret",
			"CREATE PROCEDURE r_hidden() SELECT id, ssn FROM r_people",
			"CREATE FUNCTION r_leak() RETURNS varchar(20) READS SQL DATA RETURN (SELECT code FROM r_secret LIMIT 1)",
		},
		teardown: []string{
			"DROP FUNCTION IF EXISTS r_leak",
			"DROP PROCEDURE IF EXISTS r_clean", "DROP PROCEDURE IF EXISTS r_denied",
			"DROP PROCEDURE IF EXISTS r_hidden",
			"DROP TABLE IF EXISTS r_orders", "DROP TABLE IF EXISTS r_secret", "DROP TABLE IF EXISTS r_people",
		},
		clean: "CALL r_clean()", denied: "CALL r_denied()",
		hidden: "CALL r_hidden()", leak: "SELECT r_leak() AS code",
		trigger:     "CREATE TRIGGER r_trg BEFORE INSERT ON r_orders FOR EACH ROW SET NEW.total = NEW.total",
		dropTrigger: "DROP TRIGGER IF EXISTS r_trg",
	}, {
		driver: "postgres", env: "SAG_TEST_PG_DSN",
		setup: []string{
			"DROP FUNCTION IF EXISTS r_leak()", "DROP FUNCTION IF EXISTS r_clean_fn()",
			"DROP PROCEDURE IF EXISTS r_clean()", "DROP PROCEDURE IF EXISTS r_denied()",
			"DROP PROCEDURE IF EXISTS r_hidden()",
			"DROP TABLE IF EXISTS r_orders", "DROP TABLE IF EXISTS r_secret", "DROP TABLE IF EXISTS r_people",
			"CREATE TABLE r_orders (id int, total int)",
			"CREATE TABLE r_secret (id int, code text)",
			"CREATE TABLE r_people (id int, ssn text)",
			"INSERT INTO r_orders VALUES (1, 99)",
			"INSERT INTO r_secret VALUES (1, '" + liveSecret + "')",
			"INSERT INTO r_people VALUES (1, '123-45-6789')",
			// A procedure on this engine returns no rows, so the one that must be
			// seen to RUN is a function.
			"CREATE FUNCTION r_clean_fn() RETURNS int LANGUAGE sql AS $$ SELECT total FROM r_orders LIMIT 1 $$",
			"CREATE PROCEDURE r_denied() LANGUAGE sql AS $$ SELECT id, code FROM r_secret $$",
			"CREATE PROCEDURE r_hidden() LANGUAGE sql AS $$ SELECT id, ssn FROM r_people $$",
			"CREATE FUNCTION r_leak() RETURNS text LANGUAGE sql AS $$ SELECT code FROM r_secret LIMIT 1 $$",
			"CREATE FUNCTION r_trgfn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$",
		},
		teardown: []string{
			"DROP TRIGGER IF EXISTS r_trg ON r_orders", "DROP FUNCTION IF EXISTS r_trgfn()",
			"DROP FUNCTION IF EXISTS r_leak()", "DROP FUNCTION IF EXISTS r_clean_fn()",
			"DROP PROCEDURE IF EXISTS r_denied()", "DROP PROCEDURE IF EXISTS r_hidden()",
			"DROP TABLE IF EXISTS r_orders", "DROP TABLE IF EXISTS r_secret", "DROP TABLE IF EXISTS r_people",
		},
		clean: "SELECT r_clean_fn() AS total", denied: "CALL r_denied()",
		hidden: "CALL r_hidden()", leak: "SELECT r_leak() AS code",
		trigger:     "CREATE TRIGGER r_trg BEFORE INSERT ON r_orders FOR EACH ROW EXECUTE FUNCTION r_trgfn()",
		dropTrigger: "DROP TRIGGER IF EXISTS r_trg ON r_orders",
	}}
}

// liveDatabase is this suite's own, per engine. A database of its own rather
// than the one the DSN names, because every package's tests run beside every
// other package's: writing into the shared one made the identity suite fail on
// rows it had just written.
const liveDatabase = "flexie_sag_routine_test"

func liveSettings(t *testing.T, e liveEngine) Settings {
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
			if _, err := conn.Exec(context.Background(), stmt, nil); err != nil && strict &&
				!strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}

	admin := liveConfig(e.driver, dsn)
	exec(admin, []string{"DROP DATABASE IF EXISTS " + liveDatabase, "CREATE DATABASE " + liveDatabase}, true)
	t.Cleanup(func() { exec(admin, []string{"DROP DATABASE IF EXISTS " + liveDatabase}, false) })

	cfg := admin
	cfg.Database = liveDatabase
	exec(cfg, e.setup, true)
	t.Cleanup(func() { exec(cfg, e.teardown, false) })
	return Settings{
		Connection: cfg, Access: AccessBoth,
		Allowed: map[string]bool{datasource.CapCallRoutines: true},
		Policy: sqlguard.Policy{
			TableMode: sqlguard.ModeDenylist, Tables: "r_secret",
			FieldMode: sqlguard.ModeDenylist, Fields: "r_people.ssn",
		},
	}
}

func TestLiveARoutineIsHeldToThePolicyByItsBody(t *testing.T) {
	for _, e := range liveEngines() {
		t.Run(e.driver, func(t *testing.T) {
			governed := liveSettings(t, e)

			// The one that must run, and does. Without it the refusals below could
			// not be told from a feature that never works.
			res, reason, err := run(context.Background(), governed, e.clean, nil, 0)
			if err != nil || reason != "" {
				t.Fatalf("a clean routine was refused: reason=%q err=%v", reason, err)
			}
			if res == nil || res.RowCount == 0 {
				t.Fatalf("%s ran but returned nothing", e.clean)
			}

			for _, c := range []struct{ sql, because string }{
				{e.denied, "its body reads a table the policy keeps back"},
				{e.hidden, "its body reads a field the policy hides, and a body cannot be rewritten"},
				{e.leak, "the same code reached as a function is still that code"},
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
				if strings.Contains(fmt.Sprint(res), liveSecret) {
					t.Errorf("%s was refused and the secret came back anyway", c.sql)
				}
				t.Logf("  %-9s refused %-24s (%s)", e.driver, c.sql, c.because)
			}
		})
	}
}

// The control for the whole suite: ungoverned, that function really does hand
// over the value the policy exists to keep back. A refusal is worth something
// only if what it refused was going to happen.
func TestLiveUngovernedTheFunctionReallyDoesLeak(t *testing.T) {
	for _, e := range liveEngines() {
		t.Run(e.driver, func(t *testing.T) {
			open := liveSettings(t, e)
			open.Policy = sqlguard.Policy{}
			res, reason, err := run(context.Background(), open, e.leak, nil, 0)
			if err != nil || reason != "" {
				t.Fatalf("the control could not run %s: reason=%q err=%v", e.leak, reason, err)
			}
			if !strings.Contains(fmt.Sprint(res.Rows), liveSecret) {
				t.Fatalf("the control failed: %s returned no secret even ungoverned: %+v", e.leak, res.Rows)
			}
			t.Logf("  %-9s ungoverned, %s returns %s", e.driver, e.leak, liveSecret)
		})
	}
}

// And the capability itself: with the box unticked nothing runs, by either
// route, however clean the body is.
func TestLiveWithTheBoxUntickedNothingRuns(t *testing.T) {
	for _, e := range liveEngines() {
		t.Run(e.driver, func(t *testing.T) {
			off := liveSettings(t, e)
			off.Allowed = map[string]bool{}
			for _, sql := range []string{e.clean, e.denied, e.leak} {
				if _, reason, err := run(context.Background(), off, sql, nil, 0); reason == "" {
					t.Errorf("%s ran with the capability off (err=%v)", sql, err)
				}
			}
		})
	}
}

// liveConfig turns a test DSN into a connection.
func liveConfig(driver, dsn string) datasource.Config {
	if driver == "mysql" {
		creds, rest, _ := strings.Cut(dsn, "@tcp(")
		user, pass, _ := strings.Cut(creds, ":")
		hostPort, tail, _ := strings.Cut(rest, ")/")
		host, port, _ := strings.Cut(hostPort, ":")
		db, _, _ := strings.Cut(tail, "?")
		p := 3306
		_, _ = fmt.Sscanf(port, "%d", &p)
		return datasource.Config{Driver: "mysql", Host: host, Port: p, Database: db,
			Username: user, Password: pass, TLS: datasource.TLS{Mode: "disable"}}
	}
	rest := strings.TrimPrefix(dsn, "postgres://")
	creds, address, _ := strings.Cut(rest, "@")
	user, pass, _ := strings.Cut(creds, ":")
	hostPort, db, _ := strings.Cut(address, "/")
	db, _, _ = strings.Cut(db, "?")
	host, port, _ := strings.Cut(hostPort, ":")
	p := 5432
	_, _ = fmt.Sscanf(port, "%d", &p)
	if db == "" {
		db = "postgres"
	}
	return datasource.Config{Driver: "postgres", Host: host, Port: p, Database: db,
		Username: user, Password: pass, TLS: datasource.TLS{Mode: "disable"}}
}

// Making stored code, against real servers.
//
// The gate for it is text rather than a parse, because it has to work on a tool
// with no policy, which is where there is no parse at all. So the half that
// cannot be checked from here is whether the statement the gate lets through is
// one the ENGINE accepts: a rule that refuses correctly and permits something
// the database then rejects has proved nothing.
func TestLiveMakingATriggerNeedsItsOwnBox(t *testing.T) {
	for _, e := range liveEngines() {
		t.Run(e.driver, func(t *testing.T) {
			s := liveSettings(t, e)
			s.Policy = sqlguard.Policy{}
			s.Allowed = map[string]bool{}

			if _, reason, err := run(context.Background(), s, e.trigger, nil, 0); reason == "" {
				t.Fatalf("the trigger was made with nothing ticked (err=%v)", err)
			}

			s.Allowed = map[string]bool{datasource.CapCreateTriggers: true}
			t.Cleanup(func() {
				drop := s
				drop.Allowed = map[string]bool{datasource.CapCreateTriggers: true}
				_, _, _ = run(context.Background(), drop, e.dropTrigger, nil, 0)
			})
			if _, reason, err := run(context.Background(), s, e.trigger, nil, 0); reason != "" || err != nil {
				t.Fatalf("with the box ticked the trigger was not made: reason=%q err=%v", reason, err)
			}
			t.Logf("  %-9s made and removed a trigger, and could not without the box", e.driver)
		})
	}
}
