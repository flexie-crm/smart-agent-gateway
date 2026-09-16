package mysql

import (
	"strings"
	"testing"
)

// Making a routine is decided by what its body would read, exactly as calling
// one is. Refusing every CREATE was safe and useless: the checkbox was a door
// with nothing behind it.
func TestARoutineMayBeCreatedIfItsBodyKeepsThePolicy(t *testing.T) {
	g := standard(t) // denies secret_keys, hides customers.ssn

	// A body that stays inside the policy is CREATED. Without this the refusals
	// below would only prove the feature does not work.
	for _, sql := range []string{
		"CREATE PROCEDURE p_ok() BEGIN SELECT id, total FROM orders; END",
		"CREATE PROCEDURE p_decl() BEGIN DECLARE n INT; SELECT COUNT(*) FROM orders; END",
		"CREATE PROCEDURE p_branch() BEGIN IF 1=1 THEN SELECT id FROM orders; ELSE SELECT id FROM scratch; END IF; END",
		"CREATE PROCEDURE p_cursor() BEGIN DECLARE c CURSOR FOR SELECT id FROM orders; OPEN c; CLOSE c; END",
		// A hidden field may be USED, exactly as in a query.
		"CREATE PROCEDURE p_uses() BEGIN SELECT id FROM customers WHERE ssn IS NOT NULL; END",
		"DROP PROCEDURE p_ok",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s\n  %v %s", sql, err, reason)
		}
	}

	// A body that breaks the policy is refused, and the message says WHY.
	for _, c := range []struct{ sql, mustSay string }{
		{"CREATE PROCEDURE p_bad() BEGIN SELECT value FROM secret_keys; END", "secret_keys"},
		{"CREATE PROCEDURE p_loop() BEGIN WHILE 1=1 DO SELECT value FROM secret_keys; END WHILE; END", "secret_keys"},
		{"CREATE PROCEDURE p_else() BEGIN IF 1=1 THEN SELECT id FROM orders; ELSE SELECT value FROM secret_keys; END IF; END", "secret_keys"},
		{"CREATE PROCEDURE p_cur() BEGIN DECLARE c CURSOR FOR SELECT value FROM secret_keys; OPEN c; END", "secret_keys"},
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
			t.Errorf("refused without saying what the problem is.\n  %s\n  said: %s\n  wanted it to name: %s",
				c.sql, reason, c.mustSay)
		}
	}
}

// A star hides what it stands for, and a body full of stars was the way past
// this: SELECT * over a table with a hidden column names no column at all, so
// reading only the list of fields-to-be-replaced let it through. The signal is
// that the statement WOULD have been rewritten, whatever form the rewriting
// would have taken.
func TestAStarInABodyIsNotAWayPast(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"CREATE PROCEDURE p() BEGIN SELECT * FROM customers; END",
		"CREATE PROCEDURE p() BEGIN SELECT c.* FROM customers c; END",
		"CREATE PROCEDURE p() BEGIN SELECT * FROM customers JOIN orders ON orders.customer_id = customers.id; END",
	} {
		d, reason, err := g.Check(sql)
		if err != nil || reason != "" {
			t.Errorf("REFUSED, and should be rewritten: %s -> %v %s", sql, err, reason)
			continue
		}
		if !d.Rewritten || !strings.Contains(d.SQL, "'[hidden]'") {
			t.Errorf("a star let the hidden field through: %s -> %s", sql, d.SQL)
		}
	}
	// A star over a table with nothing hidden in it is fine, which is what stops
	// this being "refuse every star".
	for _, sql := range []string{
		"CREATE PROCEDURE p() BEGIN SELECT * FROM orders; END",
		"CREATE PROCEDURE p() BEGIN SELECT o.* FROM orders o; END",
	} {
		if _, reason, err := g.Check(sql); err != nil || reason != "" {
			t.Errorf("REFUSED, and should not be: %s -> %v %s", sql, err, reason)
		}
	}
}
