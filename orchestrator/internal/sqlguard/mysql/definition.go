package mysql

import (
	"strings"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Reading a view's definition is how a rule written about a table's column
// survives being reached through a view that calls the column something else,
// and how a view over a table nobody may see is refused without the statement
// ever naming that table.
//
// Two things come out of a definition: every table it reads, and where each of
// its own columns comes from. Neither is guessed at. A definition that cannot be
// read in full leaves the view unaccounted for, and an unaccounted-for view is
// refused rather than assumed to be harmless.

// ReadDefinition reads a view's defining statement.
func (mysqlAnalyzer) ReadDefinition(definition string) (sqlguard.Definition, bool) {
	stmt, reason := readStatement(definition)
	if reason != "" {
		return sqlguard.Definition{}, false
	}

	reader := &definitionReader{seen: map[string]bool{}}
	stmt.Accept(reader)
	if reader.unreadable {
		return sqlguard.Definition{}, false
	}

	out := sqlguard.Definition{Reads: reader.reads}
	if projection := projectionOf(stmt); projection != nil {
		out.Origin = originsOf(projection)
	}
	return out, true
}

// projectionOf is the query whose select list names the view's own columns. For
// a set of queries joined together that is the first of them, which is where the
// database itself takes the names from.
func projectionOf(stmt ast.StmtNode) *ast.SelectStmt {
	switch v := stmt.(type) {
	case *ast.SelectStmt:
		return v
	case *ast.SetOprStmt:
		if v.SelectList == nil || len(v.SelectList.Selects) == 0 {
			return nil
		}
		return projectionOf(v.SelectList.Selects[0].(ast.StmtNode))
	}
	return nil
}

// originsOf works out, for each column the view offers, which table column it
// reads. Only a plainly selected column has an origin: one built out of an
// expression is not read from a single place, and saying otherwise would be
// making it up. A column with no origin here is hidden whenever anything the
// view reads has something hidden in it, which is the safe way to be wrong.
func originsOf(sel *ast.SelectStmt) map[string]sqlguard.Column {
	if sel.Fields == nil {
		return nil
	}
	sources := baseSources(sel)
	out := map[string]sqlguard.Column{}
	for _, field := range sel.Fields.Fields {
		if field.WildCard != nil {
			// A star in a stored definition is unusual: the database expands one
			// when the view is made. An unexpanded one names no column, so there is
			// nothing here to record.
			continue
		}
		column, ok := field.Expr.(*ast.ColumnNameExpr)
		if !ok || column.Name == nil {
			continue
		}
		name := field.AsName.O
		if name == "" {
			name = column.Name.Name.O
		}
		table, ok := resolveAgainst(sources, column.Name)
		if !ok {
			continue
		}
		out[strings.ToLower(name)] = sqlguard.Column{Table: table, Name: column.Name.Name.O}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// baseSources is the tables a query selects from, by the name they can be
// qualified with. A subquery or a name a WITH introduced is deliberately absent:
// a column read through one of those cannot be traced to a table from here.
func baseSources(sel *ast.SelectStmt) map[string]string {
	out := map[string]string{}
	if sel.From == nil {
		return out
	}
	var walk func(n ast.ResultSetNode)
	walk = func(n ast.ResultSetNode) {
		switch v := n.(type) {
		case *ast.Join:
			walk(v.Left)
			if v.Right != nil {
				walk(v.Right)
			}
		case *ast.TableSource:
			name, ok := v.Source.(*ast.TableName)
			if !ok {
				return
			}
			alias := strings.ToLower(v.AsName.O)
			if alias == "" {
				alias = strings.ToLower(name.Name.O)
			}
			out[alias] = name.Name.O
		}
	}
	walk(sel.From.TableRefs)
	return out
}

// resolveAgainst names the table a column reads from, when one query's own FROM
// settles it. An unqualified column with more than one table to choose from is
// left unresolved rather than picked.
func resolveAgainst(sources map[string]string, name *ast.ColumnName) (string, bool) {
	if qualifier := strings.ToLower(name.Table.O); qualifier != "" {
		table, ok := sources[qualifier]
		return table, ok
	}
	if len(sources) != 1 {
		return "", false
	}
	for _, table := range sources {
		return table, true
	}
	return "", false
}

// definitionReader collects every table a definition reads. The names a WITH
// introduces are tracked as they come into and go out of scope, because one of
// those looks exactly like a table and is not one; anything else that looks like
// a table is recorded, and it is the catalog that decides afterwards whether it
// knows the name.
type definitionReader struct {
	frames []frame
	reads  []sqlguard.Reference
	seen   map[string]bool
	// unreadable marks a definition with something in it this cannot account
	// for, which leaves the view unvouched for rather than quietly allowed.
	unreadable bool
}

type frame struct {
	node ast.Node
	ctes map[string]bool
}

func (d *definitionReader) Enter(n ast.Node) (ast.Node, bool) {
	switch node := n.(type) {
	case *ast.SelectStmt:
		d.push(node, node.With)
	case *ast.SetOprStmt:
		d.push(node, node.With)
	case *ast.TableName:
		d.read(node)
	}
	return n, false
}

func (d *definitionReader) Leave(n ast.Node) (ast.Node, bool) {
	if len(d.frames) > 0 && d.frames[len(d.frames)-1].node == n {
		d.frames = d.frames[:len(d.frames)-1]
	}
	return n, true
}

func (d *definitionReader) push(node ast.Node, with *ast.WithClause) {
	f := frame{node: node, ctes: map[string]bool{}}
	if with != nil {
		for _, cte := range with.CTEs {
			f.ctes[strings.ToLower(cte.Name.O)] = true
		}
	}
	d.frames = append(d.frames, f)
}

func (d *definitionReader) read(name *ast.TableName) {
	if name.Schema.O == "" {
		key := strings.ToLower(name.Name.O)
		for i := len(d.frames) - 1; i >= 0; i-- {
			if d.frames[i].ctes[key] {
				return
			}
		}
	}
	key := strings.ToLower(name.Schema.O) + "." + strings.ToLower(name.Name.O)
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	d.reads = append(d.reads, sqlguard.Reference{Schema: name.Schema.O, Name: name.Name.O})
}
