package postgres

import (
	"strings"
	"testing"
)

// Making a routine is decided by what its body would read, exactly as calling
// one is.
func TestARoutineMayBeCreatedIfItsBodyKeepsThePolicy(t *testing.T) {
	g := standard(t) // denies secret_keys, hides customers.ssn

	for _, sql := range []string{
		"CREATE FUNCTION f_ok() RETURNS int LANGUAGE sql AS $$ SELECT id FROM orders $$",
		"CREATE FUNCTION f_join() RETURNS int LANGUAGE sql AS $$ SELECT o.id FROM orders o JOIN customers c ON c.id = o.customer_id $$",
		// A hidden field may be USED, exactly as in a query.
		"CREATE FUNCTION f_uses() RETURNS int LANGUAGE sql AS $$ SELECT id FROM customers WHERE ssn IS NOT NULL $$",
		"CREATE FUNCTION f_star() RETURNS SETOF orders LANGUAGE sql AS $$ SELECT * FROM orders $$",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s\n  %v %s", sql, err, reason)
		}
	}

	for _, c := range []struct{ sql, mustSay string }{
		{"CREATE FUNCTION f_bad() RETURNS int LANGUAGE sql AS $$ SELECT value FROM secret_keys $$", "secret_keys"},
		// A language this cannot read is refused rather than assumed harmless.
		{"CREATE FUNCTION f_pl() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$", "plain SQL"},
	} {
		_, reason, err := g.Check(c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if reason == "" {
			t.Errorf("CREATED, and should not have been: %s", c.sql)
			continue
		}
		if !strings.Contains(reason, c.mustSay) {
			t.Errorf("refused without saying why.\n  %s\n  said: %s\n  wanted: %s", c.sql, reason, c.mustSay)
		}
	}
}

// A hidden field in a body is REPLACED, exactly as in a query. The policy says
// what the field is worth to anyone reading through this tool, and a routine
// made through this tool is no different from a query run through it.
func TestAHiddenFieldInABodyIsReplaced(t *testing.T) {
	g := standard(t)
	for _, c := range []struct{ sql, mustContain string }{
		{"CREATE FUNCTION f() RETURNS text LANGUAGE sql AS $$ SELECT ssn FROM customers $$", "'[hidden]' AS ssn"},
		{"CREATE FUNCTION f() RETURNS SETOF customers LANGUAGE sql AS $$ SELECT * FROM customers $$", "'[hidden]' AS ssn"},
		// Filtering on it is untouched: only the value that comes BACK is replaced.
		{"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT id FROM customers WHERE ssn IS NOT NULL $$", "ssn IS NOT NULL"},
	} {
		d, reason, err := g.Check(c.sql)
		if err != nil || reason != "" {
			t.Errorf("REFUSED, and should be rewritten: %s -> %v %s", c.sql, err, reason)
			continue
		}
		if !strings.Contains(d.SQL, c.mustContain) {
			t.Errorf("%s\n  got:  %s\n  want it to contain: %s", c.sql, d.SQL, c.mustContain)
		}
	}
	// A clean body is left exactly as written.
	d, _, _ := g.Check("CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT id FROM orders $$")
	if d.Rewritten {
		t.Errorf("a clean body was rewritten: %s", d.SQL)
	}
}
