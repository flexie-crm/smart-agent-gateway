package postgres

import (
	"flexie.io/sag/internal/sqlguard"
	pgq "github.com/pganalyze/pg_query_go/v6"
)

// rewrite is what a statement asked for that has to be written differently for
// it to run at all: a star has to become the columns it stands for once one of
// them is hidden, and a read of the catalog has to be narrowed to the tables
// this tool may see.
func (a *analysis) rewrite() {
	// Innermost query first: a star over a subquery can only be expanded once
	// the subquery's own has been, and scopes were recorded outermost first.
	for i := len(a.scopes) - 1; i >= 0; i-- {
		a.expandStars(a.scopes[i])
		if a.reason != "" {
			return
		}
	}
	for _, s := range a.scopes {
		a.narrowCatalog(s)
		if a.reason != "" {
			return
		}
	}
}

// expandStars turns a star into the columns it stands for, but only where that
// changes something. A query over tables with nothing hidden in them keeps the
// star it was written with and is sent on untouched.
func (a *analysis) expandStars(s *scope) {
	items := s.projection()
	if len(items) == 0 {
		return
	}
	var out []*pgq.Node
	expanded := false
	for _, item := range items {
		qualifier, star := starOf(item)
		if !star {
			out = append(out, item)
			continue
		}
		fields, needed, ok := a.expandStar(s, qualifier)
		if !ok {
			return
		}
		if !needed {
			out = append(out, item)
			continue
		}
		out = append(out, fields...)
		expanded = true
	}
	if expanded {
		s.setProjection(out)
		a.changed = true
	}
}

// starOf reports whether a select item is a star, and what it was qualified
// with when it was.
func starOf(item *pgq.Node) (qualifier string, star bool) {
	rt := item.GetResTarget()
	if rt == nil {
		return "", false
	}
	ref := rt.GetVal().GetColumnRef()
	if ref == nil {
		return "", false
	}
	qualifier, _, star = columnParts(ref)
	return qualifier, star
}

func (a *analysis) expandStar(s *scope, qualifier string) (fields []*pgq.Node, needed, ok bool) {
	covered := s.order
	if qualifier != "" {
		src, found := s.sources[qualifier]
		if !found {
			a.refuse("What %s.* stands for could not be established. Name the columns you want.", qualifier)
			return nil, false, false
		}
		covered = []*source{src}
	}

	for _, src := range covered {
		if src.table == "" {
			continue
		}
		if columns, known := a.guard.Catalog().Columns(src.table); known && a.guard.MasksAnyColumn(src.table, columns) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, false, true
	}
	if s.feeds {
		// A star in the rows an INSERT is writing stands for values going IN.
		// Replacing a hidden one there would not hide anything: it would store
		// the stand-in over the real value, permanently, and the control would be
		// the thing doing the damage.
		a.refuse("This statement writes rows read with *, and one of those columns is hidden. " +
			"Name the columns you mean to write, leaving the hidden one out.")
		return nil, false, false
	}

	for _, src := range covered {
		columns, known := a.columnsOf(src)
		if !known {
			a.refuse("Which columns * stands for could not be established, and one of the tables has a hidden " +
				"field. Name the columns you want.")
			return nil, false, false
		}
		for _, column := range columns {
			fields = append(fields, a.selectField(src, column))
		}
	}
	return fields, true, true
}

// columnsOf is what a source contributes to a star: a table's own columns, or
// the names a subquery projects, which may themselves have come from a star that
// was expanded a moment ago.
func (a *analysis) columnsOf(src *source) ([]string, bool) {
	if src.table != "" {
		return a.guard.Catalog().Columns(src.table)
	}
	if src.query != nil {
		return a.projectedNames(src.query)
	}
	return nil, false
}

// projectedNames is what a subquery calls the columns it returns.
func (a *analysis) projectedNames(sel *pgq.SelectStmt) ([]string, bool) {
	if len(sel.GetTargetList()) == 0 {
		return nil, false
	}
	var out []string
	for _, item := range sel.GetTargetList() {
		rt := item.GetResTarget()
		if rt == nil {
			return nil, false
		}
		if rt.GetName() != "" {
			out = append(out, rt.GetName())
			continue
		}
		ref := rt.GetVal().GetColumnRef()
		if ref == nil {
			return nil, false
		}
		_, column, star := columnParts(ref)
		if star || column == "" {
			return nil, false
		}
		out = append(out, column)
	}
	return out, true
}

// selectField writes one column of an expanded star: the column itself, or the
// value that stands for it when it is hidden, under the name it would have had.
func (a *analysis) selectField(src *source, column string) *pgq.Node {
	if src.table != "" && a.guard.MasksColumn(src.table, column) {
		return &pgq.Node{Node: &pgq.Node_ResTarget{ResTarget: &pgq.ResTarget{
			Name: column,
			Val: &pgq.Node{Node: &pgq.Node_AConst{AConst: &pgq.A_Const{
				Val: &pgq.A_Const_Sval{Sval: &pgq.String{Sval: sqlguard.Hidden}},
			}}},
		}}}
	}
	var fields []*pgq.Node
	if src.alias != "" {
		fields = append(fields, str(src.alias))
	}
	fields = append(fields, str(column))
	return &pgq.Node{Node: &pgq.Node_ResTarget{ResTarget: &pgq.ResTarget{
		Val: &pgq.Node{Node: &pgq.Node_ColumnRef{ColumnRef: &pgq.ColumnRef{Fields: fields}}},
	}}}
}

// narrowCatalog adds to a read of information_schema what the read did not ask
// for: only the tables this tool may see. A tool that answers "which tables are
// there" with the name of a hidden one has already said where to look.
func (a *analysis) narrowCatalog(s *scope) {
	for _, src := range s.order {
		if src.catalog == "" {
			continue
		}
		if s.sel == nil {
			// A statement that changes rows has no WHERE this can be added to in
			// one place, and half a narrowing is worse than none.
			a.refuse("You are not permitted to read information_schema inside a statement that changes rows. " +
				"Ask what the database holds with a SELECT of its own.")
			return
		}
		qualifier := src.alias
		if qualifier == "" {
			qualifier = src.catalog
		}
		a.and(s.sel, a.nameFilter(qualifier, "table_name"))
	}
}

// and joins a condition onto a query's own.
//
// There is no precedence to get wrong here, which there was on the MySQL side:
// the existing condition is a node rather than a run of words, so making it one
// argument of an AND keeps it whole however it was written.
func (a *analysis) and(sel *pgq.SelectStmt, pred *pgq.Node) {
	if pred == nil {
		return
	}
	if sel.WhereClause == nil {
		sel.WhereClause = pred
	} else {
		sel.WhereClause = &pgq.Node{Node: &pgq.Node_BoolExpr{BoolExpr: &pgq.BoolExpr{
			Boolop: pgq.BoolExprType_AND_EXPR,
			Args:   []*pgq.Node{sel.WhereClause, pred},
		}}}
	}
	a.changed = true
}

// nameFilter is the list of tables a catalog read may answer with, written the
// way the policy itself reads: an allowlist asks for the names it permits, so a
// table made since the snapshot is not among them; a denylist asks for
// everything except the names it refuses, so one is.
func (a *analysis) nameFilter(qualifier, column string) *pgq.Node {
	if a.guard.Policy().TableMode == sqlguard.ModeAllowlist {
		visible := a.guard.VisibleTables()
		if len(visible) == 0 {
			return never()
		}
		return anyOf(qualifier, column, visible, false)
	}
	hidden := a.guard.HiddenTables()
	if len(hidden) == 0 {
		return nil
	}
	return anyOf(qualifier, column, hidden, true)
}

// anyOf builds "col IN (...)" or "col NOT IN (...)".
func anyOf(qualifier, column string, names []string, not bool) *pgq.Node {
	elements := make([]*pgq.Node, 0, len(names))
	for _, name := range names {
		elements = append(elements, &pgq.Node{Node: &pgq.Node_AConst{AConst: &pgq.A_Const{
			Val: &pgq.A_Const_Sval{Sval: &pgq.String{Sval: name}},
		}}})
	}
	operator := "="
	if not {
		operator = "<>"
	}
	return &pgq.Node{Node: &pgq.Node_AExpr{AExpr: &pgq.A_Expr{
		Kind:  pgq.A_Expr_Kind_AEXPR_IN,
		Name:  []*pgq.Node{str(operator)},
		Lexpr: columnRefNode(qualifier, column),
		Rexpr: &pgq.Node{Node: &pgq.Node_List{List: &pgq.List{Items: elements}}},
	}}}
}

// never is a condition nothing satisfies, for an allowlist that permits no table
// the database actually has: the read runs and answers with nothing, rather than
// being written as a list of no names, which is not a thing SQL can say.
func never() *pgq.Node {
	return &pgq.Node{Node: &pgq.Node_AExpr{AExpr: &pgq.A_Expr{
		Kind:  pgq.A_Expr_Kind_AEXPR_OP,
		Name:  []*pgq.Node{str("=")},
		Lexpr: number(0),
		Rexpr: number(1),
	}}}
}

func columnRefNode(qualifier, column string) *pgq.Node {
	var fields []*pgq.Node
	if qualifier != "" {
		fields = append(fields, str(qualifier))
	}
	fields = append(fields, str(column))
	return &pgq.Node{Node: &pgq.Node_ColumnRef{ColumnRef: &pgq.ColumnRef{Fields: fields}}}
}

func str(s string) *pgq.Node {
	return &pgq.Node{Node: &pgq.Node_String_{String_: &pgq.String{Sval: s}}}
}

func number(n int32) *pgq.Node {
	return &pgq.Node{Node: &pgq.Node_AConst{AConst: &pgq.A_Const{
		Val: &pgq.A_Const_Ival{Ival: &pgq.Integer{Ival: n}},
	}}}
}
