package postgres

import "testing"

// A CTE named after a table does not make the table disappear. Same rule and
// same measurement as the MySQL sibling: before this, the denied row came back
// from a live server.

func TestACTECannotHideATable(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys",
		"WITH x AS (SELECT * FROM secret_keys), secret_keys AS (SELECT 1 AS id) SELECT * FROM x",
		"SELECT * FROM (WITH secret_keys AS (SELECT * FROM secret_keys) SELECT * FROM secret_keys) a",
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
}

func TestOrdinaryCTEsStillWork(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
		"WITH a AS (SELECT id FROM orders), b AS (SELECT id FROM a) SELECT * FROM b",
		"WITH orders AS (SELECT id FROM orders) SELECT * FROM orders",
		"WITH RECURSIVE n AS (SELECT 1 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 5) SELECT * FROM n",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %s", sql, reason)
		}
	}
}
