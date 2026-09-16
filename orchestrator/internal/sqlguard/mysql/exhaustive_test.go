package mysql

import (
	"testing"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// This dialect has no sweep, and that is a deliberate difference rather than an
// omission: where the other two analyzers walk the shapes they understand and
// then sweep for anything they missed, this one is a VISITOR, so Enter is called
// on every node the parser produced and every table and column is recorded
// wherever it sits.
//
// That claim is worth a test rather than a comment, because it is what stands in
// for the sweep. This counts the table and column nodes in a tree with an
// INDEPENDENT walk, and asserts the analysis recorded every one of them. A shape
// whose children TiDB's Accept does not descend into would show up here as a
// node the analysis never saw.
//
// Measured while writing it: ast.ProcedureInfo and ast.ProcedureBlock visit
// NOTHING, which is why a routine body is refused as unreadable elsewhere rather
// than walked. No statement this tool may run contains one.

type nodeCounter struct {
	tables  []*ast.TableName
	columns []*ast.ColumnName
}

func (c *nodeCounter) Enter(n ast.Node) (ast.Node, bool) {
	switch v := n.(type) {
	case *ast.TableName:
		c.tables = append(c.tables, v)
	case *ast.ColumnName:
		c.columns = append(c.columns, v)
	}
	return n, false
}

func (c *nodeCounter) Leave(n ast.Node) (ast.Node, bool) { return n, true }

func TestTheWalkVisitsEveryTableAndColumn(t *testing.T) {
	g := standard(t)
	for _, sql := range []string{
		"SELECT c.id, c.email FROM customers c WHERE c.ssn LIKE '1%'",
		"SELECT o.id FROM orders o JOIN customers c ON c.id = o.customer_id WHERE c.ssn > ''",
		"SELECT (SELECT ssn FROM customers LIMIT 1) AS v FROM orders",
		"WITH x AS (SELECT id FROM orders) SELECT * FROM x",
		"SELECT id FROM customers GROUP BY id HAVING MAX(ssn) > '' ORDER BY email",
		"UPDATE scratch SET note = (SELECT email FROM customers LIMIT 1) WHERE id = 1",
		"INSERT INTO scratch SELECT id, note FROM orders",
		"DELETE FROM scratch WHERE note IN (SELECT email FROM customers)",
		"SELECT CASE WHEN id = 1 THEN email ELSE profile END FROM customers",
		"SELECT FIRST_VALUE(email) OVER (PARTITION BY id) FROM customers",
		"SELECT id FROM orders UNION SELECT id FROM scratch",
	} {
		stmt, reason := readStatement(sql)
		if reason != "" {
			t.Fatalf("%s: %s", sql, reason)
		}
		counter := &nodeCounter{}
		stmt.Accept(counter)

		a := &analysis{guard: g}
		a.statement(stmt)
		stmt.Accept(a)

		seenTables := map[*ast.TableName]bool{}
		for _, ref := range a.tables {
			seenTables[ref.name] = true
		}
		seenColumns := map[*ast.ColumnName]bool{}
		for _, ref := range a.columns {
			seenColumns[ref.name] = true
		}
		for _, node := range counter.tables {
			if !seenTables[node] {
				t.Errorf("%s\n  a table node was never recorded: %s", sql, node.Name.O)
			}
		}
		for _, node := range counter.columns {
			if !seenColumns[node] {
				t.Errorf("%s\n  a column node was never recorded: %s", sql, node.Name.O)
			}
		}
		if len(counter.tables) == 0 && len(counter.columns) == 0 {
			t.Errorf("%s: the independent walk found nothing, so this proves nothing", sql)
		}
	}
}
