package postgres

import "testing"

// The sweep is the pass that makes the descent trustworthy, and it is worth a
// test of its own rather than a test of some shape that happens to be unhandled
// today. A shape the descent refuses BY NAME proves nothing about the sweep: it
// would be refused with the sweep deleted.
//
// So this drives the mechanism directly. It reads a statement the descent
// handles completely, then takes one reference out of what the descent accounted
// for, exactly as an unvisited node would be absent, and asserts the sweep
// refuses. The control is the same statement with nothing removed: if that ever
// refuses too, this is measuring something other than what it claims.
func TestTheSweepRefusesWhatTheDescentNeverVisited(t *testing.T) {
	g := standard(t)
	const sql = `SELECT c.id, c.email FROM customers c WHERE c.ssn LIKE '1%'`

	// The control: fully accounted for, nothing refused.
	tree, reason := readStatement(sql)
	if reason != "" {
		t.Fatalf("control: %s", reason)
	}
	a := &analysis{guard: g}
	a.statement(tree.Stmts[0].Stmt)
	a.sweep(tree)
	if a.reason != "" {
		t.Fatalf("control: a fully visited statement was refused: %s", a.reason)
	}
	if len(a.columns) == 0 || len(a.tables) == 0 {
		t.Fatal("control: the descent found nothing, so removing one proves nothing")
	}

	// A column the descent never accounted for.
	tree, _ = readStatement(sql)
	b := &analysis{guard: g}
	b.statement(tree.Stmts[0].Stmt)
	b.columns = b.columns[1:]
	b.sweep(tree)
	if b.reason == "" {
		t.Error("a column the descent never accounted for was not refused, so nothing " +
			"stands between an unwalked corner of the tree and a hidden value")
	}

	// And a table.
	tree, _ = readStatement(sql)
	c := &analysis{guard: g}
	c.statement(tree.Stmts[0].Stmt)
	c.tables = c.tables[1:]
	c.sweep(tree)
	if c.reason == "" {
		t.Error("a table the descent never accounted for was not refused")
	}
}
