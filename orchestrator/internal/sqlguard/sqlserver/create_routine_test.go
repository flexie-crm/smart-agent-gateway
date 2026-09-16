package sqlserver

import (
	"strings"
	"testing"
)

// Making a routine is decided by what its body would read, exactly as calling
// one is.
func TestARoutineMayBeCreatedIfItsBodyKeepsThePolicy(t *testing.T) {
	g := standard(t) // denies payroll, hides customers.ssn

	for _, sql := range []string{
		"CREATE PROCEDURE dbo.p_ok AS BEGIN SELECT id, total FROM orders; END",
		"CREATE PROCEDURE dbo.p_two AS BEGIN SELECT id FROM orders; SELECT id FROM scratch; END",
		// A hidden field may be USED, exactly as in a query.
		"CREATE PROCEDURE dbo.p_uses AS BEGIN SELECT id FROM customers WHERE ssn IS NOT NULL; END",
		"CREATE PROCEDURE dbo.p_star AS BEGIN SELECT * FROM orders; END",
		"CREATE TRIGGER t ON orders AFTER INSERT AS BEGIN SELECT id FROM orders; END",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s\n  %v %s", sql, err, reason)
		}
	}

	for _, c := range []struct{ sql, mustSay string }{
		{"CREATE PROCEDURE dbo.p_bad AS BEGIN SELECT amount FROM payroll; END", "payroll"},
		{"CREATE TRIGGER t2 ON orders AFTER INSERT AS BEGIN SELECT amount FROM payroll; END", "payroll"},
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

// A hidden field in a body is REPLACED, exactly as in a query.
func TestAHiddenFieldInABodyIsReplaced(t *testing.T) {
	g := standard(t)
	for _, c := range []struct{ sql, mustContain string }{
		{"CREATE PROCEDURE dbo.p AS BEGIN SELECT ssn FROM customers; END", "'[hidden]' AS [ssn]"},
		{"CREATE PROCEDURE dbo.p AS BEGIN SELECT * FROM customers; END", "'[hidden]' AS [ssn]"},
		{"CREATE PROCEDURE dbo.p AS BEGIN SELECT id FROM customers WHERE ssn IS NOT NULL; END", "ssn IS NOT NULL"},
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
	d, _, _ := g.Check("CREATE PROCEDURE dbo.p AS BEGIN SELECT id FROM orders; END")
	if d.Rewritten {
		t.Errorf("a clean body was rewritten: %s", d.SQL)
	}
}
