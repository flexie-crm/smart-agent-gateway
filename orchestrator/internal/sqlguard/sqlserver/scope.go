package sqlserver

import (
	"fmt"
	"strings"

	"flexie.io/sag/lib/sqlserver/antlr"

	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/lib/sqlserver/tsql"
)

// Working out what a statement would touch, and where each thing sits.
//
// There are two passes over the tree and they do different jobs. The first is a
// descent through the shapes this package understands (below), and it is the one
// that knows POSITION: a column in a select list is returned and one in a WHERE
// is not; a name in a FROM may be a table or may be something a WITH introduced.
// Position cannot be had any other way, so the descent is written by hand.
//
// A hand-written descent has exactly one failure mode and it is the one that
// matters: a shape nobody thought of is never visited, so what it names is never
// decided about, and it passes in silence. The second pass (sweep) exists for
// that and nothing else. It walks EVERY node the parser produced, finds every
// table, column and star in the tree whatever it is nested in, and asks one
// question of each: did the first pass account for you? Anything unaccounted for
// refuses the statement. The first pass is what decides; the second is what
// makes trusting the first pass reasonable.

// analysis is one statement being read.
type analysis struct {
	// hiddenFields are the fields this statement would have had replaced with
	// the placeholder, so a refusal about a routine can name them.
	hiddenFields []string
	guard        *sqlguard.Guard
	stream       *antlr.CommonTokenStream

	// reason is the first thing found wrong, which is what gets reported.
	reason string

	write       bool
	changed     bool
	sawUnknown  bool
	listsTables bool

	// feeding is above zero while inside the query an INSERT takes its rows
	// FROM. A star there stands for values going into a table rather than values
	// coming back, which is the one place a star must not be masked: replacing a
	// hidden column there would store the stand-in over the real value,
	// permanently, and the control would be the thing doing the damage.
	feeding int

	scopes []*scope

	tables      []tableRef
	columns     []columnRef
	stars       []starRef
	assignments []assignRef
	// rolled are FOR XML / FOR JSON clauses, which turn a whole result into one
	// value. There is no select item left to replace, so a statement with one is
	// refused when anything it reads has something hidden in it.
	rolled []tsql.IFor_clauseContext

	// seen records what the descent accounted for, so the sweep can tell whether
	// anything went unvisited. Keyed by the node itself rather than by a name:
	// two references to one table in a statement are two nodes, and are decided
	// about separately.
	seenTables  map[antlr.Tree]bool
	seenColumns map[antlr.Tree]bool
	seenStars   map[antlr.Tree]bool
	seenNames   map[antlr.Tree]bool

	// root is the statement as parsed, for the passes that need the tree again
	// rather than what the descent made of it.
	root antlr.Tree

	rewriter *antlr.TokenStreamRewriter
	// replaced records the items already spliced, so two hidden columns in one
	// expression replace their item once rather than twice.
	replaced map[antlr.Tree]bool
}

// scope is one query's view of the names it may use: what its FROM brought in,
// under the name it can be qualified with, and the names a WITH introduced,
// which look exactly like tables and are not.
type scope struct {
	parent *scope

	sources map[string]*source
	order   []*source

	// ctes are the names a WITH introduced that are visible here. A name in this
	// set is not a table and must not be decided about as one.
	ctes map[string]bool
}

// source is one thing a FROM brought into scope.
type source struct {
	// table is the base table's name, empty when this is a derived table, a CTE,
	// or anything else without one.
	table string
	// alias is what it can be qualified with here: the alias when there is one,
	// and the table's own name otherwise.
	alias string
}

// tableRef is one table reference, and the scope it was written in.
type tableRef struct {
	node  tsql.IFull_table_nameContext
	scope *scope
}

// columnRef is one column reference, the scope it was written in, and whether
// this query RETURNS it.
//
// returned is the whole difference between a column that gets replaced and one
// left exactly as it was. It belongs to the item and to the query that item is
// in: a column read in the WHERE of a subquery that happens to sit inside
// somebody else's select item is not being returned by that item, and deciding
// otherwise would blank a count nobody asked to hide.
type columnRef struct {
	node     tsql.IFull_column_nameContext
	scope    *scope
	returned bool
	// item is the element that would return it, and is what gets replaced.
	item antlr.ParserRuleContext
	// hidden is what decide() made of it, and name is what the replacement is
	// called when nothing else names it.
	hidden bool
	name   string
}

// walkColumns visits every column reference under a node. Used where position
// no longer matters and only presence does.
func walkColumns(node antlr.Tree, visit func(tsql.IFull_column_nameContext)) {
	if node == nil {
		return
	}
	walk(node, func(n antlr.Tree) {
		if col, ok := n.(tsql.IFull_column_nameContext); ok {
			visit(col)
		}
	})
}

// starRef is a star in something that is returned.
type starRef struct {
	node  tsql.IAsteriskContext
	scope *scope
	item  antlr.ParserRuleContext
	// feeding marks a star standing for values going INTO a table.
	feeding bool
}

// assignRef is one column a write assigns to.
type assignRef struct {
	node  tsql.IUpdate_elemContext
	scope *scope
}

// push opens a scope.
func (a *analysis) push(parent *scope) *scope {
	s := &scope{parent: parent, sources: map[string]*source{}, ctes: map[string]bool{}}
	a.scopes = append(a.scopes, s)
	return s
}

// refuse records the first thing found wrong, which is what gets reported.
func (a *analysis) refuse(format string, args ...any) {
	if a.reason == "" {
		a.reason = fmt.Sprintf(format, args...)
	}
}

// walk visits every node in a tree, parents before children.
func walk(node antlr.Tree, visit func(antlr.Tree)) {
	visit(node)
	for _, child := range node.GetChildren() {
		walk(child, visit)
	}
}

// text is a node's source text with nothing between the tokens, which is what
// ANTLR gives back. It is used for names and comparisons, never for rebuilding a
// statement: a rewrite splices tokens instead, so everything it does not touch
// keeps the spacing it was written with.
func text(node antlr.Tree) string {
	switch n := node.(type) {
	case antlr.ParserRuleContext:
		return n.GetText()
	case antlr.TerminalNode:
		return n.GetText()
	}
	return ""
}

// ident strips the quoting off a name. T-SQL has three spellings and they mean
// the same thing: [name], "name", and a bare word.
func ident(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 {
		if (raw[0] == '[' && raw[len(raw)-1] == ']') || (raw[0] == '"' && raw[len(raw)-1] == '"') {
			return raw[1 : len(raw)-1]
		}
	}
	return raw
}

// parts splits a table name into the pieces that were written. T-SQL allows up
// to four (server.database.schema.table), and the last one is the table.
func parts(name tsql.IFull_table_nameContext) []string {
	var out []string
	if id := name.Id_(); id != nil {
		out = append(out, ident(text(id)))
	}
	if dd := name.DoubleDotID(); dd != nil {
		// a..b means a, the default schema, b: the empty middle is a real part.
		out = append(out, "", ident(strings.TrimPrefix(text(dd), "..")))
	}
	for _, dot := range name.AllDotID() {
		out = append(out, ident(strings.TrimPrefix(text(dot), ".")))
	}
	return out
}

// tableOf is the table a name refers to, which is the last part of it.
func tableOf(name tsql.IFull_table_nameContext) string {
	p := parts(name)
	if len(p) == 0 {
		return ""
	}
	return p[len(p)-1]
}

// pick returns the alias when there is one, and the name otherwise.
func pick(alias, name string) string {
	if alias != "" {
		return alias
	}
	return name
}

// --- the descent ----------------------------------------------------------

// selectStandalone reads a SELECT with the WITH in front of it when there is
// one. The CTEs come first, because the query after them can name them.
func (a *analysis) selectStandalone(node tsql.ISelect_statement_standaloneContext, parent *scope) {
	s := a.push(parent)
	if with := node.With_expression(); with != nil {
		a.withExpression(with, s)
	}
	if sel := node.Select_statement(); sel != nil {
		a.selectStatement(sel, s)
	}
}

// withExpression reads the CTEs, each of which is a query of its own AND a name
// the rest of the statement may use as though it were a table.
func (a *analysis) withExpression(node tsql.IWith_expressionContext, s *scope) {
	for _, cte := range node.AllCommon_table_expression() {
		name := ""
		if id := cte.GetExpression_name(); id != nil {
			name = ident(text(id))
		}
		if name == "" {
			a.refuse("One of the names in the WITH clause could not be read.")
			return
		}
		// A CTE may name itself (a recursive one) and the CTEs before it, so the
		// name is in scope for its own body.
		s.ctes[strings.ToLower(name)] = true
		if query := cte.GetCte_query(); query != nil {
			a.selectStatement(query, s)
		}
	}
}

// selectStatement reads the query and whatever is hung off it.
func (a *analysis) selectStatement(node tsql.ISelect_statementContext, parent *scope) {
	if expr := node.Query_expression(); expr != nil {
		a.queryExpression(expr, parent)
	}
	// ORDER BY sits outside the query specification in this grammar, and what it
	// names is read rather than returned.
	if order := node.Select_order_by_clause(); order != nil {
		a.expression(order, parent, false, nil)
	}
	if forClause := node.For_clause(); forClause != nil {
		a.rolled = append(a.rolled, forClause)
	}
}

// queryExpression reads a query and the ones it is unioned with. Each arm has
// its own FROM and its own select list, and each of them projects.
func (a *analysis) queryExpression(node tsql.IQuery_expressionContext, parent *scope) {
	if spec := node.Query_specification(); spec != nil {
		a.querySpecification(spec, parent)
	}
	for _, inner := range node.AllQuery_expression() {
		a.queryExpression(inner, parent)
	}
	for _, union := range node.AllSql_union() {
		if spec := union.GetSpec(); spec != nil {
			a.querySpecification(spec, parent)
		}
		if op := union.GetOp(); op != nil {
			a.queryExpression(op, parent)
		}
	}
	if order := node.Select_order_by_clause(); order != nil {
		a.expression(order, parent, false, nil)
	}
}

// querySpecification is one SELECT: its FROM first, because the select list is
// resolved against it, then the select list, then everything else.
func (a *analysis) querySpecification(node tsql.IQuery_specificationContext, parent *scope) {
	s := a.push(parent)

	if from := node.From_table_sources(); from != nil {
		if sources := from.Table_sources(); sources != nil {
			a.tableSources(sources, s)
		}
	}

	// SELECT ... INTO makes a new table out of the answer. It is a write, and one
	// this package could not hold to anything: the table it makes is not in the
	// catalog, has no policy of its own, and would hold whatever was selected.
	if into := node.GetInto(); into != nil {
		a.seenNames[into] = true
		a.refuse("You are not permitted to run SELECT ... INTO, which makes a new table out of the answer.")
		return
	}

	if list := node.Select_list(); list != nil {
		for _, item := range list.AllSelect_list_elem() {
			a.expression(item, s, true, item)
		}
	}

	if where := node.GetWhere(); where != nil {
		a.expression(where, s, false, nil)
	}
	if group := node.Group_by_clause(); group != nil {
		a.expression(group, s, false, nil)
	}
	if having := node.Having_clause(); having != nil {
		a.expression(having, s, false, nil)
	}
	if top := node.GetTop(); top != nil {
		a.expression(top, s, false, nil)
	}
}

// tableSources reads a FROM.
func (a *analysis) tableSources(node tsql.ITable_sourcesContext, s *scope) {
	for _, src := range node.AllTable_source() {
		a.tableSource(src, s)
	}
}

func (a *analysis) tableSource(node tsql.ITable_sourceContext, s *scope) {
	if item := node.Table_source_item(); item != nil {
		a.tableSourceItem(item, s)
	}
	for _, join := range node.AllJoin_part() {
		// PIVOT and UNPIVOT are not joins, whatever the grammar files them under.
		// They INVENT columns: PIVOT (MAX(ssn) FOR id IN ([1],[2])) produces columns
		// called [1] and [2] whose values are the hidden field, and the select list
		// above names neither ssn nor customers. There is no reference left to
		// replace, and nothing here could work out what the new columns hold
		// without modelling the operator itself. Refused, the same way everything
		// this package cannot account for is refused.
		if join.Pivot() != nil || join.Unpivot() != nil {
			a.refuse("You are not permitted to run PIVOT or UNPIVOT: what the columns they make would hold cannot be established.")
			return
		}
		// A join brings in another source and carries a condition. The sources go
		// into the same scope; the condition's columns are read, not returned.
		walk(join, func(n antlr.Tree) {
			if inner, ok := n.(tsql.ITable_source_itemContext); ok {
				a.tableSourceItem(inner, s)
			}
		})
		a.expression(join, s, false, nil)
	}
}

// tableSourceItem is one thing in a FROM: a table, a derived table, or a name a
// WITH introduced.
func (a *analysis) tableSourceItem(node tsql.ITable_source_itemContext, s *scope) {
	alias := ""
	if as := node.As_table_alias(); as != nil {
		if t := as.Table_alias(); t != nil {
			alias = ident(text(t))
		}
	}

	if name := node.Full_table_name(); name != nil {
		a.seenTables[name] = true
		table := tableOf(name)
		// A name a WITH introduced looks exactly like a table and is not one.
		if len(parts(name)) == 1 && a.knowsCTE(s, table) {
			a.bind(s, &source{alias: pick(alias, table)})
			return
		}
		a.tables = append(a.tables, tableRef{node: name, scope: s})
		a.bind(s, &source{table: table, alias: pick(alias, table)})
		return
	}

	if derived := node.Derived_table(); derived != nil {
		// A derived table is a query, and its columns were decided where they were
		// read. What it contributes upward is a name with no table behind it.
		a.expression(derived, s, false, nil)
		a.bind(s, &source{alias: alias})
		return
	}

	// Everything else a FROM can hold: a function call, OPENROWSET, OPENQUERY,
	// OPENJSON, OPENXML, a table variable, a change table. None is a table this
	// package can resolve, and several of them reach a database outside this
	// connection entirely. Refused rather than bound as something opaque, because
	// something opaque in a FROM is a hole: a star over it cannot be expanded,
	// and an unqualified column could be resolved to the wrong place.
	a.refuse("One of the things in the FROM clause could not be established. Read from tables and views by name.")
}

// bind puts a source in scope under the name it can be qualified with.
func (a *analysis) bind(s *scope, src *source) {
	if key := strings.ToLower(src.alias); key != "" {
		s.sources[key] = src
	}
	s.order = append(s.order, src)
}

// knowsCTE reports whether a name was introduced by a WITH anywhere up the scope
// chain.
func (a *analysis) knowsCTE(s *scope, name string) bool {
	for at := s; at != nil; at = at.parent {
		if at.ctes[strings.ToLower(name)] {
			return true
		}
	}
	return false
}

// expression reads anything that is not a clause of its own: a WHERE, a select
// item, a join condition, an ORDER BY. It records every column and star it
// finds, with whether this query returns it, and hands a subquery back to the
// query reader, where returned starts again as false.
func (a *analysis) expression(node antlr.Tree, s *scope, returned bool, item antlr.ParserRuleContext) {
	if node == nil || a.reason != "" {
		return
	}
	switch n := node.(type) {
	case tsql.ISubqueryContext:
		// A subquery is a query. What it returns belongs to it, not to the item
		// it happens to sit inside.
		if sel := n.Select_statement(); sel != nil {
			a.selectStatement(sel, s)
		}
		return
	case tsql.IFull_column_nameContext:
		a.seenColumns[n] = true
		// The qualifier inside a column reference is not a table reference: it
		// names something already in scope. Accounted for here so the sweep does
		// not read it as a table nobody decided about.
		if q := n.Full_table_name(); q != nil {
			a.seenTables[q] = true
		}
		a.columns = append(a.columns, columnRef{node: n, scope: s, returned: returned, item: item})
		return
	case tsql.IAsteriskContext:
		a.seenStars[n] = true
		if q := n.Table_name(); q != nil {
			a.seenNames[q] = true
		}
		if returned {
			a.stars = append(a.stars, starRef{node: n, scope: s, item: item, feeding: a.feeding > 0})
		}
		return
	}
	for _, child := range node.GetChildren() {
		a.expression(child, s, returned, item)
	}
}

// --- writes ---------------------------------------------------------------

// insert reads an INSERT. Its rows may come from a query, and that query is an
// ordinary read but for one thing: a star in it must not be masked, because what
// a star stands for there is values going IN.
func (a *analysis) insert(node tsql.IInsert_statementContext) {
	s := a.push(nil)
	if with := node.With_expression(); with != nil {
		a.withExpression(with, s)
	}
	if obj := node.Ddl_object(); obj != nil {
		a.target(obj, s)
	}
	if node.Rowset_function_limited() != nil {
		a.refuse("What this statement writes to could not be established. Write to tables by name.")
		return
	}
	if cols := node.Insert_column_name_list(); cols != nil {
		a.expression(cols, s, false, nil)
	}
	if out := node.Output_clause(); out != nil {
		a.outputClause(out, s)
	}
	if values := node.Insert_statement_value(); values != nil {
		a.feeding++
		a.expression(values, s, false, nil)
		a.feeding--
	}
}

// update reads an UPDATE: what it writes, what it reads to decide, and what it
// hands back.
func (a *analysis) update(node tsql.IUpdate_statementContext) {
	s := a.push(nil)
	if with := node.With_expression(); with != nil {
		a.withExpression(with, s)
	}
	if obj := node.Ddl_object(); obj != nil {
		a.target(obj, s)
	}
	if node.Rowset_function_limited() != nil {
		a.refuse("What this statement writes to could not be established. Write to tables by name.")
		return
	}
	if from := node.Table_sources(); from != nil {
		a.tableSources(from, s)
	}
	for _, elem := range node.AllUpdate_elem() {
		a.assignments = append(a.assignments, assignRef{node: elem, scope: s})
		a.expression(elem, s, false, nil)
	}
	if out := node.Output_clause(); out != nil {
		a.outputClause(out, s)
	}
	if where := node.Search_condition(); where != nil {
		a.expression(where, s, false, nil)
	}
}

// del reads a DELETE.
func (a *analysis) del(node tsql.IDelete_statementContext) {
	s := a.push(nil)
	if with := node.With_expression(); with != nil {
		a.withExpression(with, s)
	}
	if obj := node.Delete_statement_from(); obj != nil {
		a.target(obj, s)
	}
	if from := node.From_table_sources(); from != nil {
		if sources := from.Table_sources(); sources != nil {
			a.tableSources(sources, s)
		}
	}
	if out := node.Output_clause(); out != nil {
		a.outputClause(out, s)
	}
	if where := node.Search_condition(); where != nil {
		a.expression(where, s, false, nil)
	}
}

// target is the table a write writes to. It is a table reference like any other,
// and the policy decides it the same way.
func (a *analysis) target(node antlr.Tree, s *scope) {
	found := false
	walk(node, func(n antlr.Tree) {
		name, ok := n.(tsql.IFull_table_nameContext)
		if !ok {
			return
		}
		found = true
		a.seenTables[name] = true
		table := tableOf(name)
		if len(parts(name)) == 1 && a.knowsCTE(s, table) {
			a.bind(s, &source{alias: table})
			return
		}
		a.tables = append(a.tables, tableRef{node: name, scope: s})
		a.bind(s, &source{table: table, alias: table})
	})
	if !found {
		a.refuse("What this statement writes to could not be established. Write to tables by name.")
	}
}

// outputClause is a projection standing somewhere other than a select list.
// Everything true of a select list is true of it: DELETE ... OUTPUT deleted.ssn
// hands back the value exactly as SELECT ssn would.
func (a *analysis) outputClause(node tsql.IOutput_clauseContext, s *scope) {
	if node.INTO() != nil {
		// OUTPUT ... INTO writes the rows somewhere instead of handing them back.
		// Masking would put the stand-in into a real table, which is the control
		// destroying data rather than protecting it.
		if into := node.Table_name(); into != nil {
			a.seenNames[into] = true
		}
		a.refuse("You are not permitted to run OUTPUT ... INTO, which writes the answer into another table.")
		return
	}
	for _, elem := range node.AllOutput_dml_list_elem() {
		a.expression(elem, s, true, elem)
	}
}

// --- the sweep ------------------------------------------------------------

// sweep is the second pass, and the reason the first one can be trusted.
//
// It visits every node in the tree, including shapes this package has never
// heard of, and asks one question of every table, column and star it finds: did
// the descent account for you? A reference the descent never visited is one that
// nothing decided about, which is exactly how a hidden column would be returned
// through a corner of the grammar nobody thought to walk. That is not
// hypothetical: it is how NATURAL JOIN and DEFAULT(col) got through on the MySQL
// side during its audit.
//
// The Postgres side does this by protobuf reflection over a typed AST. Here the
// tree is ANTLR contexts, which is if anything simpler: every node is a node, and
// a type assertion finds the kinds that matter wherever they are nested.
func (a *analysis) sweep(root antlr.Tree) {
	walk(root, func(node antlr.Tree) {
		if a.reason != "" {
			return
		}
		switch ref := node.(type) {
		case tsql.IFull_table_nameContext:
			if !a.seenTables[ref] {
				a.refuse("What %q is in this statement could not be established.", text(ref))
			}
		case tsql.IFull_column_nameContext:
			if !a.seenColumns[ref] {
				a.refuse("What %q refers to in this statement could not be established.", text(ref))
			}
		case tsql.IAsteriskContext:
			if !a.seenStars[ref] {
				a.refuse("What * stands for here could not be established. Name the columns you want.")
			}
		case tsql.ITable_nameContext:
			if !a.seenNames[ref] {
				a.refuse("What %q is in this statement could not be established.", text(ref))
			}
		case tsql.IScalar_function_nameContext:
			a.routineCall(ref)
		}
	})
}
