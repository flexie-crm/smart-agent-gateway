package postgres

import (
	"strings"

	"flexie.io/sag/internal/sqlguard"

	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// decide is the whole policy, applied to what the reading found.
func (a *analysis) decide() {
	for _, ref := range a.tables {
		a.decideTable(ref)
		if a.reason != "" {
			return
		}
	}
	for _, ref := range a.columns {
		a.decideColumn(ref)
		if a.reason != "" {
			return
		}
	}
	for _, ref := range a.inserts {
		a.decideInsert(ref)
		if a.reason != "" {
			return
		}
	}
	for _, ref := range a.assigns {
		a.decideAssignment(ref)
		if a.reason != "" {
			return
		}
	}
}

func (a *analysis) decideTable(ref tableRef) {
	rel := ref.rel
	schema := rel.GetSchemaname()
	switch {
	case strings.EqualFold(schema, "information_schema"):
		if !catalogTables[strings.ToLower(rel.GetRelname())] {
			a.refuse("this tool reads only the parts of information_schema that describe tables and columns " +
				"(tables, columns, table_constraints, key_column_usage)")
		}
		return
	case strings.HasPrefix(strings.ToLower(schema), "pg_"):
		// pg_catalog names tables through joins on oids, and holds the text of
		// view and function bodies. Nothing here could narrow a read of it
		// honestly, so it is refused rather than half-narrowed.
		a.refuse("this tool does not read the server's own catalog schemas. Ask information_schema instead: " +
			"it describes the same tables and columns and can be held to what this tool may see.")
		return
	case schema != "" && !a.guard.Catalog().Knows(schema):
		// A database holds many schemas, and this tool answers for the ones its
		// connection resolves names in. One it does not is one nothing here has a
		// snapshot of.
		a.refuse("this tool reaches the schemas its connection resolves names in, and %q is not one of them", schema)
		return
	}

	if a.knowsCTE(ref.scope, rel) {
		return
	}
	_, known := a.guard.Catalog().Lookup(rel.GetRelname())
	if !known {
		// pg_catalog is on every connection's search path whether anybody put it
		// there or not, so pg_class reaches the server's own catalog without a
		// schema in front of it to refuse. Postgres keeps the pg_ prefix for
		// itself, and a table of ours with that name would be in the snapshot.
		if strings.HasPrefix(strings.ToLower(rel.GetRelname()), "pg_") {
			a.refuse("this tool does not read the server's own catalog. Ask information_schema instead: " +
				"it describes the same tables and columns and can be held to what this tool may see.")
			return
		}
		a.sawUnknown = true
	}
	if ok, reason := a.guard.Reaches(rel.GetRelname()); !ok {
		a.refuse("%s", withoutRetry(reason))
	}
}

func (a *analysis) decideColumn(ref columnRef) {
	if ref.star || ref.column == "" {
		// A star is expanded rather than decided about here; what it stands for
		// is decided one column at a time when it is.
		return
	}
	if ref.whole {
		a.decideRow(ref)
		return
	}
	column := ref.column
	// The common case by far: a column no rule mentions needs no working out at
	// all, which is what keeps the expensive half of this rare.
	if !a.guard.CouldHide(column) {
		return
	}

	tables, origin := a.resolveIn(ref.scope, ref.qualifier, column)
	if origin == fromDerived {
		// It came out of a subquery or a WITH, where it was already decided.
		return
	}
	hidden, agreed := a.verdict(tables, column)
	if origin == unresolved || !agreed {
		// Which table it belongs to could not be established, or the tables it
		// might belong to disagree. Nothing is refused for that: if the value
		// would be returned it is hidden, and if it would not, it is left where
		// it was. Being wrong towards hiding costs a column in a report; being
		// wrong the other way costs the value.
		if ref.target != nil {
			a.hide(ref.target, column)
		}
		return
	}
	if !hidden {
		return
	}
	// A hidden field may be used anywhere. What an administrator hides is the
	// VALUE, and a value is given away by being RETURNED, so that is the one
	// place it is taken out. Reading it to decide which rows come back, what
	// order to put them in, or what to join them to leaves it where it was.
	if ref.target == nil {
		return
	}
	// It IS being returned. The whole select item goes, not only the column: an
	// expression that reads it would otherwise hand the value back with a little
	// arithmetic wrapped round it.
	a.hide(ref.target, column)
}

// decideRow reads a reference to a whole row: SELECT c FROM customers c, and
// everything built on it (row_to_json(c), to_jsonb(c), a row put in a column of
// its own).
//
// A row is every column at once, so there is no select item to replace: taking
// the hidden one out would mean rebuilding the row, and what came back would no
// longer be the row the caller asked for. So it is refused when it is returned
// from a table with something hidden in it, and left alone otherwise. Using a row
// without returning it (WHERE c IS NOT NULL, ORDER BY c) gives nothing away and
// is allowed, which is the same rule every other reference follows.
func (a *analysis) decideRow(ref columnRef) {
	if ref.target == nil {
		return
	}
	src := a.sourceNamed(ref.scope, ref.column)
	if src == nil || src.table == "" {
		// A subquery or a WITH: its columns were decided where they were read,
		// so the row is made of values that already stand in for what is hidden.
		return
	}
	columns, known := a.guard.Catalog().Columns(src.table)
	if !known {
		return
	}
	if a.guard.MasksAnyColumn(src.table, columns) {
		a.refuse("%q here means the whole row of %q, which hands back every column at once including one that is "+
			"hidden in this tool. Name the columns you want.", ref.column, src.table)
	}
}

// sourceNamed finds the source a name refers to, here or in a query this one
// sits inside.
func (a *analysis) sourceNamed(from *scope, name string) *source {
	key := strings.ToLower(name)
	for s := from; s != nil; s = s.parent {
		if src, ok := s.sources[key]; ok {
			return src
		}
	}
	return nil
}

// decideAssignment reads one SET of an UPDATE, or of an ON CONFLICT DO UPDATE.
//
// A SET has no select list, so there is nowhere to put the stand-in: whatever it
// reads goes into the row as it is. Reading a hidden field here would copy the
// real value into a column somebody can read back afterwards, which is returning
// it by a longer route, and the row outlives the answer.
//
// Writing INTO a hidden field is refused for a different reason, and it is not
// about the policy at all: this tool would be storing the stand-in over whatever
// was really there.
func (a *analysis) decideAssignment(ref assignRef) {
	if a.hiddenName(ref.target.GetName(), ref.scope) {
		a.refuse("%q is hidden in this tool, so it cannot be written to: this tool cannot know what is really "+
			"there, and writing would put the stand-in over it.", ref.target.GetName())
		return
	}
	if name := a.firstHiddenIn(ref.target.GetVal()); name != "" {
		a.refuse("%q is hidden in this tool, so its value cannot be copied into another column, where it would "+
			"be readable afterwards. Report that it is not available.", name)
	}
}

// decideInsert covers the one write that names no column at all. INSERT INTO t
// VALUES (...) writes every column of t in order, so it reaches a hidden field
// without a single reference for the walk to have seen, and the same statement
// written with its column list would have been refused.
func (a *analysis) decideInsert(ref insertRef) {
	// Named columns say plainly where the rows are going.
	for _, col := range ref.stmt.GetCols() {
		rt := col.GetResTarget()
		if rt == nil {
			continue
		}
		if a.hiddenName(rt.GetName(), ref.scope) {
			a.refuse("%q is hidden in this tool, so it cannot be written to: this tool cannot know what is really "+
				"there, and writing would put the stand-in over it.", rt.GetName())
			return
		}
	}
	if len(ref.stmt.GetCols()) > 0 {
		return
	}
	for _, src := range ref.scope.order {
		if src.table == "" {
			continue
		}
		columns, ok := a.guard.Catalog().Columns(src.table)
		if !ok {
			continue
		}
		for _, column := range columns {
			if a.guard.MasksColumn(src.table, column) {
				a.refuse("%q is hidden in this tool, and an INSERT without a column list writes every column of "+
					"%q including that one. Name the columns you mean to write.", column, src.table)
				return
			}
		}
	}
}

// hiddenName reports whether a column named as a bare word, read where it
// stands, is one the policy hides. It is the decision decideColumn makes,
// without the part about what to do next.
func (a *analysis) hiddenName(column string, s *scope) bool {
	if column == "" || !a.guard.CouldHide(column) {
		return false
	}
	tables, origin := a.resolveIn(s, "", column)
	if origin == fromDerived {
		return false
	}
	hidden, agreed := a.verdict(tables, column)
	return hidden || !agreed || origin == unresolved
}

// firstHiddenIn names the first hidden column an expression reads, or nothing.
//
// The expression is walked by reflection rather than by the shapes the first
// pass understands: a column reached through a node nobody listed still puts a
// real value into the row, and the sweep that would otherwise catch it only runs
// over references the first pass did not account for.
func (a *analysis) firstHiddenIn(node *pgq.Node) string {
	if node == nil {
		return ""
	}
	found := ""
	walk(node.ProtoReflect(), func(m protoreflect.Message) {
		if found != "" {
			return
		}
		ref, ok := m.Interface().(*pgq.ColumnRef)
		if !ok {
			return
		}
		known, seen := a.byColumn[ref]
		if !seen || known.star || known.column == "" {
			return
		}
		if known.whole {
			// A whole row copied into a column takes every hidden value with it.
			if src := a.sourceNamed(known.scope, known.column); src != nil && src.table != "" {
				if columns, ok := a.guard.Catalog().Columns(src.table); ok && a.guard.MasksAnyColumn(src.table, columns) {
					found = known.column
				}
			}
			return
		}
		if !a.guard.CouldHide(known.column) {
			return
		}
		tables, origin := a.resolveIn(known.scope, known.qualifier, known.column)
		if origin == fromDerived {
			return
		}
		hidden, agreed := a.verdict(tables, known.column)
		if hidden || !agreed || origin == unresolved {
			found = known.column
		}
	})
	return found
}

type columnOrigin int

const (
	fromTable columnOrigin = iota
	fromDerived
	unresolved
)

// resolveIn works out which table a column reads from. What comes back is every
// table it could belong to: one, for a column written with its table or found in
// only one of them, and several when the statement leaves it open.
func (a *analysis) resolveIn(from *scope, qualifier, column string) ([]string, columnOrigin) {
	if qualifier != "" {
		for s := from; s != nil; s = s.parent {
			if src, ok := s.sources[qualifier]; ok {
				if src.derived || src.catalog != "" {
					return nil, fromDerived
				}
				return []string{src.table}, fromTable
			}
		}
		return nil, unresolved
	}

	sawDerived := false
	for s := from; s != nil; s = s.parent {
		var found []string
		for _, src := range s.order {
			if src.derived || src.catalog != "" {
				sawDerived = true
				continue
			}
			if a.guard.Catalog().HasColumn(src.table, column) {
				found = append(found, src.table)
			}
		}
		if len(found) > 0 {
			return found, fromTable
		}
	}
	if sawDerived {
		return nil, fromDerived
	}
	return nil, unresolved
}

// verdict is what the candidate tables say about a column, and whether they say
// the same thing.
func (a *analysis) verdict(tables []string, column string) (hidden, agreed bool) {
	if len(tables) == 0 {
		return false, false
	}
	hidden = a.guard.MasksColumn(tables[0], column)
	for _, table := range tables[1:] {
		if a.guard.MasksColumn(table, column) != hidden {
			return false, false
		}
	}
	return hidden, true
}

// hide replaces a returned select item with the value that stands for it,
// keeping the name the result would have had so a caller reading the columns
// back still finds the one it asked for.
func (a *analysis) hide(target *pgq.ResTarget, column string) {
	if target.Name == "" {
		target.Name = a.nameOf(target, column)
	}
	target.Val = &pgq.Node{Node: &pgq.Node_AConst{AConst: &pgq.A_Const{
		Val: &pgq.A_Const_Sval{Sval: &pgq.String{Sval: sqlguard.Hidden}},
	}}}
	a.changed = true
}

// nameOf is the name Postgres itself would have given a select item that had no
// alias, so a caller reading the columns back finds the one it asked for rather
// than a column that changed its name because a policy touched it.
//
// Postgres names an unaliased item after the column it reads, or after the
// function that produced it, and calls anything else ?column?. That last one is
// no use to a reader, so the hidden column's own name stands in for it.
func (a *analysis) nameOf(target *pgq.ResTarget, column string) string {
	var from func(node *pgq.Node) string
	from = func(node *pgq.Node) string {
		if node == nil {
			return ""
		}
		switch v := node.Node.(type) {
		case *pgq.Node_ColumnRef:
			_, name, star := columnParts(v.ColumnRef)
			if !star {
				return name
			}
		case *pgq.Node_FuncCall:
			names := v.FuncCall.GetFuncname()
			if len(names) > 0 {
				return names[len(names)-1].GetString_().GetSval()
			}
		case *pgq.Node_TypeCast:
			return from(v.TypeCast.GetArg())
		case *pgq.Node_CollateClause:
			return from(v.CollateClause.GetArg())
		case *pgq.Node_CaseExpr:
			return "case"
		case *pgq.Node_CoalesceExpr:
			return "coalesce"
		}
		return ""
	}
	if name := from(target.GetVal()); name != "" {
		return name
	}
	return column
}
