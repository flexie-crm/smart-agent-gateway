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
	guard *sqlguard.Guard

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
				a.refuse("this tool could not work out where %q sits in that statement, so it did not run it. "+
					"Write the query a plainer way.", msg.GetRelname())
			}
		case *pgq.ColumnRef:
			if !a.seenColumns[msg] {
				a.refuse("this tool could not work out where a column sits in that statement, so it did not run it. " +
					"Write the query a plainer way.")
			}
		}
	})
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
