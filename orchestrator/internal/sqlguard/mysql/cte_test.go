package mysql

import (
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// A CTE named after a table does not make the table disappear.
//
// SQL's own rule, which this package had backwards: a CTE body does not see its
// own name, and does not see one written after it, so the database resolves
// either of those to the TABLE of that name. Measured against a live server
// before the fix: the denied table's row came back.

func TestACTECannotHideATable(t *testing.T) {
	g := standard(t) // denies secret_keys, hides customers.ssn
	for _, sql := range []string{
		// Its own name, inside its own body.
		"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys",
		// A name written LATER in the same WITH is not in scope earlier.
		"WITH x AS (SELECT * FROM secret_keys), secret_keys AS (SELECT 1 AS id, 'd' AS value) SELECT * FROM x",
		// Nested, so the shadowing sits under two more levels.
		"SELECT * FROM (SELECT * FROM (WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys) a) b",
		// The same shape reaching a view that reads the denied table.
		"WITH leaky_view AS (SELECT * FROM leaky_view) SELECT * FROM leaky_view",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason == "" {
			t.Errorf("ALLOWED, and should not be: %s", sql)
		}
	}
}

func TestACTECannotUnhideAField(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers",
		"WITH x AS (SELECT ssn FROM customers), customers AS (SELECT 'd' AS ssn) SELECT * FROM x",
		"WITH customer_view AS (SELECT * FROM customer_view) SELECT code FROM customer_view",
	} {
		d, reason, err := g.Check(sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if reason == "" && !d.Rewritten {
			t.Errorf("ALLOWED unchanged, so the value comes back: %s", sql)
		}
	}
	// And the copy-it-somewhere-readable route, which has its own rule.
	if _, reason, _ := g.Check(
		"UPDATE scratch SET note = (WITH customers AS (SELECT * FROM customers) SELECT ssn FROM customers LIMIT 1)"); reason == "" {
		t.Error("a hidden field was copied into a readable column through a CTE")
	}
}

// The other direction, which is what stops the fix being "refuse every WITH".
func TestOrdinaryCTEsStillWork(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
		"WITH a AS (SELECT id FROM orders), b AS (SELECT id FROM a) SELECT * FROM b",
		"WITH orders AS (SELECT id FROM orders) SELECT * FROM orders",
		"WITH RECURSIVE n AS (SELECT 1 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 5) SELECT * FROM n",
		"SELECT id FROM (WITH inner_q AS (SELECT id FROM orders) SELECT * FROM inner_q) z",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %s", sql, reason)
		}
	}
}

// A CTE that shadows a table nobody keeps back is still an ordinary CTE, and a
// reference to it from AFTER its definition is the CTE, not the table.
func TestAShadowingCTEIsStillACTEWhereItIsInScope(t *testing.T) {
	g := guarded(t, sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys"})
	// orders is not denied, so this runs either way; what it proves is that the
	// outer reference resolves to the CTE and the body reads the real table.
	if _, reason, err := g.Check("WITH orders AS (SELECT id FROM orders) SELECT id FROM orders"); err != nil || reason != "" {
		t.Errorf("refused: %v %s", err, reason)
	}
}
