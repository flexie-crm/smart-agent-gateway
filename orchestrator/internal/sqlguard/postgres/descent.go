package postgres

import (
	"strings"

	pgq "github.com/pganalyze/pg_query_go/v6"
)

// The first pass: walk the shapes this understands and record where everything
// sits. What is NOT here is caught by the sweep, which refuses rather than
// guesses, so this is allowed to be incomplete without being unsafe.

// query reads one SELECT, in a scope of its own.
//
// The scope is recorded before anything inside it is read, so the list of scopes
// runs outermost first. Rewriting walks it backwards, which is how a star over a
// subquery is expanded only after the subquery's own has been.
func (a *analysis) query(sel *pgq.SelectStmt, parent *scope) {
	if sel == nil || a.reason != "" {
		return
	}
	s := a.newScope(parent)
	s.sel = sel

	// A WITH is read first: its names are what the FROM below may refer to, and
	// each body is a query in its own right.
	a.withClause(sel.GetWithClause(), s)

	for _, from := range sel.GetFromClause() {
		a.from(from, s)
	}

	// The select list is the one position a hidden field is RETURNED from.
	for _, target := range sel.GetTargetList() {
		if rt := target.GetResTarget(); rt != nil {
			a.expression(rt.GetVal(), s, rt)
		}
	}
	// Everything else reads without returning.
	a.expression(sel.GetWhereClause(), s, nil)
	a.expression(sel.GetHavingClause(), s, nil)
	for _, group := range sel.GetGroupClause() {
		a.expression(group, s, nil)
	}
	for _, sort := range sel.GetSortClause() {
		if by := sort.GetSortBy(); by != nil {
			a.expression(by.GetNode(), s, nil)
		}
	}
	for _, window := range sel.GetWindowClause() {
		a.expression(window, s, nil)
	}
	a.expression(sel.GetLimitCount(), s, nil)
	a.expression(sel.GetLimitOffset(), s, nil)
	for _, values := range sel.GetValuesLists() {
		a.expression(values, s, nil)
	}

	// A set operation is two queries; each keeps its own projection.
	a.query(sel.GetLarg(), parent)
	a.query(sel.GetRarg(), parent)
}

// newScope records a scope in the order it was entered.
func (a *analysis) newScope(parent *scope) *scope {
	s := &scope{
		parent:  parent,
		feeds:   a.feeding > 0,
		ctes:    map[string]bool{},
		sources: map[string]*source{},
	}
	a.scopes = append(a.scopes, s)
	return s
}

// statementIn reads a statement that sits inside another one: a CTE body, which
// in Postgres may be a write, or the query an INSERT takes its rows from.
func (a *analysis) statementIn(node *pgq.Node, parent *scope) {
	if node == nil {
		return
	}
	switch v := node.Node.(type) {
	case *pgq.Node_SelectStmt:
		a.query(v.SelectStmt, parent)
	case *pgq.Node_InsertStmt:
		a.write = true
		a.insert(v.InsertStmt, parent)
	case *pgq.Node_UpdateStmt:
		a.write = true
		a.update(v.UpdateStmt, parent)
	case *pgq.Node_DeleteStmt:
		a.write = true
		a.delete(v.DeleteStmt, parent)
	}
}

// insert reads an INSERT. Its RETURNING list is a projection like any other: a
// write that hands rows back is a read wearing a different hat.
func (a *analysis) insert(stmt *pgq.InsertStmt, parent *scope) {
	if stmt == nil {
		return
	}
	s := a.newScope(parent)
	s.returns = &stmt.ReturningList
	a.withClause(stmt.GetWithClause(), s)
	a.relation(stmt.GetRelation(), s)
	a.inserts = append(a.inserts, insertRef{stmt: stmt, scope: s})

	// The rows going in. Inside there a star cannot be masked, because what it
	// stands for is stored rather than shown.
	a.feeding++
	a.statementIn(stmt.GetSelectStmt(), s)
	a.feeding--

	a.onConflict(stmt.GetOnConflictClause(), s)
	a.returning(stmt.GetReturningList(), s)
}

// onConflict reads the ON CONFLICT of an INSERT, which is an UPDATE written in
// another place: its SET parts assign, exactly as an UPDATE's do.
func (a *analysis) onConflict(clause *pgq.OnConflictClause, s *scope) {
	if clause == nil {
		return
	}
	if target := clause.GetInfer(); target != nil {
		for _, elem := range target.GetIndexElems() {
			if e := elem.GetIndexElem(); e != nil {
				a.namedColumn(e.GetName(), s)
				a.expression(e.GetExpr(), s, nil)
			}
		}
		a.expression(target.GetWhereClause(), s, nil)
	}
	for _, item := range clause.GetTargetList() {
		if rt := item.GetResTarget(); rt != nil {
			a.assigns = append(a.assigns, assignRef{target: rt, scope: s})
			a.expression(rt.GetVal(), s, nil)
		}
	}
	a.expression(clause.GetWhereClause(), s, nil)
}

func (a *analysis) update(stmt *pgq.UpdateStmt, parent *scope) {
	if stmt == nil {
		return
	}
	s := a.newScope(parent)
	s.returns = &stmt.ReturningList
	a.withClause(stmt.GetWithClause(), s)
	a.relation(stmt.GetRelation(), s)
	for _, from := range stmt.GetFromClause() {
		a.from(from, s)
	}

	for _, target := range stmt.GetTargetList() {
		if rt := target.GetResTarget(); rt != nil {
			// A SET has nowhere to put a stand-in, so it is decided on its own
			// rather than through the select-list rule.
			a.assigns = append(a.assigns, assignRef{target: rt, scope: s})
			a.expression(rt.GetVal(), s, nil)
		}
	}
	a.expression(stmt.GetWhereClause(), s, nil)
	a.returning(stmt.GetReturningList(), s)
}

func (a *analysis) delete(stmt *pgq.DeleteStmt, parent *scope) {
	if stmt == nil {
		return
	}
	s := a.newScope(parent)
	s.returns = &stmt.ReturningList
	a.withClause(stmt.GetWithClause(), s)
	a.relation(stmt.GetRelation(), s)
	for _, using := range stmt.GetUsingClause() {
		a.from(using, s)
	}
	a.expression(stmt.GetWhereClause(), s, nil)
	a.returning(stmt.GetReturningList(), s)
}

func (a *analysis) withClause(with *pgq.WithClause, s *scope) {
	if with == nil {
		return
	}
	for _, cte := range with.GetCtes() {
		if c := cte.GetCommonTableExpr(); c != nil {
			s.ctes[strings.ToLower(c.GetCtename())] = true
		}
	}
	for _, cte := range with.GetCtes() {
		if c := cte.GetCommonTableExpr(); c != nil {
			a.statementIn(c.GetCtequery(), s)
		}
	}
}

// returning is a projection: what a write hands back is returned exactly as a
// select list is.
func (a *analysis) returning(list []*pgq.Node, s *scope) {
	for _, item := range list {
		if rt := item.GetResTarget(); rt != nil {
			a.expression(rt.GetVal(), s, rt)
		}
	}
}

// relation records the table a write is aimed at.
func (a *analysis) relation(rel *pgq.RangeVar, s *scope) {
	if rel == nil {
		return
	}
	a.source(rel, s)
	a.tables = append(a.tables, tableRef{rel: rel, scope: s})
}

// from reads one entry of a FROM.
func (a *analysis) from(node *pgq.Node, s *scope) {
	if node == nil || a.reason != "" {
		return
	}
	switch v := node.Node.(type) {
	case *pgq.Node_RangeVar:
		a.source(v.RangeVar, s)
		a.tables = append(a.tables, tableRef{rel: v.RangeVar, scope: s})
	case *pgq.Node_JoinExpr:
		join := v.JoinExpr
		a.from(join.GetLarg(), s)
		a.from(join.GetRarg(), s)
		// A join condition reads without returning, and USING names columns the
		// same way.
		a.expression(join.GetQuals(), s, nil)
		for _, using := range join.GetUsingClause() {
			if str := using.GetString_(); str != nil {
				a.namedColumn(str.GetSval(), s)
			}
		}
	case *pgq.Node_RangeSubselect:
		sub := v.RangeSubselect
		src := &source{derived: true, alias: strings.ToLower(sub.GetAlias().GetAliasname())}
		if inner := sub.GetSubquery().GetSelectStmt(); inner != nil {
			src.query = inner
		}
		a.add(s, src)
		a.statementIn(sub.GetSubquery(), s)
	case *pgq.Node_RangeFunction:
		// A function in a FROM is not a table; it names no relation to govern.
		fn := v.RangeFunction
		a.add(s, &source{derived: true, alias: strings.ToLower(fn.GetAlias().GetAliasname())})
		for _, f := range fn.GetFunctions() {
			a.expression(f, s, nil)
		}
	default:
		a.refuse("this tool could not work out which tables that statement reads, so it did not run it")
	}
}

// source records what a FROM entry brings into scope.
func (a *analysis) source(rel *pgq.RangeVar, s *scope) {
	src := &source{alias: aliasOf(rel)}
	switch {
	case strings.EqualFold(rel.GetSchemaname(), "information_schema"):
		src.catalog = strings.ToLower(rel.GetRelname())
	case a.knowsCTE(s, rel):
		src.derived = true
	default:
		table := rel.GetRelname()
		if t, ok := a.guard.Catalog().Lookup(table); ok {
			table = t.Name
		}
		src.table = table
	}
	a.add(s, src)
}

func (a *analysis) add(s *scope, src *source) {
	// Every source is kept, in the order the FROM brought it in; the map is only
	// for looking a qualifier up, and the first to claim a name keeps it. The
	// second does not quietly replace the first: a statement with two of the same
	// name is one the database refuses anyway, and letting it replace is how a
	// read of the catalog stopped being narrowed at all.
	if src.alias != "" {
		if _, taken := s.sources[src.alias]; !taken {
			s.sources[src.alias] = src
		}
	}
	s.order = append(s.order, src)
}

// knowsCTE reports whether a name is one a WITH introduced, here or in a query
// this one sits inside. Such a name looks exactly like a table and is not one.
func (a *analysis) knowsCTE(s *scope, rel *pgq.RangeVar) bool {
	if rel.GetSchemaname() != "" {
		return false
	}
	key := strings.ToLower(rel.GetRelname())
	for ; s != nil; s = s.parent {
		if s.ctes[key] {
			return true
		}
	}
	return false
}

// expression reads anything that is not a statement or a FROM. target is the
// select item it belongs to, when it belongs to one; that is what makes the
// difference between a value being returned and a value merely being read.
func (a *analysis) expression(node *pgq.Node, s *scope, target *pgq.ResTarget) {
	if node == nil || a.reason != "" {
		return
	}
	switch v := node.Node.(type) {
	case *pgq.Node_ColumnRef:
		a.column(v.ColumnRef, s, target)
		return
	case *pgq.Node_AIndirection:
		if a.compositeField(v.AIndirection, s, target) {
			return
		}
	case *pgq.Node_SubLink:
		// A subquery inside an expression has a projection of its own, and what
		// it returns is decided there rather than here.
		a.expression(v.SubLink.GetTestexpr(), s, target)
		a.statementIn(v.SubLink.GetSubselect(), s)
		return
	}
	// Everything else is a shape whose children sit in the same position it
	// does, so they are walked without changing what position means.
	for _, child := range operands(node) {
		a.expression(child, s, target)
	}
}

// column records a column reference and what it means where it stands.
//
// A bare name is a column when something in scope has one by that name, and a
// whole ROW when nothing does and a source answers to it instead. That second
// reading is the one Postgres has and MySQL does not, and it hands back every
// column of a table at once without naming any of them.
func (a *analysis) column(ref *pgq.ColumnRef, s *scope, target *pgq.ResTarget) {
	qualifier, name, star := columnParts(ref)
	out := columnRef{ref: ref, scope: s, target: target, qualifier: qualifier, column: name, star: star}
	if !star && qualifier == "" && a.namesRow(s, name) {
		out.whole, out.column = true, name
	}
	a.recordColumn(out)
}

// namesRow reports whether a bare name is a source rather than a column. A
// column wins when both would fit, which is the order Postgres resolves in.
func (a *analysis) namesRow(from *scope, name string) bool {
	key := strings.ToLower(name)
	for s := from; s != nil; s = s.parent {
		for _, src := range s.order {
			if src.table != "" && a.guard.Catalog().HasColumn(src.table, name) {
				return false
			}
		}
	}
	for s := from; s != nil; s = s.parent {
		if _, ok := s.sources[key]; ok {
			return true
		}
	}
	return false
}

// compositeField reads (c).ssn, which names a column without a column reference
// anywhere in it: the row is a reference and the field beside it is a bare
// string. Read as written it looks like nothing but a mention of c, which is how
// a hidden column would come back in full.
//
// It reports whether it accounted for the whole shape. One field of a row is a
// qualified column and is treated as one; anything else (a field of a field, or
// (c).*) is left to the row reference itself, which is refused when it is
// returned from a table with something hidden in it.
func (a *analysis) compositeField(node *pgq.A_Indirection, s *scope, target *pgq.ResTarget) bool {
	ref := node.GetArg().GetColumnRef()
	if ref == nil || len(node.GetIndirection()) != 1 {
		return false
	}
	field := node.GetIndirection()[0].GetString_()
	if field == nil || field.GetSval() == "" {
		return false
	}
	qualifier, name, star := columnParts(ref)
	switch {
	case star && qualifier != "":
		// (customers.*).ssn
		name = qualifier
	case star:
		return false
	case qualifier != "":
		// (c.x).y is a field of a column, not of a row.
		return false
	case !a.namesRow(s, name):
		return false
	}
	a.recordColumn(columnRef{
		ref: ref, scope: s, target: target,
		qualifier: strings.ToLower(name), column: field.GetSval(),
	})
	return true
}

func (a *analysis) recordColumn(ref columnRef) {
	a.columns = append(a.columns, ref)
	if a.byColumn == nil {
		a.byColumn = map[*pgq.ColumnRef]columnRef{}
	}
	a.byColumn[ref.ref] = ref
}

// operands are the sub-expressions of an expression node. A shape missing here
// is not a hole: its columns are then unaccounted for, and the sweep refuses
// the statement rather than letting them past.
func operands(node *pgq.Node) []*pgq.Node {
	switch v := node.Node.(type) {
	case *pgq.Node_AExpr:
		return []*pgq.Node{v.AExpr.GetLexpr(), v.AExpr.GetRexpr()}
	case *pgq.Node_BoolExpr:
		return v.BoolExpr.GetArgs()
	case *pgq.Node_FuncCall:
		out := append([]*pgq.Node{}, v.FuncCall.GetArgs()...)
		out = append(out, v.FuncCall.GetAggFilter())
		if over := v.FuncCall.GetOver(); over != nil {
			out = append(out, over.GetPartitionClause()...)
			for _, sort := range over.GetOrderClause() {
				if by := sort.GetSortBy(); by != nil {
					out = append(out, by.GetNode())
				}
			}
		}
		for _, sort := range v.FuncCall.GetAggOrder() {
			if by := sort.GetSortBy(); by != nil {
				out = append(out, by.GetNode())
			}
		}
		return out
	case *pgq.Node_TypeCast:
		return []*pgq.Node{v.TypeCast.GetArg()}
	case *pgq.Node_CollateClause:
		return []*pgq.Node{v.CollateClause.GetArg()}
	case *pgq.Node_CaseExpr:
		out := []*pgq.Node{v.CaseExpr.GetArg(), v.CaseExpr.GetDefresult()}
		return append(out, v.CaseExpr.GetArgs()...)
	case *pgq.Node_CaseWhen:
		return []*pgq.Node{v.CaseWhen.GetExpr(), v.CaseWhen.GetResult()}
	case *pgq.Node_CoalesceExpr:
		return v.CoalesceExpr.GetArgs()
	case *pgq.Node_MinMaxExpr:
		return v.MinMaxExpr.GetArgs()
	case *pgq.Node_NullTest:
		return []*pgq.Node{v.NullTest.GetArg()}
	case *pgq.Node_BooleanTest:
		return []*pgq.Node{v.BooleanTest.GetArg()}
	case *pgq.Node_List:
		return v.List.GetItems()
	case *pgq.Node_RowExpr:
		return v.RowExpr.GetArgs()
	case *pgq.Node_AArrayExpr:
		return v.AArrayExpr.GetElements()
	case *pgq.Node_AIndirection:
		out := []*pgq.Node{v.AIndirection.GetArg()}
		return append(out, v.AIndirection.GetIndirection()...)
	case *pgq.Node_AIndices:
		return []*pgq.Node{v.AIndices.GetLidx(), v.AIndices.GetUidx()}
	case *pgq.Node_GroupingSet:
		return v.GroupingSet.GetContent()
	case *pgq.Node_GroupingFunc:
		return v.GroupingFunc.GetArgs()
	case *pgq.Node_NamedArgExpr:
		return []*pgq.Node{v.NamedArgExpr.GetArg()}
	case *pgq.Node_XmlExpr:
		out := append([]*pgq.Node{}, v.XmlExpr.GetNamedArgs()...)
		return append(out, v.XmlExpr.GetArgs()...)
	case *pgq.Node_SortBy:
		return []*pgq.Node{v.SortBy.GetNode()}
	case *pgq.Node_WindowDef:
		out := append([]*pgq.Node{}, v.WindowDef.GetPartitionClause()...)
		for _, sort := range v.WindowDef.GetOrderClause() {
			if by := sort.GetSortBy(); by != nil {
				out = append(out, by.GetNode())
			}
		}
		return out
	case *pgq.Node_SubLink:
		return []*pgq.Node{v.SubLink.GetTestexpr()}
	}
	return nil
}

// namedColumn records a column named as a bare word rather than as a reference:
// a USING clause, the columns an ON CONFLICT infers on. Neither returns
// anything, so neither can be a projection.
func (a *analysis) namedColumn(column string, s *scope) {
	if column == "" {
		return
	}
	a.recordColumn(columnRef{
		ref:   &pgq.ColumnRef{Fields: []*pgq.Node{{Node: &pgq.Node_String_{String_: &pgq.String{Sval: column}}}}},
		scope: s,
	})
}
