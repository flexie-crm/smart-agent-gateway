package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Every shape the guard makes a decision about, with the secret in it.
//
// The corpus below is DERIVED rather than imagined, and that is the whole point.
// Each analyzer's hand-written walk names a finite set of shapes: the pg_query
// node types, the TiDB ast types, the T-SQL grammar contexts it reaches for by
// name. Those are the places a decision is made, and therefore the only places a
// decision can be made wrongly. So the list of statements is read off the code:
// one or more per shape, each putting the denied table or the hidden field in
// that position, run against a real server.
//
// Written after two holes were found by hand in one day, both of the same kind:
// a shape that WAS visited, WAS decided about, and decided wrongly. The sweep
// each analyzer runs cannot catch that, because it only asks whether a node was
// visited at all. This can, because it does not care how the guard decided. A
// statement that is refused, one that is rewritten, and one the engine itself
// rejects are all passes; the only failure is the secret appearing in the rows.
//
// When an analyzer learns a new shape, add a statement for it here.

var pgShapes = map[string][]string{
	"AArrayExpr":     {"SELECT ARRAY[c.ssn] FROM customers c", "SELECT ARRAY(SELECT ssn FROM customers)"},
	"AExpr":          {"SELECT ssn || '' FROM customers", "SELECT c.ssn = 'x' FROM customers c"},
	"AIndices":       {"SELECT (ARRAY[c.ssn])[1] FROM customers c"},
	"AIndirection":   {"SELECT (c).ssn FROM customers c", "SELECT (c.*).ssn FROM customers c"},
	"AStar":          {"SELECT * FROM customers", "SELECT c.* FROM customers c", "SELECT * FROM secret_keys"},
	"BoolExpr":       {"SELECT (ssn IS NOT NULL) AND true FROM customers"},
	"BooleanTest":    {"SELECT (ssn IS NOT NULL) IS TRUE FROM customers"},
	"CaseExpr":       {"SELECT CASE WHEN id = 1 THEN ssn ELSE email END FROM customers", "SELECT CASE ssn WHEN 'x' THEN 'y' ELSE ssn END FROM customers"},
	"CoalesceExpr":   {"SELECT COALESCE(ssn, email) FROM customers", "SELECT COALESCE(NULL, ssn) FROM customers"},
	"CollateClause":  {"SELECT ssn COLLATE \"C\" FROM customers"},
	"ColumnRef":      {"SELECT ssn FROM customers", "SELECT c.ssn FROM customers c"},
	"DeleteStmt":     {"DELETE FROM scratch WHERE note = (SELECT ssn FROM customers LIMIT 1)", "DELETE FROM customers RETURNING ssn"},
	"ExplainStmt":    {"EXPLAIN SELECT ssn FROM customers", "EXPLAIN SELECT * FROM secret_keys"},
	"FuncCall":       {"SELECT upper(ssn) FROM customers", "SELECT md5(ssn) FROM customers", "SELECT string_agg(ssn, ',') FROM customers"},
	"GroupingFunc":   {"SELECT GROUPING(ssn) FROM customers GROUP BY ssn"},
	"GroupingSet":    {"SELECT ssn FROM customers GROUP BY GROUPING SETS ((ssn))"},
	"InsertStmt":     {"INSERT INTO scratch SELECT id, ssn FROM customers", "INSERT INTO scratch VALUES (5, 'x') RETURNING note"},
	"JoinExpr":       {"SELECT c.ssn FROM customers c JOIN orders o ON o.customer_id = c.id", "SELECT k.value FROM orders o JOIN secret_keys k ON k.id = o.id"},
	"MinMaxExpr":     {"SELECT GREATEST(ssn, email) FROM customers", "SELECT LEAST(ssn, email) FROM customers"},
	"NamedArgExpr":   {"SELECT query_to_xml(query => 'SELECT ssn FROM customers', nulls => true, tableforest => true, targetns => '')"},
	"NullTest":       {"SELECT ssn IS NULL FROM customers"},
	"RangeFunction":  {"SELECT * FROM unnest(ARRAY(SELECT ssn FROM customers))", "SELECT * FROM json_each(row_to_json(c)) FROM customers c"},
	"RangeSubselect": {"SELECT x.ssn FROM (SELECT ssn FROM customers) x", "SELECT * FROM (SELECT * FROM secret_keys) y"},
	"RangeVar":       {"SELECT value FROM secret_keys", "SELECT * FROM public.secret_keys"},
	"RowExpr":        {"SELECT ROW(c.*) FROM customers c", "SELECT ROW(c.ssn) FROM customers c", "SELECT (ROW(c.*)).f3 FROM customers c"},
	"SortBy":         {"SELECT id FROM customers ORDER BY ssn", "SELECT ssn FROM customers ORDER BY 1"},
	"SubLink":        {"SELECT (SELECT ssn FROM customers LIMIT 1)", "SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM secret_keys)", "SELECT ARRAY(SELECT value FROM secret_keys)"},
	"TypeCast":       {"SELECT ssn::text FROM customers", "SELECT CAST(ssn AS varchar) FROM customers", "SELECT c::text FROM customers c"},
	"UpdateStmt":     {"UPDATE scratch SET note = (SELECT ssn FROM customers LIMIT 1)", "UPDATE scratch SET note = 'x' RETURNING (SELECT ssn FROM customers LIMIT 1)"},
	"WindowDef":      {"SELECT first_value(ssn) OVER () FROM customers", "SELECT count(*) OVER (PARTITION BY ssn) FROM customers"},
	"XmlExpr":        {"SELECT xmlelement(name x, ssn) FROM customers", "SELECT xmlforest(ssn) FROM customers", "SELECT xmlagg(xmlelement(name r, ssn)) FROM customers"},
	"CallStmt":       {"CALL p_none()"},
	"setops":         {"SELECT ssn FROM customers UNION ALL SELECT note FROM scratch", "SELECT value FROM secret_keys EXCEPT SELECT note FROM scratch"},
	"cte":            {"WITH x AS (SELECT ssn FROM customers) SELECT * FROM x", "WITH x AS (SELECT * FROM secret_keys) SELECT * FROM x"},
	"catalog":        {"SELECT * FROM pg_class", "SELECT prosrc FROM pg_proc", "SELECT definition FROM pg_views"},
	"text-as-code":   {"SELECT query_to_xml('SELECT ssn FROM customers', true, true, '')", "SELECT table_to_xml('secret_keys', true, true, '')"},
}

var mysqlShapes = map[string][]string{
	"AggregateFuncExpr":   {"SELECT GROUP_CONCAT(ssn) FROM customers", "SELECT MAX(ssn) FROM customers", "SELECT COUNT(DISTINCT ssn) FROM customers"},
	"Assignment":          {"UPDATE scratch SET note = (SELECT ssn FROM customers LIMIT 1)", "UPDATE customers SET ssn = 'x'"},
	"BinaryOperationExpr": {"SELECT ssn = 'x' FROM customers", "SELECT CONCAT(ssn, '') FROM customers", "SELECT ssn + 0 FROM customers"},
	"CallStmt":            {"CALL p_none()"},
	"ColumnNameExpr":      {"SELECT ssn FROM customers", "SELECT c.ssn FROM customers c", "SELECT customers.ssn FROM customers"},
	"DefaultExpr":         {"INSERT INTO scratch VALUES (9, DEFAULT)"},
	"DeleteStmt":          {"DELETE FROM scratch WHERE note = (SELECT ssn FROM customers LIMIT 1)"},
	"ExplainStmt":         {"EXPLAIN SELECT ssn FROM customers", "EXPLAIN SELECT * FROM secret_keys"},
	"FuncCallExpr":        {"SELECT UPPER(ssn) FROM customers", "SELECT MD5(ssn) FROM customers", "SELECT JSON_OBJECT('s', ssn) FROM customers", "SELECT JSON_ARRAY(ssn) FROM customers", "SELECT HEX(ssn) FROM customers", "SELECT EXPORT_SET(1,ssn,ssn) FROM customers", "SELECT ELT(1, ssn) FROM customers"},
	"InsertStmt":          {"INSERT INTO scratch SELECT id, ssn FROM customers", "INSERT INTO scratch VALUES (7, 'x') ON DUPLICATE KEY UPDATE note = (SELECT ssn FROM customers LIMIT 1)", "REPLACE INTO scratch SELECT id, ssn FROM customers"},
	"IsNullExpr":          {"SELECT ssn IS NULL FROM customers"},
	"Join":                {"SELECT c.ssn FROM customers c JOIN orders o ON o.customer_id = c.id", "SELECT k.value FROM orders o JOIN secret_keys k ON k.id = o.id", "SELECT c.ssn FROM customers c LEFT JOIN orders o ON o.id = c.id"},
	"ParenthesesExpr":     {"SELECT (ssn) FROM customers", "SELECT ((ssn)) FROM customers"},
	"PatternInExpr":       {"SELECT id FROM customers WHERE ssn IN ('a','b')", "SELECT ssn IN ('a') FROM customers"},
	"SelectField":         {"SELECT ssn AS s FROM customers", "SELECT * FROM customers", "SELECT c.* FROM customers c"},
	"SetOprStmt":          {"SELECT ssn FROM customers UNION ALL SELECT note FROM scratch", "SELECT value FROM secret_keys UNION SELECT note FROM scratch"},
	"ShowColumns":         {"SHOW COLUMNS FROM customers", "SHOW COLUMNS FROM secret_keys"},
	"ShowCreateTable":     {"SHOW CREATE TABLE customers", "SHOW CREATE TABLE secret_keys"},
	"ShowIndex":           {"SHOW INDEX FROM secret_keys"},
	"ShowTables":          {"SHOW TABLES"},
	"ShowTableStatus":     {"SHOW TABLE STATUS"},
	"TableName":           {"SELECT * FROM secret_keys", "SELECT * FROM shop.secret_keys", "SELECT * FROM `secret_keys`"},
	"TableSource":         {"SELECT x.ssn FROM (SELECT ssn FROM customers) x", "SELECT * FROM (SELECT * FROM secret_keys) y"},
	"UpdateStmt":          {"UPDATE scratch SET note = 'x' WHERE note = (SELECT ssn FROM customers LIMIT 1)"},
	"VariableExpr":        {"SELECT @x := ssn FROM customers", "SET @x = (SELECT ssn FROM customers LIMIT 1)"},
	"WithClause":          {"WITH x AS (SELECT ssn FROM customers) SELECT * FROM x", "WITH x AS (SELECT * FROM secret_keys) SELECT * FROM x"},
	"subquery":            {"SELECT (SELECT ssn FROM customers LIMIT 1)", "SELECT id FROM orders WHERE id IN (SELECT id FROM secret_keys)", "SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM secret_keys)"},
	"window":              {"SELECT FIRST_VALUE(ssn) OVER () FROM customers", "SELECT COUNT(*) OVER (PARTITION BY ssn) FROM customers"},
	"having-order":        {"SELECT id FROM customers GROUP BY id HAVING MAX(ssn) > ''", "SELECT id FROM customers ORDER BY ssn"},
	"case":                {"SELECT CASE WHEN id=1 THEN ssn ELSE email END FROM customers", "SELECT IFNULL(ssn, email) FROM customers", "SELECT IF(1, ssn, email) FROM customers"},
	"catalog":             {"SELECT table_name FROM information_schema.tables", "SELECT column_name FROM information_schema.columns WHERE table_name='secret_keys'", "SELECT routine_definition FROM information_schema.routines"},
	"view":                {"SELECT code FROM customer_view", "SELECT * FROM leaky_view"},
	"files":               {"SELECT LOAD_FILE('/etc/passwd')", "SELECT ssn INTO OUTFILE '/tmp/x' FROM customers"},
}

var msShapes = map[string][]string{
	"Asterisk":         {"SELECT * FROM customers", "SELECT c.* FROM customers c", "SELECT * FROM payroll"},
	"Full_column_name": {"SELECT ssn FROM customers", "SELECT c.ssn FROM customers c", "SELECT dbo.customers.ssn FROM customers"},
	"Full_table_name":  {"SELECT * FROM payroll", "SELECT * FROM dbo.payroll", "SELECT * FROM [payroll]"},
	"As_column_alias":  {"SELECT ssn AS s FROM customers"},
	"Subquery":         {"SELECT (SELECT TOP 1 ssn FROM customers)", "SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM payroll)", "SELECT id FROM orders WHERE id IN (SELECT id FROM payroll)"},
	"Table_source":     {"SELECT x.ssn FROM (SELECT ssn FROM customers) x", "SELECT * FROM (SELECT * FROM payroll) y"},
	"Insert_statement": {"INSERT INTO scratch SELECT id, ssn FROM customers", "INSERT INTO scratch (id, note) VALUES (5, 'x')"},
	"Update_statement": {"UPDATE scratch SET note = (SELECT TOP 1 ssn FROM customers)", "UPDATE customers SET ssn = 'x'"},
	"Update_elem":      {"UPDATE scratch SET note = c.ssn FROM customers c WHERE c.id = scratch.id"},
	"Delete_statement": {"DELETE FROM scratch WHERE note = (SELECT TOP 1 ssn FROM customers)"},
	"Output_clause":    {"INSERT INTO scratch VALUES (6, 'x') OUTPUT inserted.note", "UPDATE customers SET email = 'x' OUTPUT deleted.ssn", "DELETE FROM customers OUTPUT deleted.ssn"},
	"For_clause":       {"SELECT ssn FROM customers FOR XML PATH('r')", "SELECT ssn FROM customers FOR JSON AUTO", "SELECT * FROM customers FOR XML AUTO"},
	"CTE":              {"WITH x AS (SELECT ssn FROM customers) SELECT * FROM x", "WITH x AS (SELECT * FROM payroll) SELECT * FROM x"},
	"Execute":          {"EXEC dbo.p_none", "EXEC('SELECT ssn FROM customers')", "EXEC sp_executesql N'SELECT ssn FROM customers'"},
	"Scalar_function":  {"SELECT UPPER(ssn) FROM customers", "SELECT CONCAT(ssn, '') FROM customers", "SELECT STRING_AGG(ssn, ',') FROM customers", "SELECT dbo.f_none()"},
	"window":           {"SELECT FIRST_VALUE(ssn) OVER (ORDER BY id) FROM customers", "SELECT COUNT(*) OVER (PARTITION BY ssn) FROM customers"},
	"apply":            {"SELECT a.ssn FROM orders o CROSS APPLY (SELECT TOP 1 ssn FROM customers) a", "SELECT a.amount FROM orders o OUTER APPLY (SELECT TOP 1 amount FROM payroll) a"},
	"pivot":            {"SELECT * FROM (SELECT id, ssn FROM customers) s PIVOT (MAX(ssn) FOR id IN ([1])) p"},
	"into":             {"SELECT ssn INTO scratch2 FROM customers"},
	"sys":              {"SELECT * FROM sys.tables", "SELECT definition FROM sys.sql_modules", "SELECT OBJECT_DEFINITION(OBJECT_ID('dbo.customers'))"},
	"catalog":          {"SELECT table_name FROM INFORMATION_SCHEMA.TABLES", "SELECT column_name FROM INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'payroll'"},
	"fourpart":         {"SELECT * FROM srv.db.dbo.payroll"},
	"case":             {"SELECT CASE WHEN id = 1 THEN ssn ELSE email END FROM customers", "SELECT ISNULL(ssn, email) FROM customers", "SELECT COALESCE(ssn, email) FROM customers"},
	"cast":             {"SELECT CAST(ssn AS varchar(50)) FROM customers", "SELECT CONVERT(varchar(50), ssn) FROM customers"},
	"setops":           {"SELECT ssn FROM customers UNION ALL SELECT note FROM scratch", "SELECT amount FROM payroll EXCEPT SELECT total FROM orders"},
	"having-order":     {"SELECT id FROM customers GROUP BY id HAVING MAX(ssn) > ''", "SELECT id FROM customers ORDER BY ssn"},
	"merge":            {"MERGE scratch AS t USING customers AS s ON t.id = s.id WHEN MATCHED THEN UPDATE SET t.note = s.ssn"},
}

func TestEveryPostgresShapeWithTheSecretInIt(t *testing.T) {
	cfg := pgFixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE scratch (id int, note text)",
		"INSERT INTO scratch VALUES (1, 'a')",
		"CREATE PROCEDURE p_none() LANGUAGE sql AS $$ SELECT 1 $$",
	} {
		_, _ = conn.Exec(context.Background(), stmt, nil)
	}
	_ = conn.Close()

	s := Settings{Connection: cfg, Access: AccessBoth, Policy: sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}}
	secrets := []string{"123-45-6789", "the-key"}
	leaks, refused, masked, errs := 0, 0, 0, 0
	for shape, sqls := range pgShapes {
		for _, sql := range sqls {
			res, reason, err := run(context.Background(), s, sql, nil, 0)
			switch {
			case reason != "":
				refused++
			case err != nil:
				errs++
				t.Logf("  engine   %-14s %.80s | %.60s", shape, sql, err.Error())
			default:
				out := fmt.Sprint(res.Rows)
				hit := false
				for _, sec := range secrets {
					if strings.Contains(out, sec) {
						hit = true
					}
				}
				if hit {
					leaks++
					t.Errorf("LEAK [%s] %s\n    -> %.150s", shape, sql, out)
				} else {
					masked++
				}
			}
		}
	}
	t.Logf("SUMMARY: %d leaked, %d refused, %d ran clean, %d engine-rejected", leaks, refused, masked, errs)
}

func TestEveryMySQLShapeWithTheSecretInIt(t *testing.T) {
	s := governed(t)
	s.Access = AccessBoth
	conn, err := datasource.Open(s.Connection)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE scratch (id int, note varchar(100))",
		"INSERT INTO scratch VALUES (1, 'a')",
		"CREATE PROCEDURE p_none() SELECT 1",
	} {
		_, _ = conn.Exec(context.Background(), stmt, nil)
	}
	_ = conn.Close()

	secrets := []string{"123-45-6789", "the-key"}
	leaks, refused, masked, errs := 0, 0, 0, 0
	for shape, sqls := range mysqlShapes {
		for _, sql := range sqls {
			res, reason, err := run(context.Background(), s, sql, nil, 0)
			switch {
			case reason != "":
				refused++
			case err != nil:
				errs++
				t.Logf("  engine   %-16s %-70.70s | %.50s", shape, sql, err.Error())
			default:
				out := fmt.Sprint(res.Rows)
				hit := false
				for _, sec := range secrets {
					if strings.Contains(out, sec) {
						hit = true
					}
				}
				if hit {
					leaks++
					t.Errorf("LEAK [%s] %s\n    -> %.150s", shape, sql, out)
				} else {
					masked++
				}
			}
		}
	}
	t.Logf("SUMMARY: %d leaked, %d refused, %d ran clean, %d engine-rejected", leaks, refused, masked, errs)
}

func TestEverySQLServerShapeWithTheSecretInIt(t *testing.T) {
	cfg := msFixture(t)
	conn, err := datasource.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE PROCEDURE dbo.p_none AS BEGIN SELECT 1 END",
		"CREATE FUNCTION dbo.f_none() RETURNS int AS BEGIN RETURN 1 END",
	} {
		_, _ = conn.Exec(context.Background(), stmt, nil)
	}
	_ = conn.Close()

	s := Settings{Connection: cfg, Access: AccessBoth, Policy: sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "payroll",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}}
	secrets := []string{msRealSSN, "1000"}
	leaks, refused, masked, errs := 0, 0, 0, 0
	for shape, sqls := range msShapes {
		for _, sql := range sqls {
			res, reason, err := run(context.Background(), s, sql, nil, 0)
			switch {
			case reason != "":
				refused++
			case err != nil:
				errs++
				t.Logf("  engine   %-16s %-66.66s | %.45s", shape, sql, err.Error())
			default:
				out := fmt.Sprint(res.Rows)
				hit := false
				for _, sec := range secrets {
					if strings.Contains(out, sec) {
						hit = true
					}
				}
				if hit {
					leaks++
					t.Errorf("LEAK [%s] %s\n    -> %.150s", shape, sql, out)
				} else {
					masked++
				}
			}
		}
	}
	t.Logf("SUMMARY: %d leaked, %d refused, %d ran clean, %d engine-rejected", leaks, refused, masked, errs)
}
