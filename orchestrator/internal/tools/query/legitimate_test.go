package query

import (
	"context"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// The mirror of coverage_test.go, and the half that was missing.
//
// That file puts the denied table and the hidden field into every shape the
// analyzers name and asserts nothing escapes. This one puts ORDINARY data into
// the same shapes and asserts nothing is wrongly stopped. A guard that refuses
// everything satisfies the first file perfectly and is worthless, so neither
// number means anything without the other.
//
// Derived from the same list of node types rather than invented, for the same
// reason the other one is: a corpus somebody makes up is shaped by what they
// already believe the code does. Every statement here touches only what the
// policy permits, under every policy where it is legitimate, so every refusal is
// a defect.

var legitReads = map[string][]string{
	"common": {
		"SELECT id, email FROM customers",
		"SELECT * FROM orders",
		"SELECT COUNT(*) FROM orders",
		"SELECT id FROM orders WHERE total > 5",
		"SELECT id FROM orders WHERE note IS NULL",
		"SELECT id FROM orders WHERE note IS NOT NULL",
		"SELECT id FROM orders WHERE id IN (1, 2, 3)",
		"SELECT id FROM orders WHERE total BETWEEN 1 AND 100",
		"SELECT id FROM orders WHERE note LIKE '%a%'",
		"SELECT id FROM orders WHERE NOT (total < 0)",
		"SELECT id FROM orders WHERE total > 0 AND note IS NOT NULL",
		"SELECT id FROM orders ORDER BY total DESC",
		"SELECT id FROM orders ORDER BY 1",
		"SELECT customer_id, SUM(total) FROM orders GROUP BY customer_id",
		"SELECT customer_id, COUNT(*) FROM orders GROUP BY customer_id HAVING COUNT(*) > 0",
		"SELECT DISTINCT customer_id FROM orders",
		"SELECT MIN(total), MAX(total), AVG(total), SUM(total) FROM orders",
		"SELECT c.id, o.total FROM customers c JOIN orders o ON o.customer_id = c.id",
		"SELECT c.id FROM customers c LEFT JOIN orders o ON o.customer_id = c.id",
		"SELECT c.id FROM customers c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id)",
		"SELECT id FROM orders WHERE customer_id IN (SELECT id FROM customers)",
		"SELECT (SELECT COUNT(*) FROM orders) AS n",
		"SELECT x.id FROM (SELECT id FROM orders) x",
		"WITH recent AS (SELECT id, total FROM orders) SELECT * FROM recent",
		"WITH a AS (SELECT id FROM orders), b AS (SELECT id FROM a) SELECT * FROM b",
		"SELECT id, CASE WHEN total > 5 THEN 'big' ELSE 'small' END FROM orders",
		"SELECT COALESCE(note, 'none') FROM orders",
		"SELECT UPPER(email) FROM customers",
		"SELECT id FROM orders UNION SELECT id FROM orders",
		"SELECT id FROM orders UNION ALL SELECT id FROM orders",
		"SELECT COUNT(*) OVER () AS n, id FROM orders",
		"SELECT id, ROW_NUMBER() OVER (ORDER BY id) AS rn FROM orders",
		"SELECT id, SUM(total) OVER (PARTITION BY customer_id) FROM orders",
		"SELECT id FROM orders o WHERE o.total = (SELECT MAX(total) FROM orders)",
		"SELECT id, profile FROM customers",
		// A hidden field may be USED wherever it is not returned.
		"SELECT id FROM customers WHERE ssn IS NOT NULL",
		"SELECT COUNT(ssn) FROM customers",
		"SELECT id FROM customers ORDER BY ssn",
		"SELECT id FROM customers GROUP BY id, ssn",
	},
	"mysql": {
		"SELECT id FROM orders LIMIT 5",
		"SELECT id FROM orders LIMIT 5 OFFSET 2",
		"SELECT CONCAT(email, '!') FROM customers",
		"SELECT GROUP_CONCAT(id) FROM orders",
		"SELECT IFNULL(note, 'n') FROM orders",
		"SELECT IF(total > 0, 'y', 'n') FROM orders",
		"SELECT JSON_OBJECT('id', id) FROM orders",
		"SELECT DATE_FORMAT(NOW(), '%Y-%m-%d')",
		"SHOW TABLES",
		"DESCRIBE orders",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()",
	},
	"postgres": {
		"SELECT id FROM orders LIMIT 5",
		"SELECT email || '!' FROM customers",
		"SELECT string_agg(id::text, ',') FROM orders",
		"SELECT id::text FROM orders",
		"SELECT CAST(id AS text) FROM orders",
		"SELECT id FROM orders ORDER BY id FETCH FIRST 3 ROWS ONLY",
		"SELECT DISTINCT ON (customer_id) id FROM orders ORDER BY customer_id, id",
		"SELECT count(*) FILTER (WHERE total > 1) FROM orders",
		"SELECT ARRAY[id] FROM orders",
		"SELECT ARRAY(SELECT id FROM orders)",
		"SELECT ROW(o.id, o.total) FROM orders o",
		"SELECT GREATEST(id, customer_id), LEAST(id, customer_id) FROM orders",
		"SELECT id IS NULL FROM orders",
		"SELECT id FROM orders GROUP BY GROUPING SETS ((id))",
		"SELECT xmlelement(name x, id) FROM orders",
		"SELECT row_to_json(o) FROM orders o",
		"SELECT o FROM orders o",
	},
	"sqlserver": {
		"SELECT TOP 5 id FROM orders",
		"SELECT id FROM orders ORDER BY id OFFSET 2 ROWS FETCH NEXT 3 ROWS ONLY",
		"SELECT CONCAT(email, '!') FROM customers",
		"SELECT STRING_AGG(CAST(id AS varchar(10)), ',') FROM orders",
		"SELECT ISNULL(note, 'n') FROM orders",
		"SELECT CONVERT(varchar(10), id) FROM orders",
		"SELECT id FROM orders o CROSS APPLY (SELECT 1 AS x) a",
		"SELECT id, FIRST_VALUE(total) OVER (ORDER BY id) FROM orders",
		"SELECT id FROM orders FOR JSON AUTO",
		"SELECT table_name FROM INFORMATION_SCHEMA.TABLES",
	},
}

var legitWrites = []string{
	"INSERT INTO scratch (id, note) VALUES (900, 'x')",
	"UPDATE scratch SET note = 'y' WHERE id = 900",
	"DELETE FROM scratch WHERE id = 900",
	"INSERT INTO scratch (id, note) SELECT id, note FROM orders",
	"UPDATE scratch SET note = 'z' WHERE id IN (SELECT id FROM orders)",
}

func TestNoLegitimateQueryIsRefused(t *testing.T) {
	for _, e := range matrixEngines() {
		t.Run(e.name, func(t *testing.T) {
			base := e.settings(t)
			conn, err := datasource.Open(base.Connection)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = conn.Exec(context.Background(), "CREATE TABLE scratch (id int, note varchar(100))", nil)
			_, _ = conn.Exec(context.Background(), "CREATE TABLE scratch (id int, note text)", nil)
			_ = conn.Close()

			reads := append(append([]string{}, legitReads["common"]...), legitReads[e.name]...)
			checked, refused := 0, 0
			for _, tables := range listModes() {
				for _, fields := range listModes() {
					policy := sqlguard.Policy{
						TableMode: tables.mode, Tables: tables.value(e, false),
						FieldMode: fields.mode, Fields: fields.value(e, true),
					}
					for _, group := range []struct {
						sqls  []string
						modes []Access
					}{
						{reads, []Access{AccessRead, AccessBoth}},
						{legitWrites, []Access{AccessWrite, AccessBoth}},
					} {
						for _, access := range group.modes {
							s := base
							s.Access = access
							s.Policy = policy
							s.Allowed = map[string]bool{}
							for _, sql := range group.sqls {
								checked++
								_, reason, _ := run(context.Background(), s, sql, nil, 0)
								if reason != "" {
									refused++
									t.Errorf("FALSE REFUSAL  tables=%s fields=%s access=%s\n  %s\n  %s",
										tables.label, fields.label, access, sql, reason)
								}
							}
						}
					}
				}
			}
			t.Logf("  %-9s %d checks, %d FALSELY REFUSED", e.name, checked, refused)
		})
	}
}
