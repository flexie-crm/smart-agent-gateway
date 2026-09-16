package postgres

import "testing"

// A star inside an expression hands the whole row over as one value, so there is
// no select item left to put the stand-in in.
func TestAStarInsideAnExpressionCannotCarryAHiddenField(t *testing.T) {
	g := standard(t) // hides customers.ssn
	for _, sql := range []string{
		"SELECT ROW(c.*) FROM customers c",
		"SELECT ROW(c.*)::text FROM customers c",
		"UPDATE scratch SET note = concat(c.*) FROM customers c WHERE c.id = scratch.id",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason == "" {
			t.Errorf("ALLOWED, and should not be: %s", sql)
		}
	}
}

// And the other direction, twice over: a star inside an expression over a table
// with nothing hidden is fine, and a plain star as a select item is still
// EXPANDED rather than refused.
func TestOrdinaryStarsAreUntouched(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT ROW(o.*) FROM orders o",
		"SELECT * FROM orders",
		"SELECT o.* FROM orders o",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %s", sql, reason)
		}
	}
	// The select-item star over a table WITH a hidden column still expands.
	d, reason, err := g.Check("SELECT * FROM customers")
	if err != nil || reason != "" {
		t.Fatalf("refused: %v %s", err, reason)
	}
	if !d.Rewritten {
		t.Errorf("a plain star over a table with a hidden column was not expanded: %s", d.SQL)
	}
}

// The functions that are handed a query or a name as TEXT.
func TestAStatementWrittenAsTextIsRefused(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT query_to_xml('SELECT ssn FROM customers', true, true, '')",
		"SELECT query_to_xml('SELECT value FROM secret_keys', true, true, '')",
		"SELECT table_to_xml('secret_keys', true, true, '')",
		"SELECT pg_catalog.table_to_xml('customers', true, true, '')",
		"SELECT database_to_xml(true, true, '')",
		"SELECT schema_to_xml('public', true, true, '')",
		"SELECT id FROM orders WHERE note = query_to_xml('SELECT 1', true, true, '')::text",
	} {
		if _, reason, err := g.Check(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if reason == "" {
			t.Errorf("ALLOWED, and should not be: %s", sql)
		}
	}
	// Ordinary functions are untouched.
	for _, sql := range []string{
		"SELECT lower(email) FROM customers",
		"SELECT count(*) FROM orders",
		"SELECT to_char(total, '999') FROM orders",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}
}
