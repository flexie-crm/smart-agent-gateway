package sqlserver

import (
	"fmt"
	"strings"

	"flexie.io/sag/lib/sqlserver/antlr"

	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/lib/sqlserver/tsql"
)

// Writing the statement back out.
//
// This is where this dialect differs most from the other two, and it differs in
// its favour. MySQL and Postgres both rebuild the whole statement from the tree
// and print it again, which means their printers have to be trusted with every
// literal in it: the MySQL side needed a round-trip fidelity check because its
// restorer turned 'a\\b' into 'a\b' and CHAR(65) into a function that does not
// exist.
//
// Here nothing is reprinted. A rewrite REPLACES A RANGE OF TOKENS and leaves
// every other byte of the statement exactly as it was written, spacing, comments,
// casing and all. What cannot be mangled is everything the rewrite did not touch,
// which is almost all of it. What is spliced in is text this package wrote
// itself, and the result is parsed again before it is sent (analyzer.go).

// hidden is what a hidden field's value is replaced with, as a SQL literal. The
// text inside it is the one every dialect uses (sqlguard.Hidden) rather than a
// copy of it, because whoever reads a result back looks for that exact string
// and must not have to know which database answered.
//
// It is a literal, so the shape of the result does not change: the column is
// still there, still named what it was named, and says it is not available.
var hidden = "'" + strings.ReplaceAll(sqlguard.Hidden, "'", "''") + "'"

// rewrite is the second half: what the statement asked for that has to be
// written differently for it to run at all.
func (a *analysis) rewrite() {
	// One rewriter over the stream, shared where a statement is read in pieces.
	// A routine's body is a list of statements read one at a time, and all of
	// their splices have to land in the SAME rewriter or only the last one would
	// come out.
	if a.rewriter == nil {
		a.rewriter = antlr.NewTokenStreamRewriter(a.stream)
	}

	for i := range a.columns {
		a.mask(&a.columns[i])
		if a.reason != "" {
			return
		}
	}
	for _, star := range a.stars {
		a.expandStar(star)
		if a.reason != "" {
			return
		}
	}
	a.narrowCatalog()
}

// mask replaces a returned hidden column with the stand-in.
//
// It is the whole ITEM that is replaced, not the column inside it, and that is
// the point rather than a convenience: CONCAT(ssn,”) would otherwise hand the
// value back with a little arithmetic wrapped round it, and UPPER(ssn) with a
// function call. Replacing the item means nothing that reads the column can
// carry its value outward.
//
// The item keeps the name the database would have given it, so the shape of the
// result does not change: a caller asking for three columns gets three.
func (a *analysis) mask(ref *columnRef) {
	if !ref.hidden || ref.item == nil {
		return
	}
	// An item already replaced by an earlier column in it is not replaced twice.
	// Two hidden columns in one expression are one item.
	if a.replaced[ref.item] {
		return
	}
	a.replaced[ref.item] = true

	name := outputName(ref.item, ref.name)
	replacement := hidden
	if name != "" {
		replacement = hidden + " AS " + quote(name)
	}
	a.rewriter.ReplaceDefault(
		ref.item.GetStart().GetTokenIndex(),
		ref.item.GetStop().GetTokenIndex(),
		replacement,
	)
	a.changed = true
	if ref.name != "" {
		a.hiddenFields = append(a.hiddenFields, ref.name)
	}
}

// hiddenNames is every field this statement would have had replaced, so a
// refusal about a routine can NAME them instead of saying "a field".
func (a *analysis) hiddenNames() []string { return a.hiddenFields }

// outputName is what the database would call this item: the alias when there is
// one, and the column's own name otherwise.
func outputName(item antlr.ParserRuleContext, column string) string {
	var alias string
	switch elem := item.(type) {
	case tsql.ISelect_list_elemContext:
		if expr := elem.Expression_elem(); expr != nil {
			if as := expr.As_column_alias(); as != nil {
				alias = aliasText(as)
			}
			if left := expr.GetLeftAlias(); left != nil {
				// alias = expression, which names the column on the left.
				alias = ident(text(left))
			}
		}
	case tsql.IOutput_dml_list_elemContext:
		if as := elem.As_column_alias(); as != nil {
			alias = aliasText(as)
		}
	}
	if alias != "" {
		return alias
	}
	return column
}

// aliasText is the name out of an AS clause, without the AS and without quotes.
func aliasText(as tsql.IAs_column_aliasContext) string {
	if c := as.Column_alias(); c != nil {
		return ident(text(c))
	}
	return ""
}

// quote writes a name back the way T-SQL delimits one. Brackets, because they
// need no session setting to mean what they say: whether "x" is an identifier
// or a string depends on QUOTED_IDENTIFIER, and [x] is an identifier always.
func quote(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// expandStar turns a star into the columns it stands for, but only where that
// changes something. A query over tables with nothing hidden in them keeps the
// star it was written with, and the statement is sent on untouched.
func (a *analysis) expandStar(star starRef) {
	covered := star.scope.order
	if q := star.node.Table_name(); q != nil {
		name := ""
		if t := q.GetTable(); t != nil {
			name = ident(text(t))
		}
		// inserted.* and deleted.* are every column of the table being written.
		// They are checked before the qualifier is looked up, not after, because
		// the grammar's id_ accepts keywords: deleted.* matches the ordinary
		// "table_name . *" alternative, so Table_name() is set and the pseudo-table
		// accessors are nil. Reading it as a qualifier would refuse a perfectly
		// good OUTPUT clause.
		if !strings.EqualFold(name, "inserted") && !strings.EqualFold(name, "deleted") {
			src := a.sourceNamed(star.scope, name)
			if src == nil {
				a.refuse("What %s stands for could not be established. Name the columns you want.", text(star.node))
				return
			}
			covered = []*source{src}
		}
	}

	needed := false
	for _, src := range covered {
		if src.table == "" {
			continue
		}
		columns, known := a.guard.Catalog().Columns(src.table)
		if known && a.guard.MasksAnyColumn(src.table, columns) {
			needed = true
			break
		}
	}
	if !needed {
		return
	}

	// A star in the rows an INSERT takes is values going IN. Replacing a hidden
	// column there would store the stand-in over the real value, permanently.
	if star.feeding {
		a.refuse("This statement writes rows read with *, and one of those columns is hidden. " +
			"Name the columns you mean to write, leaving the hidden one out.")
		return
	}

	var out []string
	for _, src := range covered {
		if src.table == "" {
			a.refuse("Which columns * stands for could not be established, and one of the tables has a hidden field. Name the columns you want.")
			return
		}
		columns, known := a.guard.Catalog().Columns(src.table)
		if !known {
			a.refuse("Which columns * stands for could not be established, and one of the tables has a hidden field. Name the columns you want.")
			return
		}
		for _, column := range columns {
			if a.guard.MasksColumn(src.table, column) {
				out = append(out, hidden+" AS "+quote(column))
				continue
			}
			out = append(out, quote(src.alias)+"."+quote(column)+" AS "+quote(column))
		}
	}
	if len(out) == 0 {
		a.refuse("Which columns * stands for here could not be established. Name the columns you want.")
		return
	}

	a.rewriter.ReplaceDefault(
		star.node.GetStart().GetTokenIndex(),
		star.node.GetStop().GetTokenIndex(),
		strings.Join(out, ", "),
	)
	a.changed = true
}

// sourceNamed finds what a qualifier names, up the scope chain.
func (a *analysis) sourceNamed(s *scope, name string) *source {
	if name == "" {
		return nil
	}
	for at := s; at != nil; at = at.parent {
		if src, ok := at.sources[strings.ToLower(name)]; ok {
			return src
		}
	}
	return nil
}

// narrowCatalog adds a condition to a read of INFORMATION_SCHEMA, so that it
// answers about the tables this tool may see and no others.
//
// Without it, introspection is the way round the whole policy: a tool that may
// not read payroll will happily tell you it exists, what its columns are called
// and what type each one is, which is most of what somebody wanted it for.
//
// The condition is built from the policy's own reading of the catalog, so the
// shorter of the two lists is the one sent: a database of four hundred tables
// with one secret in it is not asked about four hundred names.
func (a *analysis) narrowCatalog() {
	var reads []tableRef
	for _, ref := range a.tables {
		if isCatalog(parts(ref.node)) {
			reads = append(reads, ref)
		}
	}
	if len(reads) == 0 {
		return
	}

	for _, ref := range reads {
		view := tableOf(ref.node)
		// Only the parts of the catalog that describe tables and columns are
		// readable, and the condition is that each has a table name to narrow on.
		// VIEWS and ROUTINES hold the text of a definition, which names the tables
		// a policy exists to keep quiet, so they are not on the list.
		switch strings.ToLower(view) {
		case "tables", "columns", "table_constraints", "key_column_usage", "constraint_column_usage":
		default:
			a.refuse("You can read only the parts of the catalog that describe tables and columns.")
			return
		}
	}

	// The narrowing goes on the statement's own WHERE, and there has to be
	// exactly one query to put it on. A catalog read joined to another query, or
	// unioned with one, has more than one place the condition would have to go
	// and no way to be sure which; that is refused rather than half-narrowed.
	spec := a.soleSpecification()
	if spec == nil {
		a.refuse("You can read the catalog one query at a time.")
		return
	}

	visible, hiddenNames := a.guard.VisibleTables(), a.guard.HiddenTables()
	var condition string
	if len(hiddenNames) > 0 && len(hiddenNames) <= len(visible) {
		condition = fmt.Sprintf("TABLE_NAME NOT IN (%s)", literals(hiddenNames))
	} else if len(visible) > 0 {
		condition = fmt.Sprintf("TABLE_NAME IN (%s)", literals(visible))
	} else {
		// Nothing is visible, so nothing is listed.
		condition = "1 = 0"
	}

	if where := spec.GetWhere(); where != nil {
		// The statement's own condition is wrapped, so that an OR in it cannot
		// widen what comes back: (theirs) AND (ours).
		a.rewriter.InsertBeforeDefault(where.GetStart().GetTokenIndex(), "(")
		a.rewriter.InsertAfterDefault(where.GetStop().GetTokenIndex(), ") AND ("+condition+")")
		a.changed = true
		return
	}

	// No WHERE at all: one is added after the FROM. It goes at the end of the
	// table sources rather than at the end of the statement, because a GROUP BY
	// or an ORDER BY may follow and a WHERE cannot come after either.
	from := spec.From_table_sources()
	if from == nil {
		a.refuse("That catalog query could not be narrowed to what you may see.")
		return
	}
	a.rewriter.InsertAfterDefault(from.GetStop().GetTokenIndex(), " WHERE "+condition)
	a.changed = true
}

// soleSpecification is the statement's one query, or nil when it has more than
// one.
func (a *analysis) soleSpecification() tsql.IQuery_specificationContext {
	var found tsql.IQuery_specificationContext
	n := 0
	walk(a.root, func(node antlr.Tree) {
		if spec, ok := node.(tsql.IQuery_specificationContext); ok {
			found = spec
			n++
		}
	})
	if n != 1 {
		return nil
	}
	return found
}

// literals writes a list of names as SQL string literals, each one quoted the
// way T-SQL quotes a string. The names are the database's own, never anything a
// caller wrote, and the doubling is belt and braces on that.
func literals(names []string) string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "'"+strings.ReplaceAll(name, "'", "''")+"'")
	}
	return strings.Join(out, ", ")
}
