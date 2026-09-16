package postgres

import (
	"strings"
	"testing"
)

// DISTINCT ON names the expressions that decide which row survives. They are
// READ, not returned, so a hidden field may appear there exactly as it may in an
// ORDER BY. What must not happen is the value coming back.
//
// Before the descent walked this clause, the sweep saw a column nobody had
// accounted for and refused the query outright, which is a false refusal on
// ordinary PostgreSQL.
func TestDistinctOnIsReadNotReturned(t *testing.T) {
	g := standard(t) // denies secret_keys, hides customers.ssn

	// Ordinary use, on nothing hidden: runs.
	for _, sql := range []string{
		"SELECT DISTINCT ON (customer_id) id FROM orders ORDER BY customer_id, id",
		"SELECT DISTINCT ON (o.customer_id) o.id FROM orders o ORDER BY o.customer_id",
		"SELECT DISTINCT id FROM orders",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}

	// A hidden field may DECIDE which row survives, and must not come back.
	d, reason, err := g.Check("SELECT DISTINCT ON (ssn) id, email FROM customers ORDER BY ssn")
	if err != nil || reason != "" {
		t.Fatalf("a hidden field used to pick a row was refused: %v %s", err, reason)
	}
	if strings.Contains(d.SQL, "'[hidden]'") {
		t.Errorf("the stand-in was put where the value is only USED: %s", d.SQL)
	}

	// And when it IS returned, it is replaced.
	d, reason, err = g.Check("SELECT DISTINCT ON (id) ssn FROM customers")
	if err != nil || reason != "" {
		t.Fatalf("refused: %v %s", err, reason)
	}
	if !d.Rewritten || !strings.Contains(d.SQL, "'[hidden]'") {
		t.Errorf("a hidden field came back through DISTINCT ON: %s", d.SQL)
	}

	// A denied table is still denied, however the row is picked.
	if _, reason, _ := g.Check("SELECT DISTINCT ON (id) value FROM secret_keys"); reason == "" {
		t.Error("a denied table was read through DISTINCT ON")
	}
}
