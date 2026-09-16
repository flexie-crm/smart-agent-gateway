package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// The other half of the bargain, and for a long while the only half nobody was
// measuring.
//
// A guard that refuses everything leaks nothing, and is worthless. Every
// statement below is one a person or an agent would legitimately write, touching
// only what the policy permits, so every refusal here is a FALSE one and a
// defect. It is the mirror of TestTheSecretHoldsUnderEveryPolicy: that one
// counts what escapes, this one counts what is wrongly stopped, and neither
// number means anything without the other.
//
// It earned its place on the first run by finding DISTINCT ON, ordinary
// PostgreSQL that this refused outright because the descent never walked the
// distinct clause and the sweep then saw a column nobody had accounted for.
var legit = map[string][]string{
	"common": {
		"SELECT id, email FROM customers",
		"SELECT * FROM orders",
		"SELECT COUNT(*) FROM orders",
		"SELECT COUNT(*) AS n FROM customers",
		"SELECT id, total FROM orders WHERE total > 5",
		"SELECT id FROM orders WHERE total BETWEEN 1 AND 100",
		"SELECT id FROM orders WHERE note IS NOT NULL",
		"SELECT id FROM orders WHERE note LIKE '%a%'",
		"SELECT id FROM orders WHERE id IN (1,2,3)",
		"SELECT id FROM orders ORDER BY total DESC",
		"SELECT customer_id, SUM(total) FROM orders GROUP BY customer_id",
		"SELECT customer_id, SUM(total) AS s FROM orders GROUP BY customer_id HAVING SUM(total) > 0",
		"SELECT DISTINCT customer_id FROM orders",
		"SELECT c.id, c.email, o.total FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT c.id FROM customers c LEFT JOIN orders o ON o.customer_id = c.id",
		"SELECT c.id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id)",
		"SELECT id FROM orders WHERE customer_id IN (SELECT id FROM customers)",
		"SELECT (SELECT COUNT(*) FROM orders) AS n",
		"SELECT x.id FROM (SELECT id FROM orders) x",
		"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
		"WITH a AS (SELECT id FROM orders), b AS (SELECT id FROM a) SELECT * FROM b",
		"SELECT id, CASE WHEN total > 5 THEN 'big' ELSE 'small' END AS size FROM orders",
		"SELECT UPPER(email) FROM customers",
		"SELECT LOWER(email) AS e FROM customers",
		"SELECT id FROM orders UNION SELECT id FROM orders",
		"SELECT id FROM orders UNION ALL SELECT id FROM orders",
		"SELECT COUNT(*) OVER () AS n, id FROM orders",
		"SELECT id, ROW_NUMBER() OVER (ORDER BY id) AS rn FROM orders",
		"SELECT id, SUM(total) OVER (PARTITION BY customer_id) FROM orders",
		"SELECT MIN(total), MAX(total), AVG(total) FROM orders",
		"SELECT c.email FROM customers c ORDER BY c.email",
		"SELECT id FROM orders o WHERE o.total = (SELECT MAX(total) FROM orders)",
		"SELECT id, email FROM customers WHERE email IS NOT NULL ORDER BY id",
		"SELECT COALESCE(note, 'none') FROM orders",
		"SELECT id FROM orders WHERE NOT (total < 0)",
		"SELECT id FROM orders WHERE total > 0 AND note IS NOT NULL OR id = 1",
		"INSERT INTO scratch (id, note) VALUES (900, 'x')",
		"UPDATE scratch SET note = 'y' WHERE id = 900",
		"DELETE FROM scratch WHERE id = 900",
		"INSERT INTO scratch (id, note) SELECT id, note FROM orders",
	},
	"mysql": {
		"SELECT id FROM orders LIMIT 5",
		"SELECT id FROM orders LIMIT 5 OFFSET 2",
		"SELECT CONCAT(email, '!') FROM customers",
		"SELECT DATE_FORMAT(NOW(), '%Y-%m-%d')",
		"SELECT GROUP_CONCAT(id) FROM orders",
		"SELECT IFNULL(note, 'n') FROM orders",
		"SHOW TABLES",
		"DESCRIBE orders",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()",
	},
	"postgres": {
		"SELECT id FROM orders LIMIT 5",
		"SELECT id FROM orders LIMIT 5 OFFSET 2",
		"SELECT email || '!' FROM customers",
		"SELECT to_char(now(), 'YYYY-MM-DD')",
		"SELECT string_agg(id::text, ',') FROM orders",
		"SELECT COALESCE(note, 'n') FROM orders",
		"SELECT id FROM orders ORDER BY id FETCH FIRST 3 ROWS ONLY",
		"SELECT DISTINCT ON (customer_id) id FROM orders ORDER BY customer_id, id",
		"SELECT count(*) FILTER (WHERE total > 1) FROM orders",
	},
	"sqlserver": {
		"SELECT TOP 5 id FROM orders",
		"SELECT id FROM orders ORDER BY id OFFSET 2 ROWS FETCH NEXT 3 ROWS ONLY",
		"SELECT CONCAT(email, '!') FROM customers",
		"SELECT FORMAT(GETDATE(), 'yyyy-MM-dd')",
		"SELECT STRING_AGG(CAST(id AS varchar(10)), ',') FROM orders",
		"SELECT ISNULL(note, 'n') FROM orders",
		"SELECT id FROM orders o CROSS APPLY (SELECT 1 AS x) a",
		"SELECT table_name FROM INFORMATION_SCHEMA.TABLES",
	},
}

func TestOrdinaryQueriesAreNotRefused(t *testing.T) {
	for _, e := range []struct {
		name   string
		set    func(*testing.T) Settings
		policy sqlguard.Policy
	}{
		{"mysql", func(t *testing.T) Settings { s := governed(t); s.Access = AccessBoth; return s },
			sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
				FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"}},
		{"postgres", func(t *testing.T) Settings {
			return Settings{Connection: pgFixture(t), Access: AccessBoth}
		}, sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"}},
		{"sqlserver", func(t *testing.T) Settings {
			return Settings{Connection: msFixture(t), Access: AccessBoth}
		}, sqlguard.Policy{TableMode: sqlguard.ModeDenylist, Tables: "payroll",
			FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn"}},
	} {
		t.Run(e.name, func(t *testing.T) {
			s := e.set(t)
			s.Policy = e.policy
			conn, err := datasource.Open(s.Connection)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = conn.Exec(context.Background(), "CREATE TABLE scratch (id int, note varchar(100))", nil)
			_, _ = conn.Exec(context.Background(), "CREATE TABLE scratch (id int, note text)", nil)
			_ = conn.Close()

			var refused, ran, enginErr int
			for _, group := range []string{"common", e.name} {
				for _, sql := range legit[group] {
					_, reason, err := run(context.Background(), s, sql, nil, 0)
					switch {
					case reason != "":
						refused++
						t.Errorf("FALSE REFUSAL  %s\n    %s", sql, reason)
					case err != nil:
						enginErr++
						t.Logf("engine says    %-60.60s | %.60s", sql, err.Error())
					default:
						ran++
					}
				}
			}
			t.Logf("  %-9s %d ran, %d FALSELY REFUSED, %d rejected by the engine",
				e.name, ran, refused, enginErr)
			_ = fmt.Sprint
			_ = strings.TrimSpace
		})
	}
}
