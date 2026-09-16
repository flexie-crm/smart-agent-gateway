package postgres

import (
	"fmt"
	"strings"

	"flexie.io/sag/internal/sqlguard"
	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// How this reads a statement, and why it reads it twice.
//
// Deciding needs POSITION, not just presence: a column read in a select list is
// returned and one read in a WHERE is not, and a name in a FROM might be a table
// or might be something a WITH introduced. Position comes from walking the
// shapes this package understands, by hand, which is the first pass.
//
// A hand-written walk has one failure mode, and it is the one that matters: a
// shape nobody thought of is not visited, so what it names is never decided
// about and passes silently. That is how NATURAL JOIN and DEFAULT() got through
// on the MySQL side. So there is a second pass, over the SAME tree, by protobuf
// reflection: it visits every message of every type, including ones a later
// PostgreSQL adds, and asks one question of each table and column it finds, did
// the first pass account for you? Anything unaccounted for refuses the
// statement.
//
// The first pass is what decides. The second is what makes trusting the first
// pass reasonable.

// catalogTables are the parts of information_schema a governed tool may read.
// Each of them carries a table_name, and that is the condition for being on the
// list rather than a coincidence: a read of one is narrowed by adding a
// condition on that column, so a view with no such column could not be narrowed
// at all and does not belong here.
//
// The rest of information_schema, and the whole of pg_catalog, are refused: they
// hold view bodies and routine sources, and pg_catalog names tables through
// joins on oids that nothing here could narrow honestly.
var catalogTables = map[string]bool{
	"tables":            true,
	"columns":           true,
	"table_constraints": true,
	"key_column_usage":  true,
}

type analysis struct {
	// hiddenFields are the fields this statement would have had replaced with
	// the placeholder, so a refusal about a routine can name them.
	hiddenFields []string
	guard        *sqlguard.Guard

	// reason is the first thing found wrong, which is what gets reported.
	reason string

	write      bool
	changed    bool
	sawUnknown bool

	// feeding counts how deep this is inside the query whose rows an INSERT
	// writes. A star there stands for values going into a table rather than
	// values coming back, which is the one place a star cannot be masked.
	feeding int

	scopes []*scope

	tables   []tableRef
	columns  []columnRef
	assigns  []assignRef
	inserts  []insertRef
	byColumn map[*pgq.ColumnRef]columnRef

	// seen records the tables and columns the first pass accounted for, so the
	// second can tell whether anything went unvisited.
	seenTables  map[*pgq.RangeVar]bool
	seenColumns map[*pgq.ColumnRef]bool
}

// scope is one query's view of the names it may use: what its FROM brought in,
// under the name it can be qualified with, and the names a WITH introduced,
// which look like tables and are not.
type scope struct {
	parent *scope
	// sel is the SELECT this scope belongs to, when it belongs to one. A write's
	// scope has none.
	sel *pgq.SelectStmt
	// returns points at the list a write hands rows back with. A RETURNING is a
	// select list standing somewhere else, and everything that is true of a
	// select list is true of it.
	returns *[]*pgq.Node
	// feeds marks a query whose rows are being written rather than returned.
	feeds   bool
	ctes    map[string]bool
	sources map[string]*source
	order   []*source
}

// projection is what this query hands back, whether it says so with a select
// list or with a RETURNING.
func (s *scope) projection() []*pgq.Node {
	switch {
	case s.sel != nil:
		return s.sel.GetTargetList()
	case s.returns != nil:
		return *s.returns
	}
	return nil
}

// setProjection writes a rebuilt list of returned items back where it came from.
func (s *scope) setProjection(list []*pgq.Node) {
	switch {
	case s.sel != nil:
		s.sel.TargetList = list
	case s.returns != nil:
		*s.returns = list
	}
}

// source is one entry in a FROM.
type source struct {
	alias string
	// table is the database's own name for it, when it is a real table.
	table string
	// derived marks a subquery, a name a WITH introduced, or a function in a
	// FROM. Its columns were decided where they were read, if they were read at
	// all, so nothing is hidden a second time.
	derived bool
	// catalog names the information_schema table this is, when it is one.
	catalog string
	// query is the subquery behind a derived source, for expanding a star.
	query *pgq.SelectStmt
}

type tableRef struct {
	rel   *pgq.RangeVar
	scope *scope
}

type columnRef struct {
	// ref is the node in the tree, which is what the sweep checks against.
	ref   *pgq.ColumnRef
	scope *scope
	// target is the select item this column is part of, when it is part of one
	// belonging to its own query. That is the one position where a hidden field
	// is returned rather than merely used.
	target *pgq.ResTarget

	// What the reference means, worked out where it was found rather than read
	// off the node again later: (c).ssn names a column without the node saying
	// so, and c on its own names a whole row rather than a column at all.
	qualifier string
	column    string
	star      bool
	// whole marks a reference to an entire ROW: SELECT c FROM customers c hands
	// back every column of c at once, hidden ones included, without naming one.
	whole bool
}

// assignRef is one SET of an UPDATE: the column being written into, and the
// value written into it.
type assignRef struct {
	target *pgq.ResTarget
	scope  *scope
}

// insertRef is one INSERT: where its rows are going, and the columns it named,
// which may be none at all.
type insertRef struct {
	stmt  *pgq.InsertStmt
	scope *scope
}

func (a *analysis) refuse(format string, args ...any) {
	if a.reason == "" {
		a.reason = fmt.Sprintf(format, args...)
	}
}

// sweep is the second pass: every table and column in the tree, by reflection,
// checked against what the first pass accounted for.
func (a *analysis) sweep(tree *pgq.ParseResult) {
	a.seenTables = map[*pgq.RangeVar]bool{}
	a.seenColumns = map[*pgq.ColumnRef]bool{}
	for _, ref := range a.tables {
		a.seenTables[ref.rel] = true
	}
	for _, ref := range a.columns {
		a.seenColumns[ref.ref] = true
	}

	walk(tree.ProtoReflect(), func(m protoreflect.Message) {
		if a.reason != "" {
			return
		}
		switch msg := m.Interface().(type) {
		case *pgq.RangeVar:
			if !a.seenTables[msg] {
				a.refuse("Where %q sits in that statement could not be established. "+
					"Write the query a plainer way.", msg.GetRelname())
			}
		case *pgq.ColumnRef:
			if !a.seenColumns[msg] {
				a.refuse("Where a column sits in that statement could not be established. " +
					"Write the query a plainer way.")
			}
		case *pgq.FuncCall:
			a.routineCall(msg)
		}
	})
}

// routineCall decides a stored function invoked inside a statement.
//
// CALL announces itself and a function call does not: `SELECT f()` is a SELECT,
// and the body it runs is the database's own code, free to read whatever the
// policy keeps back. Measured before it was written: with a policy hiding
// customers.ssn, a function returning that column handed it straight over.
//
// It rides the sweep rather than the descent because a call can sit anywhere an
// expression can, and the sweep is the pass that visits everything. Only a name
// the catalog KNOWS is a routine is decided about: everything else is a built-in,
// and refusing lower() would refuse every statement worth running.
func (a *analysis) routineCall(call *pgq.FuncCall) {
	names := call.GetFuncname()
	if len(names) == 0 {
		return
	}
	name, ok := routineName(call)
	if !ok {
		return
	}
	bare := name
	if dot := strings.LastIndex(bare, "."); dot >= 0 {
		bare = bare[dot+1:]
	}
	if takesStatementAsText[bare] {
		a.refuse("You are not permitted to run %s, which is handed what to read as text rather than "+
			"as part of the statement.", bare)
		return
	}
	if !a.guard.IsRoutine(name) {
		return
	}
	if ok, why := a.guard.CallAllowed(name); !ok {
		a.refuse("%s", why)
	}
}

// routineName is what a call NAMES, as the guard reads a routine's name: the
// whole of it, schema and all, because a routine's identity is its schema and
// its name and two schemas are free to offer the same one.
//
// It reports false for a call that names nothing, and for the one shape that
// carries a name NOBODY WROTE. Standard SQL has syntax for a handful of
// functions, and this parser rewrites each into the pg_catalog function that
// implements it: SUBSTRING(x FROM 1 FOR 2) becomes pg_catalog.substring,
// EXTRACT becomes pg_catalog.extract, TRIM becomes pg_catalog.btrim, and so do
// POSITION, OVERLAY, NORMALIZE, XMLEXISTS and COLLATION FOR. The rule that a
// QUALIFIED name is stored code whatever the snapshot knows then refused every
// one of them, so a policy with a table rule on it could not run SUBSTRING:
// measured, all seven came back "this tool cannot establish what
// pg_catalog.substring does".
//
// The parser marks its own rewrites, and that is the discriminator rather than a
// list of names to keep up to date. It cannot be forged either: the only way to
// get this mark is to write one of the standard forms, and each of those maps to
// one fixed function. A pg_catalog name somebody writes out in full keeps the
// ordinary rule and is still refused.
func routineName(call *pgq.FuncCall) (string, bool) {
	if call.GetFuncformat() == pgq.CoercionForm_COERCE_SQL_SYNTAX {
		return "", false
	}
	var parts []string
	for _, n := range call.GetFuncname() {
		if str := n.GetString_(); str != nil && str.GetSval() != "" {
			parts = append(parts, strings.ToLower(str.GetSval()))
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "."), true
}

// takesStatementAsText is every built-in that is handed a query, a table, a
// schema or the whole database as a VALUE, and reads it.
//
// These are the one thing no parser can help with. The parser reads this
// statement correctly and completely: a function call, with a string constant as
// its argument. A string is data. `query_to_xml('SELECT ssn FROM customers')`
// has no table in it and no column in it, because there is nothing there but
// characters, which the function decides at RUN TIME to treat as SQL. So the
// statement this side reads and the statement the server runs are two different
// statements, which is precisely the disagreement this package exists to
// prevent. Measured against a live server before this was written: the hidden
// field and the denied table both came back.
//
// The other two dialects already close the same door, by name, for the same
// reason: the T-SQL analyzer refuses EXEC('...') and sp_executesql, and the
// MySQL one refuses PREPARE and EXECUTE. This is that rule, finally written
// here.
//
// Refused rather than read. Parsing the inner string and holding it to the
// policy would work for the query_ ones, and it is not worth the machinery: this
// is an XML export facility from 2008 that an assistant answering questions
// about somebody's data has no reason to reach for. If a customer ever needs it,
// the honest answer is to read the argument, not to widen this.
//
// What this does NOT cover is a function an extension adds later with the same
// shape, and there is no structural signal to catch one: an argument of type
// regclass is a hint, not a rule, and the catalog this package holds does not
// carry function signatures. The backstop is the property test, which asserts
// that no statement this guard allows ever returns the secret.
var takesStatementAsText = map[string]bool{
	// A query, as a string.
	"query_to_xml": true, "query_to_xmlschema": true, "query_to_xml_and_xmlschema": true,
	// A table, as a value.
	"table_to_xml": true, "table_to_xmlschema": true, "table_to_xml_and_xmlschema": true,
	// A whole schema, and the whole database.
	"schema_to_xml": true, "schema_to_xmlschema": true, "schema_to_xml_and_xmlschema": true,
	"database_to_xml": true, "database_to_xmlschema": true, "database_to_xml_and_xmlschema": true,
	// A cursor, which is a query somebody declared earlier.
	"cursor_to_xml": true, "cursor_to_xmlschema": true,
}

// walk visits every message in a tree. Nothing is named here, so a node type
// nobody thought of is still visited.
func walk(m protoreflect.Message, visit func(protoreflect.Message)) {
	visit(m)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			if fd.Kind() != protoreflect.MessageKind {
				return true
			}
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				walk(list.Get(i).Message(), visit)
			}
		case fd.Kind() == protoreflect.MessageKind:
			walk(v.Message(), visit)
		}
		return true
	})
}

// aliasOf is what a source can be qualified with: its alias, or its own name.
func aliasOf(rel *pgq.RangeVar) string {
	if a := rel.GetAlias(); a != nil && a.GetAliasname() != "" {
		return strings.ToLower(a.GetAliasname())
	}
	return strings.ToLower(rel.GetRelname())
}

// columnParts splits a column reference into its qualifier and its name. A
// reference is a list of strings (schema, table, column), with a star for a
// wildcard.
func columnParts(ref *pgq.ColumnRef) (qualifier, column string, star bool) {
	var parts []string
	for _, f := range ref.GetFields() {
		switch v := f.Node.(type) {
		case *pgq.Node_String_:
			parts = append(parts, v.String_.GetSval())
		case *pgq.Node_AStar:
			star = true
		}
	}
	switch {
	case star && len(parts) > 0:
		return strings.ToLower(parts[len(parts)-1]), "", true
	case star:
		return "", "", true
	case len(parts) == 1:
		return "", parts[0], false
	default:
		return strings.ToLower(parts[len(parts)-2]), parts[len(parts)-1], false
	}
}
