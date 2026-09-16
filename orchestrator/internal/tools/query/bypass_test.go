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

// The one assertion that cannot pass for the wrong reason.
//
// Every other test here asks "was this refused". That is not the question. The
// question is whether the value a policy exists to keep back can be made to
// appear, and a statement that is refused, a statement that is rewritten, and a
// statement the engine rejects are all equally fine answers to it.
//
// So: put a known secret in a real database, throw statements at the tool, and
// assert the secret never comes out. Every route below was VERIFIED to leak
// before the code that stops it was written, against these same servers.
//
// TestOrdinaryWorkStillWorks is the other half and is not optional: without it
// this suite would pass on a guard that refused everything, which is the failure
// this whole exercise exists to avoid.

type engineCase struct {
	name     string
	settings func(t *testing.T) Settings
	secrets  []string
	attacks  []string
	ordinary []string
}

func engineCases() []engineCase {
	return []engineCase{
		{
			name: "mysql",
			settings: func(t *testing.T) Settings {
				if os.Getenv("SAG_TEST_DSN") == "" {
					t.Skip("SAG_TEST_DSN not set")
				}
				return governed(t)
			},
			secrets: []string{"123-45-6789", "the-key"},
			attacks: []string{
				// A CTE named after the thing it reads. The database resolves the
				// inner name to the TABLE, because a CTE cannot see itself.
				"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys",
				"WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers",
				"WITH x AS (SELECT * FROM secret_keys), secret_keys AS (SELECT 1 AS id, 'd' AS value) SELECT * FROM x",
				"WITH x AS (SELECT ssn FROM customers), customers AS (SELECT 'd' AS ssn) SELECT * FROM x",
				"WITH customer_view AS (SELECT * FROM customer_view) SELECT code FROM customer_view",
				"SELECT * FROM (SELECT * FROM (WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys) a) b",
				// Plainly, to prove the controls still hold.
				"SELECT * FROM secret_keys",
				"SELECT ssn FROM customers",
				"SELECT c.ssn FROM customers c",
			},
			ordinary: []string{
				"SELECT id, total FROM orders",
				"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
				"WITH a AS (SELECT id FROM orders), b AS (SELECT id FROM a) SELECT * FROM b",
				"WITH orders AS (SELECT id FROM orders) SELECT * FROM orders",
				"SELECT COUNT(*) FROM customers",
				"SELECT id, email FROM customers WHERE ssn IS NOT NULL",
				"SELECT * FROM orders",
			},
		},
		{
			name: "postgres",
			settings: func(t *testing.T) Settings {
				if os.Getenv("SAG_TEST_PG_DSN") == "" {
					t.Skip("SAG_TEST_PG_DSN not set")
				}
				return Settings{Connection: pgFixture(t), Access: AccessBoth, Policy: sqlguard.Policy{
					TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
					FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
				}}
			},
			secrets: []string{"123-45-6789", "the-key"},
			attacks: []string{
				"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys",
				"WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers",
				"WITH x AS (SELECT * FROM secret_keys), secret_keys AS (SELECT 1 AS id) SELECT * FROM x",
				"SELECT * FROM (WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys) a",
				// A star inside an expression: the whole row as one value, with no
				// select item left to put the stand-in in.
				"SELECT ROW(c.*) FROM customers c",
				"SELECT ROW(c.*)::text FROM customers c",
				// A query, or a table, handed over as TEXT and read by the server.
				"SELECT query_to_xml('SELECT ssn FROM customers', true, true, '')",
				"SELECT query_to_xml('SELECT value FROM secret_keys', true, true, '')",
				"SELECT table_to_xml('secret_keys', true, true, '')",
				"SELECT table_to_xml('customers', true, true, '')",
				"SELECT database_to_xml(true, true, '')",
				// Controls.
				"SELECT * FROM secret_keys",
				"SELECT c.ssn FROM customers c",
				"SELECT row_to_json(c) FROM customers c",
			},
			ordinary: []string{
				"SELECT id, total FROM orders",
				"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
				"WITH RECURSIVE n AS (SELECT 1 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 5) SELECT * FROM n",
				"SELECT ROW(o.*) FROM orders o",
				"SELECT COUNT(*) FROM customers",
				"SELECT lower(email) FROM customers",
				"SELECT * FROM orders",
			},
		},
		{
			name: "sqlserver",
			settings: func(t *testing.T) Settings {
				if os.Getenv("SAG_TEST_MSSQL_DSN") == "" {
					t.Skip("SAG_TEST_MSSQL_DSN not set")
				}
				return Settings{Connection: msFixture(t), Access: AccessBoth, Policy: sqlguard.Policy{
					TableMode: sqlguard.ModeDenylist, Tables: "payroll",
					FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
				}}
			},
			secrets: []string{msRealSSN, "1000"},
			attacks: []string{
				"SELECT * FROM payroll",
				"SELECT ssn FROM customers",
				"WITH payroll AS (SELECT * FROM payroll) SELECT * FROM payroll",
				"SELECT 1 AS [it's] ; SELECT ssn FROM customers",
			},
			ordinary: []string{
				"SELECT id, total FROM orders",
				"SELECT COUNT(*) FROM customers",
				"SELECT id, email FROM customers",
			},
		},
	}
}

func TestTheSecretNeverComesOut(t *testing.T) {
	for _, c := range engineCases() {
		t.Run(c.name, func(t *testing.T) {
			s := c.settings(t)
			for _, sql := range c.attacks {
				res, reason, err := run(context.Background(), s, sql, nil, 0)
				if reason != "" || err != nil || res == nil {
					continue // refused, or the engine would not run it: both fine
				}
				out := fmt.Sprint(res.Rows)
				for _, secret := range c.secrets {
					if strings.Contains(out, secret) {
						t.Errorf("LEAKED %q\n  statement: %s\n  rows: %.200s", secret, sql, out)
					}
				}
			}
		})
	}
}

// The half that stops the suite above passing on a guard that refuses
// everything.
func TestOrdinaryWorkStillWorks(t *testing.T) {
	for _, c := range engineCases() {
		t.Run(c.name, func(t *testing.T) {
			s := c.settings(t)
			for _, sql := range c.ordinary {
				res, reason, err := run(context.Background(), s, sql, nil, 0)
				switch {
				case reason != "":
					t.Errorf("REFUSED, and should not be: %s\n  %s", sql, reason)
				case err != nil:
					t.Errorf("%s: %v", sql, err)
				case res == nil:
					t.Errorf("%s: no result", sql)
				}
			}
		})
	}
}

// The capability gate, on a tool with no policy at all, which is where it is the
// only thing standing. Each of these was verified to get through before the gate
// was rewritten.
func TestTheCapabilityGateCannotBeCommentedPast(t *testing.T) {
	for driver, statements := range map[string][]string{
		"mysql": {
			"CREATE TRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE /*a b c d*/ TRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE /* padding words to push it along */ TRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE\n-- a comment line\nTRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE TRIGGER/**/t1 BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE OR REPLACE DEFINER=`root`@`localhost` TRIGGER t3 BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE /*!50000 TRIGGER */ t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
		},
		"postgres": {
			"CREATE FUNCTION\"f1\"() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
			"CREATE /*a b c d*/ TRIGGER t AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION f()",
			"CREATE OR REPLACE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
		},
		"sqlserver": {
			"CREATE OR ALTER /*x*/ TRIGGER trg ON customers AFTER INSERT AS SELECT 1",
			"CREATE PROC dbo.p AS BEGIN SELECT 1 END",
		},
	} {
		off := settingsFor(t, driver)
		for _, sql := range statements {
			if ok, _ := permitted(off, sql); ok {
				t.Errorf("%s: got through with nothing ticked: %s", driver, sql)
			}
		}
		// And with the box ticked, the plain form still works.
		on := settingsFor(t, driver, datasource.CapCreateTriggers, datasource.CapCreateRoutines)
		for _, sql := range []string{
			"CREATE TRIGGER t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
			"CREATE TABLE t (id int)",
			"DROP TABLE t",
			"ALTER TABLE t ADD COLUMN note text",
		} {
			if ok, reason := permitted(on, sql); !ok {
				t.Errorf("%s: refused with the boxes ticked: %s -> %s", driver, sql, reason)
			}
		}
	}
}

// A second statement cannot ride along behind a comment or a bracket-quoted
// name. Both were verified to smuggle an INSERT through before this.
func TestASecondStatementCannotRideAlong(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlserver"} {
		s := settingsFor(t, driver)
		for _, sql := range []string{
			"SELECT 1 /* don't stop */ ; INSERT INTO scratch VALUES (97, 'x')",
			"SELECT 1 AS [it's] ; INSERT INTO scratch VALUES (98, 'x')",
			"SELECT 1 ; DROP TABLE customers",
			"/* /* */ SELECT 1 */ CALL p_read()",
		} {
			if ok, _ := permitted(s, sql); ok {
				t.Errorf("%s: two statements read as one: %s", driver, sql)
			}
		}
	}
}
