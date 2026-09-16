package postgres

import (
	"strings"

	"flexie.io/sag/internal/sqlguard"
	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Reading a view's definition is how a rule written about a table's column
// survives being reached through a view that calls the column something else,
// and how a view over a table nobody may see is refused without the statement
// ever naming that table.
//
// A definition that cannot be read in full leaves the view unaccounted for, and
// an unaccounted-for view is refused rather than assumed to be harmless.
func (Analyzer) ReadDefinition(definition string) (sqlguard.Definition, bool) {
	tree, reason := readStatement(definition)
	if reason != "" {
		return sqlguard.Definition{}, false
	}
	reader := &definitionReader{seen: map[string]bool{}}
	sel := tree.Stmts[0].Stmt.GetSelectStmt()
	switch {
	case sel != nil:
		reader.query(sel, nil)
	case tree.Stmts[0].Stmt.GetCallStmt() != nil:
		// CALL p(): a body that runs another routine. The routine it names is
		// picked up by calledRoutines below and followed from there, the same as
		// a function called in an expression. It reads no table of its own, which
		// is why there is nothing to walk here.
	default:
		return sqlguard.Definition{}, false
	}

	out := sqlguard.Definition{Reads: reader.reads, Calls: calledRoutines(tree)}
	if origin := originsOf(projectionOf(sel)); len(origin) > 0 {
		out.Origin = origin
	}
	return out, true
}

// projectionOf is the query whose select list names the view's own columns. For
// a set of queries joined together that is the first of them, which is where the
// database itself takes the names from.
func projectionOf(sel *pgq.SelectStmt) *pgq.SelectStmt {
	for sel != nil && len(sel.GetTargetList()) == 0 && sel.GetLarg() != nil {
		sel = sel.GetLarg()
	}
	return sel
}

// originsOf works out, for each column the view offers, which table column it
// reads. Only a plainly selected column has an origin: one built out of an
// expression is not read from a single place, and saying otherwise would be
// making it up.
func originsOf(sel *pgq.SelectStmt) map[string]sqlguard.Column {
	if sel == nil {
		return nil
	}
	sources := baseSources(sel)
	out := map[string]sqlguard.Column{}
	for _, item := range sel.GetTargetList() {
		rt := item.GetResTarget()
		if rt == nil {
			continue
		}
		ref := rt.GetVal().GetColumnRef()
		if ref == nil {
			continue
		}
		qualifier, column, star := columnParts(ref)
		if star || column == "" {
			continue
		}
		name := rt.GetName()
		if name == "" {
			name = column
		}
		table, ok := resolveAgainst(sources, qualifier)
		if !ok {
			continue
		}
		out[strings.ToLower(name)] = sqlguard.Column{Table: table, Name: column}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// baseSources is the tables a query selects from, by the name they can be
// qualified with. A subquery or a name a WITH introduced is deliberately absent:
// a column read through one of those cannot be traced to a table from here.
func baseSources(sel *pgq.SelectStmt) map[string]string {
	out := map[string]string{}
	var walkFrom func(node *pgq.Node)
	walkFrom = func(node *pgq.Node) {
		if node == nil {
			return
		}
		switch v := node.Node.(type) {
		case *pgq.Node_RangeVar:
			out[aliasOf(v.RangeVar)] = v.RangeVar.GetRelname()
		case *pgq.Node_JoinExpr:
			walkFrom(v.JoinExpr.GetLarg())
			walkFrom(v.JoinExpr.GetRarg())
		}
	}
	for _, from := range sel.GetFromClause() {
		walkFrom(from)
	}
	return out
}

// resolveAgainst names the table a column reads from, when one query's own FROM
// settles it. An unqualified column with more than one table to choose from is
// left unresolved rather than picked.
func resolveAgainst(sources map[string]string, qualifier string) (string, bool) {
	if qualifier != "" {
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
// those looks exactly like a table and is not one.
type definitionReader struct {
	reads []sqlguard.Reference
	seen  map[string]bool
}

func (d *definitionReader) query(sel *pgq.SelectStmt, ctes map[string]bool) {
	if sel == nil {
		return
	}
	scope := map[string]bool{}
	for name := range ctes {
		scope[name] = true
	}
	if with := sel.GetWithClause(); with != nil {
		for _, cte := range with.GetCtes() {
			if c := cte.GetCommonTableExpr(); c != nil {
				scope[strings.ToLower(c.GetCtename())] = true
			}
		}
		for _, cte := range with.GetCtes() {
			if c := cte.GetCommonTableExpr(); c != nil {
				d.query(c.GetCtequery().GetSelectStmt(), scope)
			}
		}
	}

	// Every RangeVar in this query, wherever it sits, minus the names the WITH
	// introduced. Reflection rather than a hand-listed set of shapes: a table
	// reached through something nobody thought of still counts as read.
	walk(sel.ProtoReflect(), func(m protoreflect.Message) {
		rel, ok := m.Interface().(*pgq.RangeVar)
		if !ok {
			return
		}
		if rel.GetSchemaname() == "" && scope[strings.ToLower(rel.GetRelname())] {
			return
		}
		key := strings.ToLower(rel.GetSchemaname()) + "." + strings.ToLower(rel.GetRelname())
		if d.seen[key] {
			return
		}
		d.seen[key] = true
		d.reads = append(d.reads, sqlguard.Reference{Schema: rel.GetSchemaname(), Name: rel.GetRelname()})
	})
}

// calledRoutines is every function this definition invokes, by name.
//
// A function call is an expression here, indistinguishable in the tree from
// now() or lower(), so this reports the names and lets the guard decide: it
// holds the catalogue, and only a name the catalogue knows is a stored routine.
// Refusing on every function call would refuse every body worth having.
func calledRoutines(tree *pgq.ParseResult) []string {
	var out []string
	walk(tree.ProtoReflect(), func(m protoreflect.Message) {
		call, ok := m.Interface().(*pgq.FuncCall)
		if !ok {
			return
		}
		// The same name the statement path decides about, which is the whole of
		// it: the last part alone made a call into a schema nobody read look like
		// a bare built-in, and a built-in is waved through.
		if name, ok := routineName(call); ok {
			out = append(out, name)
		}
	})
	return out
}
