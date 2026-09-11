package mysql

import (
	"fmt"
	"strings"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// catalogTables are the parts of the database's own catalog a governed tool may
// read. They describe tables and columns, and every one of them carries the
// table's name, so a read of them can be narrowed to the tables this tool may
// see. The rest of the catalog is refused: some of it holds the text of a view's
// definition or a routine's body, which names tables the policy exists to hide.
var catalogTables = map[string]bool{
	"tables":            true,
	"columns":           true,
	"statistics":        true,
	"key_column_usage":  true,
	"table_constraints": true,
}

// analysis is one reading of one statement.
type analysis struct {
	guard *sqlguard.Guard

	// reason is the first thing found wrong, which is what gets reported: a
	// person reading a refusal wants to know what to fix first, not last.
	reason string

	write       bool
	listsTables bool
	// sawUnknown marks a table this snapshot does not have, which is a reason to
	// take a fresh one rather than to decide from an old one.
	sawUnknown bool
	changed    bool

	stack  []*scope
	scopes []*scope
	byNode map[ast.Node]*scope

	tables  []tableRef
	columns []columnRef
	// joins counts how deep we are inside a join's condition. A hidden field may
	// be joined on: an administrator hides what a value IS, not which rows line
	// up with which, and joining two tables on an identifier neither side may
	// print is an ordinary thing to want. See the note on decideColumn.
	joins int
	// joinKeys are the columns named by a USING clause, which sit on the join
	// rather than inside its condition and are the same thing by another spelling.
	joinKeys map[*ast.ColumnName]bool
	// inserts are the rows going into a table, which have to be decided about
	// even when they name no column at all.
	inserts []*ast.InsertStmt
	// assigns are the SET parts of a write. They have no select list to replace a
	// value in, so they are decided separately from everything else.
	assigns []*ast.Assignment
	// fields is the stack of select items being walked, innermost last, each
	// with the query it belongs to. A hidden field is replaced where it is
	// RETURNED, so what matters about a reference to one is whether it is part
	// of ITS OWN query's projection: a column read in the WHERE of a subquery
	// that sits inside somebody else's select item is not being returned, and
	// replacing that select item would blank an answer nobody asked to hide.
	fields []fieldFrame
}

// scope is one query's view of the names it may use: what its FROM brought in,
// under the name it can be qualified with; the names a WITH introduced, which
// look like tables and are not; and the columns it selects plainly, which is the
// one position a hidden column is allowed to appear in.
type scope struct {
	parent *scope
	node   ast.Node
	sel    *ast.SelectStmt
	ctes   map[string]bool
	// sources is for looking a qualifier up; order is every source the FROM
	// brought in, which is not the same thing. Two sources can answer to one
	// name (the database refuses such a statement, but it reaches here first),
	// and keeping only one of them would leave the other unaccounted for while
	// looking for all the world like it had been checked.
	sources map[string]*source
	order   []*source
}

// source is one entry in a FROM.
type source struct {
	alias string
	// table is the database's own name for it, when it is a real table.
	table string
	// derived marks a subquery or a name a WITH introduced. Its columns were
	// already decided where they were read, so nothing is hidden a second time.
	derived bool
	// catalog marks the database's own catalog, which holds no data of its own
	// to hide, and is narrowed instead.
	catalog      string
	derivedQuery ast.Node
}

type tableRef struct {
	name  *ast.TableName
	scope *scope
}

type fieldFrame struct {
	field *ast.SelectField
	scope *scope
}

type columnRef struct {
	name  *ast.ColumnName
	scope *scope
	// joined marks a column read inside a join's condition.
	joined bool
	// field is the select item this column is part of, if any. It is the
	// innermost one: a column inside a subquery in a select list belongs to that
	// subquery's own projection, which is where it has to be replaced.
	field *ast.SelectField
}

func (a *analysis) refuse(format string, args ...any) {
	if a.reason == "" {
		a.reason = fmt.Sprintf(format, args...)
	}
}

// field is the select item currently being walked, when it belongs to the query
// currently being walked. Outside one, or inside a subquery that is only part of
// somebody else's select item, there is nothing here being returned.
func (a *analysis) field() *ast.SelectField {
	if len(a.fields) == 0 {
		return nil
	}
	top := a.fields[len(a.fields)-1]
	if top.scope != a.top() {
		return nil
	}
	return top.field
}

func (a *analysis) top() *scope {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1]
}

// Enter and Leave walk the whole statement. Collection is done here, over every
// node the statement has, rather than by walking the clauses this package knows
// about: a table or a column in a corner nobody thought of is then still seen,
// and is decided like any other.
func (a *analysis) Enter(n ast.Node) (ast.Node, bool) {
	switch node := n.(type) {
	case *ast.SelectStmt:
		if node.SelectIntoOpt != nil {
			a.refuse("this tool returns rows to you; it does not write them to a file on the server")
		}
		// TABLE t and VALUES ... are a select by another spelling, and they are
		// not written back as one: the keyword is all that comes out, so a select
		// list this package masked a column in, or narrowed a catalog read with,
		// would be dropped between the decision and the database. What cannot be
		// written back the way it was decided about does not run.
		if node.Kind != ast.SelectStmtKindSelect {
			a.refuse("write that as SELECT * FROM the table; this tool does not run the short form")
		}
		a.push(node, node.With, node.From)
	case *ast.SetOprStmt:
		a.push(node, node.With, nil)
	case *ast.InsertStmt:
		// An insert has no WITH of its own: one written in front of the rows it
		// selects belongs to that select, and is pushed with it.
		a.inserts = append(a.inserts, node)
		a.push(node, nil, node.Table)
	case *ast.UpdateStmt:
		a.push(node, node.With, node.TableRefs)
	case *ast.DeleteStmt:
		a.push(node, node.With, node.TableRefs)
	case *ast.Join:
		// USING (col) names the join key on the join itself rather than in a
		// condition, so it is recorded here to be read the same way.
		for _, using := range node.Using {
			if a.joinKeys == nil {
				a.joinKeys = map[*ast.ColumnName]bool{}
			}
			a.joinKeys[using] = true
		}
	case *ast.Assignment:
		a.assigns = append(a.assigns, node)
	case *ast.SelectField:
		a.fields = append(a.fields, fieldFrame{field: node, scope: a.top()})
	case *ast.OnCondition:
		a.joins++
	case *ast.DefaultExpr:
		// The walk does not descend into this one, so its column would otherwise
		// go unseen entirely.
		if node.Name != nil {
			a.columns = append(a.columns, columnRef{name: node.Name, scope: a.top(), joined: a.joins > 0, field: a.field()})
		}
	case *ast.TableName:
		a.tables = append(a.tables, tableRef{name: node, scope: a.top()})
	case *ast.ColumnName:
		a.columns = append(a.columns, columnRef{name: node, scope: a.top(), joined: a.joins > 0, field: a.field()})
	case *ast.VariableExpr:
		// A variable is a place to put a value in one statement and read it back
		// in another, which is a way around every rule below.
		a.refuse("this tool does not run statements that set or read a variable")
	case *ast.FuncCallExpr:
		if node.FnName.L == "load_file" {
			a.refuse("this tool reads the database, not files on the server")
		}
	}
	return n, false
}

func (a *analysis) Leave(n ast.Node) (ast.Node, bool) {
	if _, ok := n.(*ast.OnCondition); ok {
		a.joins--
	}
	if f, ok := n.(*ast.SelectField); ok && len(a.fields) > 0 && a.fields[len(a.fields)-1].field == f {
		a.fields = a.fields[:len(a.fields)-1]
	}
	if s := a.top(); s != nil && s.node == n {
		a.stack = a.stack[:len(a.stack)-1]
	}
	return n, true
}

func (a *analysis) push(node ast.Node, with *ast.WithClause, from *ast.TableRefsClause) {
	s := &scope{
		parent:  a.top(),
		node:    node,
		ctes:    map[string]bool{},
		sources: map[string]*source{},
	}
	if sel, ok := node.(*ast.SelectStmt); ok {
		s.sel = sel
	}
	if with != nil {
		for _, cte := range with.CTEs {
			s.ctes[strings.ToLower(cte.Name.O)] = true
		}
	}
	if from != nil {
		a.sources(s, from.TableRefs)
	}
	a.stack = append(a.stack, s)
	a.scopes = append(a.scopes, s)
	if a.byNode == nil {
		a.byNode = map[ast.Node]*scope{}
	}
	a.byNode[node] = s
}

// sources reads a FROM into the scope. A shape it does not recognise is refused
// rather than skipped: an entry nobody read is an entry nobody checked.
func (a *analysis) sources(s *scope, n ast.ResultSetNode) {
	switch v := n.(type) {
	case nil:
	case *ast.Join:
		a.sources(s, v.Left)
		if v.Right != nil {
			a.sources(s, v.Right)
		}
	case *ast.TableSource:
		a.source(s, v)
	default:
		a.refuse("this tool could not work out which tables that statement reads, so it did not run it")
	}
}

func (a *analysis) source(s *scope, ts *ast.TableSource) {
	src := &source{alias: strings.ToLower(ts.AsName.O)}
	switch inner := ts.Source.(type) {
	case *ast.TableName:
		name := inner.Name.O
		if src.alias == "" {
			src.alias = strings.ToLower(name)
		}
		switch {
		case isCatalogSchema(inner.Schema.O):
			src.catalog = strings.ToLower(name)
		case a.knowsCTE(s, inner):
			src.derived = true
		default:
			// Keep the database's own spelling, so everything downstream compares
			// one canonical name rather than whatever the statement typed.
			if t, ok := a.guard.Catalog().Lookup(name); ok {
				name = t.Name
			}
			src.table = name
		}
	case *ast.SelectStmt, *ast.SetOprStmt:
		src.derived, src.derivedQuery = true, inner
	default:
		a.refuse("this tool could not work out which tables that statement reads, so it did not run it")
		return
	}
	// Every source is kept, in the order the FROM brought it in. The map is only
	// for looking a qualifier up, and the first source to claim a name keeps it:
	// a statement with two of the same name is one the database refuses anyway,
	// and letting the second quietly replace the first is how a read of the
	// catalog stopped being narrowed at all.
	if src.alias != "" {
		if _, taken := s.sources[src.alias]; !taken {
			s.sources[src.alias] = src
		}
	}
	s.order = append(s.order, src)
}

// knowsCTE reports whether a name is one a WITH introduced, here or in a query
// this one sits inside. Such a name looks exactly like a table and is not one,
// and a WITH in an inner query is invisible to an outer one, which is why this
// walks out through the scopes rather than over a single set of names.
func (a *analysis) knowsCTE(s *scope, name *ast.TableName) bool {
	if name.Schema.O != "" {
		return false
	}
	key := strings.ToLower(name.Name.O)
	for ; s != nil; s = s.parent {
		if s.ctes[key] {
			return true
		}
	}
	return false
}

func isCatalogSchema(schema string) bool {
	return strings.EqualFold(schema, "information_schema")
}

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
	for _, insert := range a.inserts {
		a.decideInsert(insert)
		if a.reason != "" {
			return
		}
	}
	for _, assign := range a.assigns {
		a.decideAssignment(assign)
		if a.reason != "" {
			return
		}
	}
}

// decideAssignment reads one SET of a write.
//
// A SET has no select list, so there is nowhere to put the stand-in: whatever it
// reads goes into the row as it is. Reading a hidden field here would copy the
// real value into a column somebody can read back afterwards, which is returning
// it by a longer route, and the row outlives the answer.
//
// Writing INTO a hidden field is refused for a different reason, and it is not
// about the policy at all: this tool would be storing "[hidden]" over whatever
// was really there.
func (a *analysis) decideAssignment(assign *ast.Assignment) {
	if assign.Column != nil && a.hiddenColumn(assign.Column) {
		a.refuse("%q is hidden in this tool, so it cannot be written to: this tool cannot know what is really there, and writing would put the stand-in over it.", assign.Column.Name.O)
		return
	}
	found := &hiddenReader{a: a}
	if assign.Expr != nil {
		assign.Expr.Accept(found)
	}
	if found.name != "" {
		a.refuse("%q is hidden in this tool, so its value cannot be copied into another column, where it would be readable afterwards. Report that it is not available.", found.name)
	}
}

// hiddenReader finds the first hidden column an expression reads.
type hiddenReader struct {
	a    *analysis
	name string
}

func (h *hiddenReader) Enter(n ast.Node) (ast.Node, bool) {
	if name, ok := n.(*ast.ColumnName); ok && h.name == "" && h.a.hiddenColumn(name) {
		h.name = name.Name.O
	}
	return n, false
}
func (h *hiddenReader) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// hiddenColumn reports whether a column, read where it stands, is one the policy
// hides. It is the decision decideColumn makes, without the part about what to
// do next.
func (a *analysis) hiddenColumn(name *ast.ColumnName) bool {
	if !a.guard.CouldHide(name.Name.O) {
		return false
	}
	for _, ref := range a.columns {
		if ref.name != name {
			continue
		}
		tables, origin := a.resolveColumn(ref)
		if origin == fromDerived {
			return false
		}
		hidden, agreed := a.verdict(tables, name.Name.O)
		return hidden || !agreed || origin == unresolved
	}
	return false
}

// decideInsert covers the one write that names no column at all. INSERT INTO t
// VALUES (...) writes every column of t in order, so it reaches a hidden field
// without a single reference for the walk above to have seen, and the same
// statement written with its column list would have been refused.
func (a *analysis) decideInsert(insert *ast.InsertStmt) {
	if insert.Table == nil {
		return
	}
	// Named columns say plainly where the rows are going.
	for _, column := range insert.Columns {
		if a.hiddenColumn(column) {
			a.refuse("%q is hidden in this tool, so it cannot be written to: this tool cannot know what is really there, and writing would put the stand-in over it.", column.Name.O)
			return
		}
	}
	if len(insert.Columns) > 0 {
		return
	}
	scope, known := a.byNode[insert]
	if !known {
		return
	}
	for _, src := range scope.order {
		if src.table == "" {
			continue
		}
		columns, ok := a.guard.Catalog().Columns(src.table)
		if !ok {
			continue
		}
		for _, column := range columns {
			if a.guard.MasksColumn(src.table, column) {
				a.refuse("%q is hidden in this tool, and an INSERT without a column list writes every column of %q including that one. Name the columns you mean to write.", column, src.table)
				return
			}
		}
	}
}

func (a *analysis) decideTable(ref tableRef) {
	schema := ref.name.Schema.O
	switch {
	case isCatalogSchema(schema):
		if !catalogTables[strings.ToLower(ref.name.Name.O)] {
			a.refuse("this tool reads only the parts of the catalog that describe tables and columns (tables, columns, statistics, key_column_usage, table_constraints)")
		}
		return
	case schema != "" && !strings.EqualFold(schema, a.guard.Catalog().Database()):
		a.refuse("this tool reaches one database, %q, and nothing outside it", a.guard.Catalog().Database())
		return
	}
	if a.knowsCTE(ref.scope, ref.name) {
		return
	}
	if _, known := a.guard.Catalog().Lookup(ref.name.Name.O); !known {
		a.sawUnknown = true
	}
	if ok, reason := a.guard.Reaches(ref.name.Name.O); !ok {
		a.refuse("%s", withoutRetry(reason))
	}
}

func (a *analysis) decideColumn(ref columnRef) {
	if schema := ref.name.Schema.O; schema != "" && !isCatalogSchema(schema) && !strings.EqualFold(schema, a.guard.Catalog().Database()) {
		a.refuse("this tool reaches one database, %q, and nothing outside it", a.guard.Catalog().Database())
		return
	}
	name := ref.name.Name.O
	// The common case by far: a column no rule mentions needs no working out at
	// all, which is what keeps the expensive half of this rare.
	if !a.guard.CouldHide(name) {
		return
	}
	tables, origin := a.resolveColumn(ref)
	if origin == fromDerived {
		// It came out of a subquery or a WITH, where it was already decided.
		return
	}
	hidden, agreed := a.verdict(tables, name)
	if origin == unresolved || !agreed {
		// Which table it belongs to could not be established, or the tables it
		// might belong to disagree. Nothing is refused for that any more: if the
		// value would be returned it is hidden, and if it would not, it is left
		// where it was. Being wrong towards hiding costs a column in a report;
		// being wrong the other way costs the value.
		if ref.field != nil {
			a.hide(ref.field, name)
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
	//
	// The cost is deliberate and written down (KB/34): a condition over a hidden
	// field answers a question about it, so somebody who asks enough questions
	// can work the value out. Keeping it out of the answer is what this
	// promises; making it unknowable is not, and would mean refusing most of the
	// queries anybody actually wants to run.
	if ref.field == nil {
		return
	}

	// It IS being returned. The whole select item goes, not only the column: an
	// expression that reads it would otherwise hand the value back with a little
	// arithmetic wrapped round it.
	a.hide(ref.field, name)
}

// hide replaces a returned select item with the value that stands for it,
// keeping the name the result would have had so a caller reading the columns
// back still finds the one it asked for.
//
// The name is the one the database would have given the item: its alias, or the
// column's name when it is a plain column, or the text of the expression. That
// last one is why an expression keeps its own text as a name rather than
// becoming the hidden column's: SELECT CONCAT(ssn,'x') comes back under
// CONCAT(ssn,'x'), exactly as it would have without a policy.
func (a *analysis) hide(field *ast.SelectField, name string) {
	if field.AsName.O == "" {
		if column, plain := field.Expr.(*ast.ColumnNameExpr); plain && column.Name != nil {
			field.AsName = ast.NewCIStr(name)
		} else if text, err := restore(field.Expr); err == nil {
			field.AsName = ast.NewCIStr(text)
		} else {
			field.AsName = ast.NewCIStr(name)
		}
	}
	field.Expr = ast.NewValueExpr(sqlguard.Hidden, "", "")
	a.changed = true
}

type columnOrigin int

const (
	fromTable columnOrigin = iota
	fromDerived
	unresolved
)

// resolveColumn works out which table a column reads from, which is what makes a
// rule written for one table's column mean that column and not another table's
// of the same name. What comes back is every table it could belong to: one, for
// a column written with its table or found in only one of them, and several when
// the statement leaves it open. It is the caller that decides whether being left
// open matters, because when every candidate is hidden (or none is) the answer
// is the same either way and nothing has to be resolved at all.
func (a *analysis) resolveColumn(ref columnRef) ([]string, columnOrigin) {
	if qualifier := strings.ToLower(ref.name.Table.O); qualifier != "" {
		for s := ref.scope; s != nil; s = s.parent {
			if src, ok := s.sources[qualifier]; ok {
				if src.derived || src.catalog != "" {
					return nil, fromDerived
				}
				return []string{src.table}, fromTable
			}
		}
		return nil, unresolved
	}

	// Unqualified: it belongs to whichever table in scope has a column of that
	// name, innermost query first, the way the database itself would read it.
	name := ref.name.Name.O
	sawDerived := false
	for s := ref.scope; s != nil; s = s.parent {
		var found []string
		for _, src := range s.order {
			if src.derived || src.catalog != "" {
				sawDerived = true
				continue
			}
			if a.guard.Catalog().HasColumn(src.table, name) {
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
	// Nothing in scope has a column of that name. The database will say so; this
	// side will not pretend to know which table was meant.
	return nil, unresolved
}

// verdict is what the candidate tables say about a column, and whether they say
// the same thing. A self-join gives the same table twice, and a rule written
// without a table gives the same answer for every one of them.
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
