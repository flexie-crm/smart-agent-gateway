package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// The whole policy surface, against real databases.
//
// A policy is not one setting and not two. It is:
//
//	the table list      read as an allowlist, as a denylist, or not written
//	the field list      read as an allowlist, as a denylist, or not written
//	the access mode     read, write, or both
//	call routines       on or off
//	create routines     on or off
//	create triggers     on or off
//
// The first pass over the shapes ran ONE corner of that (denylist, denylist,
// routines off, both), which meant every CALL and EXEC was refused by the
// checkbox rather than by anything reading a routine's body, and the body
// reading is the whole of what has to hold when an administrator ticks it.
//
// So the statements run under every combination that can change the answer. Two
// assertions, and neither can pass for the wrong reason:
//
//   - whatever the guard decided, the secret must not be in the rows
//   - making stored code needs its own checkbox, and the access mode on top
//
// The statements are TWO corpora, and it takes both.
//
// coverage_test.go is derived from the node types each analyzer names, which
// covers every position a decision is made in. It is not enough on its own, and
// that was measured rather than supposed: with the CTE defect put back, the
// shape corpus under all 54 configurations reported ZERO leaks, because "a CTE
// named after a table" is not a different node type. It is the same shape with a
// different NAME, and a corpus built from the grammar cannot see it.
//
// So the known attacks go in beside them. Every route that was ever found here
// is run under every configuration, for the rest of this product's life.

type matrixEngine struct {
	name   string
	shapes map[string][]string
	// The two secrets are kept back by different halves of the policy, and which
	// half is in force decides which of them is a secret at all. A value in a
	// table nobody restricted is not leaking, it is being read.
	tableSecret string // lives in the table the table list governs
	fieldSecret string // lives in the field the field list governs
	// The same database said two ways: what is kept back, and what is permitted.
	denyTables, allowTables string
	denyFields, allowFields string
	ddl                     map[string][]string // capability key -> statements it governs
	settings                func(*testing.T) Settings
}

func matrixEngines() []matrixEngine {
	return []matrixEngine{
		{
			name: "postgres", shapes: pgShapes,
			tableSecret: "the-key", fieldSecret: "123-45-6789",
			denyTables: "secret_keys", allowTables: "customers\norders\nscratch",
			denyFields: "customers.ssn",
			allowFields: "customers.id\ncustomers.email\ncustomers.profile\norders.id\n" +
				"orders.customer_id\norders.total\norders.note\nscratch.id\nscratch.note",
			ddl: map[string][]string{
				datasource.CapCreateRoutines: {
					"CREATE FUNCTION m_f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
					"CREATE OR REPLACE FUNCTION m_f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
					"DROP FUNCTION IF EXISTS m_f()",
				},
				datasource.CapCreateTriggers: {
					"CREATE TRIGGER m_t AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION m_f()",
					"DROP TRIGGER IF EXISTS m_t ON orders",
				},
			},
			settings: func(t *testing.T) Settings {
				return Settings{Connection: pgFixture(t), Access: AccessBoth}
			},
		},
		{
			name: "mysql", shapes: mysqlShapes,
			tableSecret: "the-key", fieldSecret: "123-45-6789",
			denyTables: "secret_keys", allowTables: "customers\norders\nscratch",
			denyFields: "customers.ssn",
			allowFields: "customers.id\ncustomers.email\ncustomers.profile\norders.id\n" +
				"orders.customer_id\norders.total\norders.note\nscratch.id\nscratch.note",
			ddl: map[string][]string{
				datasource.CapCreateRoutines: {
					"CREATE PROCEDURE m_p() SELECT 1",
					"DROP PROCEDURE IF EXISTS m_p",
					"CREATE FUNCTION m_f() RETURNS int DETERMINISTIC RETURN 1",
				},
				datasource.CapCreateTriggers: {
					"CREATE TRIGGER m_t BEFORE INSERT ON orders FOR EACH ROW SET @x = 1",
					"DROP TRIGGER IF EXISTS m_t",
				},
			},
			settings: func(t *testing.T) Settings {
				s := governed(t)
				s.Access = AccessBoth
				return s
			},
		},
		{
			name: "sqlserver", shapes: msShapes,
			tableSecret: "1000", fieldSecret: msRealSSN,
			denyTables: "payroll", allowTables: "customers\norders\nscratch",
			denyFields: "customers.ssn",
			allowFields: "customers.id\ncustomers.email\ncustomers.profile\norders.id\n" +
				"orders.customer_id\norders.total\norders.note\nscratch.id\nscratch.note",
			ddl: map[string][]string{
				datasource.CapCreateRoutines: {
					"CREATE PROCEDURE dbo.m_p AS BEGIN SELECT 1 END",
					"CREATE PROC dbo.m_p2 AS BEGIN SELECT 1 END",
					"CREATE OR ALTER PROCEDURE dbo.m_p AS BEGIN SELECT 1 END",
				},
				datasource.CapCreateTriggers: {
					"CREATE TRIGGER m_t ON orders AFTER INSERT AS BEGIN SET NOCOUNT ON END",
					"DROP TRIGGER m_t",
				},
			},
			settings: func(t *testing.T) Settings {
				return Settings{Connection: msFixture(t), Access: AccessBoth}
			},
		},
	}
}

// listMode is a list as an administrator may leave it: read one way, the other
// way, or not written at all.
type listMode struct {
	label string
	mode  sqlguard.Mode
	value func(e matrixEngine, field bool) string
}

func listModes() []listMode {
	return []listMode{
		{"deny", sqlguard.ModeDenylist, func(e matrixEngine, field bool) string {
			if field {
				return e.denyFields
			}
			return e.denyTables
		}},
		{"allow", sqlguard.ModeAllowlist, func(e matrixEngine, field bool) string {
			if field {
				return e.allowFields
			}
			return e.allowTables
		}},
		{"unset", "", func(matrixEngine, bool) string { return "" }},
	}
}

// knownRoutes are the statements that actually got past this guard at some
// point, per engine. A shape corpus covers the positions; this covers the
// mistakes. Anything found from here goes in here too.
func knownRoutes(engine string) []string {
	shared := []string{
		"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys",
		"WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers",
		"WITH x AS (SELECT * FROM secret_keys), secret_keys AS (SELECT 1 AS id) SELECT * FROM x",
		"WITH x AS (SELECT ssn FROM customers), customers AS (SELECT 'd' AS ssn) SELECT * FROM x",
		"SELECT * FROM (SELECT * FROM (WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys) a) b",
		"WITH customer_view AS (SELECT * FROM customer_view) SELECT code FROM customer_view",
	}
	switch engine {
	case "postgres":
		return append(shared,
			"SELECT ROW(c.*) FROM customers c",
			"SELECT ROW(c.*)::text FROM customers c",
			"SELECT query_to_xml('SELECT ssn FROM customers', true, true, '')",
			"SELECT query_to_xml('SELECT value FROM secret_keys', true, true, '')",
			"SELECT table_to_xml('secret_keys', true, true, '')",
			"SELECT table_to_xml('customers', true, true, '')",
			"SELECT database_to_xml(true, true, '')",
			"SELECT schema_to_xml('public', true, true, '')",
			"SELECT (c).ssn FROM customers c",
			"SELECT row_to_json(c) FROM customers c",
		)
	case "mysql":
		return append(shared,
			"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT value FROM secret_keys",
			"UPDATE scratch SET note = (WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers LIMIT 1)",
		)
	default: // sqlserver: no CTE shadowing (the engine reads it as recursive)
		return []string{
			"WITH payroll AS (SELECT * FROM payroll) SELECT * FROM payroll",
			"SELECT ssn FROM customers FOR XML PATH('r')",
			"SELECT ssn FROM customers FOR JSON AUTO",
			"UPDATE customers SET email = 'x' OUTPUT deleted.ssn",
			"SELECT 1 AS [it's] ; SELECT ssn FROM customers",
			"EXEC('SELECT ssn FROM customers')",
		}
	}
}

func TestTheSecretHoldsUnderEveryPolicy(t *testing.T) {
	accesses := []Access{AccessRead, AccessWrite, AccessBoth}
	for _, e := range matrixEngines() {
		t.Run(e.name, func(t *testing.T) {
			base := e.settings(t)
			runs, statements, leaked := 0, 0, 0
			for _, tables := range listModes() {
				for _, fields := range listModes() {
					for _, routines := range []bool{false, true} {
						for _, access := range accesses {
							s := base
							s.Access = access
							s.Policy = sqlguard.Policy{
								TableMode: tables.mode, Tables: tables.value(e, false),
								FieldMode: fields.mode, Fields: fields.value(e, true),
							}
							s.Allowed = map[string]bool{}
							if routines {
								s.Allowed[datasource.CapCallRoutines] = true
							}
							runs++
							corpus := map[string][]string{"known routes": knownRoutes(e.name)}
							for shape, sqls := range e.shapes {
								corpus[shape] = sqls
							}
							for shape, sqls := range corpus {
								for _, sql := range sqls {
									statements++
									res, reason, err := run(context.Background(), s, sql, nil, 0)
									if reason != "" || err != nil || res == nil {
										continue
									}
									out := fmt.Sprint(res.Rows)
									// Each secret is only a secret while the half of the
									// policy that governs it is written. An unset list
									// keeps nothing back, and reading what nobody
									// restricted is the tool working, not leaking.
									for _, c := range []struct {
										secret  string
										inForce bool
										which   string
									}{
										{e.tableSecret, tables.mode != "", "table"},
										{e.fieldSecret, fields.mode != "", "field"},
									} {
										if !c.inForce || !strings.Contains(out, c.secret) {
											continue
										}
										leaked++
										t.Errorf("LEAK (%s) tables=%s fields=%s routines=%v access=%s  [%s]\n  %s\n  -> %.160s",
											c.which, tables.label, fields.label, routines, access, shape, sql, out)
									}
								}
							}
						}
					}
				}
			}
			t.Logf("  %-9s %d configurations, %d statements, %d leaked", e.name, runs, statements, leaked)
		})
	}
}

// Making stored code needs its own checkbox, and the access mode on top of it.
// Neither box may stand in for the other.
func TestMakingStoredCodeNeedsItsBoxUnderEveryAccess(t *testing.T) {
	for _, e := range matrixEngines() {
		t.Run(e.name, func(t *testing.T) {
			driver := e.name
			checked := 0
			for capability, statements := range e.ddl {
				other := datasource.CapCreateTriggers
				if capability == datasource.CapCreateTriggers {
					other = datasource.CapCreateRoutines
				}
				for _, sql := range statements {
					for _, access := range []Access{AccessRead, AccessWrite, AccessBoth} {
						checked++
						// Nothing ticked: refused in every access mode.
						off := settingsFor(t, driver)
						off.Access = access
						if ok, _ := permitted(off, sql); ok {
							t.Errorf("%s/%s: ran with nothing ticked: %s", driver, access, sql)
						}
						// The OTHER box ticked: still refused.
						wrong := settingsFor(t, driver, other)
						wrong.Access = access
						if ok, _ := permitted(wrong, sql); ok {
							t.Errorf("%s/%s: %s permitted a %s statement: %s", driver, access, other, capability, sql)
						}
						// Its own box ticked: allowed where the access mode allows a
						// write, refused where it does not.
						on := settingsFor(t, driver, capability)
						on.Access = access
						ok, reason := permitted(on, sql)
						switch access {
						case AccessRead:
							if ok {
								t.Errorf("%s: a read-only tool made stored code: %s", driver, sql)
							} else if !strings.Contains(reason, "read-only") {
								t.Errorf("%s: refused, but not for being read-only: %s", driver, reason)
							}
						default:
							if !ok {
								t.Errorf("%s/%s: refused with %s ticked: %s -> %s", driver, access, capability, sql, reason)
							}
						}
					}
				}
			}
			t.Logf("  %-9s %d capability checks", e.name, checked)
		})
	}
}
