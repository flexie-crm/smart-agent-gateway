package sqlserver

import (
	"strings"

	"flexie.io/sag/lib/sqlserver/tsql"
)

// What the policy makes of what the descent found.
//
// Two rules, and they are not the same shape. A table this tool may not reach
// REFUSES the statement, because there is no version of the answer that is safe
// to give. A hidden field does not refuse anything: it is replaced where its
// value would be RETURNED and left exactly as it is everywhere else, because a
// WHERE, an ORDER BY, a GROUP BY and a join over it are all questions about the
// value rather than the value itself, and refusing them would refuse most of the
// queries anybody actually wants to run.

// decide applies the policy to everything the descent collected.
func (a *analysis) decide() {
	for _, ref := range a.tables {
		a.decideTable(ref)
		if a.reason != "" {
			return
		}
	}
	for i := range a.columns {
		a.decideColumn(&a.columns[i])
		if a.reason != "" {
			return
		}
	}
	for _, ref := range a.assignments {
		a.decideAssignment(ref)
		if a.reason != "" {
			return
		}
	}
	a.decideRolled()
}

// decideTable settles one table reference.
func (a *analysis) decideTable(ref tableRef) {
	p := parts(ref.node)
	name := tableOf(ref.node)
	if name == "" {
		a.refuse("A table name in this statement could not be read.")
		return
	}

	// One tool is one database. A name with a database or a server in front of it
	// reaches outside this connection, where nothing here has a catalog, a policy,
	// or any way to know what it would return. Four parts is a linked server,
	// three is another database on this one, and both are refused by the same
	// rule rather than by naming either.
	switch len(p) {
	case 1, 2:
		// table, or schema.table: both inside this database.
	case 3:
		// database.schema.table. Allowed only when the database is the one this
		// connection is already on, where it says nothing new.
		if !strings.EqualFold(databaseOf(p), a.guard.Catalog().Database()) {
			a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
			return
		}
	case 4:
		// server.database.schema.table is a linked server, and it is refused
		// whatever the rest of the name says. The database part matching ours
		// means nothing here: the first part sends the query to another machine,
		// where this side has no catalog, no policy and no idea what comes back.
		a.refuse("You can reach one database on one server, and this name is on another.")
		return
	default:
		a.refuse("The name %q could not be read.", text(ref.node))
		return
	}

	// The catalog reads are narrowed rather than refused, because introspection is
	// half of what an assistant does with a database it has not met. Which parts
	// are readable is settled in rewrite.go; this is only the gate.
	if isCatalog(p) {
		if a.write {
			// There is no single WHERE to narrow in a statement that changes rows,
			// and half a narrowing is worse than none.
			a.refuse("You are not permitted to read the database's catalog inside a statement that changes rows.")
			return
		}
		return
	}

	// sys.* is SQL Server's own catalog, and it is reachable without naming a
	// schema at all: sys is on every name-resolution path the way pg_catalog is
	// in Postgres. sys.sql_modules and OBJECT_DEFINITION hand back the text of a
	// view, which names the very columns a policy exists to keep quiet, so the
	// whole of it is refused rather than narrowed.
	if isSys(p) {
		a.refuse("You are not permitted to read the database's own system catalog.")
		return
	}

	if _, known := a.guard.Catalog().Lookup(name); !known {
		// A snapshot is minutes old, so a table it has never heard of may simply
		// be newer than the snapshot. The caller re-reads the catalog and asks
		// again once, and a refusal that stands after a fresh read is a real one.
		a.sawUnknown = true
	}
	if ok, why := a.guard.Reaches(name); !ok {
		a.refuse("%s", why)
	}
}

// databaseOf is the database part of a three or four part name.
func databaseOf(p []string) string {
	switch len(p) {
	case 3:
		return p[0]
	case 4:
		return p[1]
	}
	return ""
}

// isCatalog reports whether a name is a read of INFORMATION_SCHEMA.
func isCatalog(p []string) bool {
	if len(p) < 2 {
		return false
	}
	return strings.EqualFold(p[len(p)-2], "information_schema")
}

// isSys reports whether a name reaches SQL Server's own catalog, qualified or
// not. The unqualified form matters as much as the qualified one: sys is on
// every search path, so sys.objects and a bare reference to one of its views
// both arrive there.
func isSys(p []string) bool {
	if len(p) >= 2 && strings.EqualFold(p[len(p)-2], "sys") {
		return true
	}
	// A bare name beginning with sys. that the snapshot does not have.
	if len(p) == 1 {
		name := strings.ToLower(p[0])
		return strings.HasPrefix(name, "sys.") || name == "sysobjects" || name == "syscolumns" ||
			name == "sysindexes" || name == "sysusers" || name == "sysfiles"
	}
	return false
}

// decideColumn settles one column reference.
//
// A column that is not returned is left alone whatever it is: that is the rule,
// and it is what keeps filtering, sorting, grouping and joining working over a
// hidden field. Only a returned one is looked into.
func (a *analysis) decideColumn(ref *columnRef) {
	// The OUTPUT pseudo-tables. inserted.x and deleted.x are the row as it was
	// and as it is, and the column is the target table's column.
	if ref.node.INSERTED() != nil || ref.node.DELETED() != nil {
		if !ref.returned {
			return
		}
		column := columnOf(ref.node)
		if column == "" {
			return
		}
		if a.hiddenAnywhere(ref.scope, column) {
			ref.hidden, ref.name = true, column
		}
		return
	}

	column := columnOf(ref.node)
	if column == "" {
		// $IDENTITY and $ROWGUID name a column without naming it: they are
		// whichever column carries that property, which this side cannot know.
		// Refused when returned, because what comes back could be anything.
		if ref.returned {
			a.refuse("You are not permitted to return $IDENTITY or $ROWGUID: which column they stand for cannot be established.")
		}
		return
	}
	if !ref.returned {
		return
	}
	// The cheap first half: a name nothing could hide needs no resolving. It is
	// asked of the guard rather than of the policy's text, because a view is free
	// to offer customers.ssn under another name and no rule mentions that one.
	if !a.guard.CouldHide(column) {
		return
	}

	qualifier := qualifierOf(ref.node)
	tables := a.resolve(ref.scope, qualifier, column)
	switch len(tables) {
	case 0:
		// Nothing in scope accounts for it. A statement is not dangerous for
		// mentioning a name, so this fails closed the narrow way: hidden where it
		// would be returned, and left alone everywhere else.
		ref.hidden, ref.name = true, column
	default:
		for _, table := range tables {
			if a.guard.MasksColumn(table, column) {
				ref.hidden, ref.name = true, column
				return
			}
		}
	}
}

// tablesInScope is every base table reachable from a scope, nearest first. It is
// what a reference resolves to when it names a row rather than a table: the
// OUTPUT pseudo-tables stand for the table being written, and that is the table
// in scope.
func (a *analysis) tablesInScope(s *scope) []string {
	var out []string
	for at := s; at != nil; at = at.parent {
		for _, src := range at.order {
			if src.table != "" {
				out = append(out, src.table)
			}
		}
	}
	return out
}

// hiddenAnywhere reports whether a column of this name is hidden in anything in
// scope. It is what an inserted./deleted. reference resolves to: the pseudo-table
// is the target table's row, and the target is in scope.
func (a *analysis) hiddenAnywhere(s *scope, column string) bool {
	if !a.guard.CouldHide(column) {
		return false
	}
	for at := s; at != nil; at = at.parent {
		for _, src := range at.order {
			if src.table == "" {
				continue
			}
			if a.guard.MasksColumn(src.table, column) {
				return true
			}
		}
	}
	return false
}

// columnOf is the column a reference names, or empty when it names one without
// naming it.
func columnOf(ref tsql.IFull_column_nameContext) string {
	if id := ref.GetColumn_name(); id != nil {
		return ident(text(id))
	}
	return ""
}

// qualifierOf is what a column was written under: c in c.ssn, empty in ssn.
func qualifierOf(ref tsql.IFull_column_nameContext) string {
	name := ref.Full_table_name()
	if name == nil {
		return ""
	}
	return tableOf(name)
}

// resolve works out which tables a column could belong to.
//
// A qualified column belongs to what the qualifier names. An unqualified one
// belongs to whichever table in scope actually HAS a column of that name, which
// is asked of the catalog rather than guessed: a rule about customers.ssn has to
// go on applying when a second table joins the query.
//
// The one case nothing can resolve is a column two tables in scope both have,
// and nothing needs to: the database rejects that statement as ambiguous, so it
// never runs whatever this decides. It comes back as both tables, and being
// hidden if either hides it is the safe way round.
func (a *analysis) resolve(s *scope, qualifier, column string) []string {
	if qualifier != "" {
		for at := s; at != nil; at = at.parent {
			if src, ok := at.sources[strings.ToLower(qualifier)]; ok {
				if src.table == "" {
					// A derived table or a CTE: its columns were decided where they
					// were read, and there is nothing here to decide again.
					return nil
				}
				return []string{src.table}
			}
		}
		// inserted and deleted are the OUTPUT pseudo-tables: the row as it will be,
		// and as it was. They arrive here as an ordinary qualifier rather than as
		// anything special, because the grammar's id_ accepts keywords, so
		// deleted.ssn looks exactly like customers.ssn by the time it gets this
		// far. What they stand for is the table being written, which is the table
		// in scope.
		if strings.EqualFold(qualifier, "inserted") || strings.EqualFold(qualifier, "deleted") {
			return a.tablesInScope(s)
		}
		// A qualifier naming nothing in scope. Treated as the table itself, which
		// is what an unaliased dbo.customers.ssn is.
		return []string{qualifier}
	}

	var out []string
	for at := s; at != nil; at = at.parent {
		for _, src := range at.order {
			if src.table == "" {
				continue
			}
			columns, known := a.guard.Catalog().Columns(src.table)
			if !known {
				continue
			}
			for _, c := range columns {
				if strings.EqualFold(c, column) {
					out = append(out, src.table)
					break
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// decideAssignment settles one column a write assigns to.
//
// Neither of the two refusals here is about the policy's wording. Writing INTO a
// hidden field would put the stand-in over whatever is really there, which is
// data destroyed by the control that exists to protect it. Reading one in a SET
// would copy the real value into a column that can be read back afterwards, and
// a SET has no select list to put a stand-in in.
func (a *analysis) decideAssignment(ref assignRef) {
	target := ref.node.Full_column_name()
	if target == nil {
		return
	}
	column := columnOf(target)
	if column == "" {
		return
	}
	if a.hiddenAnywhere(ref.scope, column) {
		a.refuse("%q is a hidden field and you cannot write to it: its real value is not visible here, so a write would replace it with the placeholder.", column)
		return
	}
	// And the value side: anything hidden read here lands somewhere readable.
	walkColumns(ref.node.Expression(), func(c tsql.IFull_column_nameContext) {
		name := columnOf(c)
		if name != "" && a.hiddenAnywhere(ref.scope, name) {
			a.refuse("%q is a hidden field and you cannot copy it into another column, where it would then be readable.", name)
		}
	})
}

// decideRolled settles FOR XML and FOR JSON, which turn the whole answer into
// one value.
//
// There is no select item left to replace: what comes back is every column of
// the result rolled into a document, and taking one out would mean rebuilding
// it. So it is refused when anything in reach has something hidden in it, and
// left alone otherwise, which is the same rule every other reference follows.
func (a *analysis) decideRolled() {
	if len(a.rolled) == 0 {
		return
	}
	for _, s := range a.scopes {
		for _, src := range s.order {
			if src.table == "" {
				continue
			}
			columns, known := a.guard.Catalog().Columns(src.table)
			if !known {
				a.refuse("What FOR XML or FOR JSON would return here could not be established.")
				return
			}
			if a.guard.MasksAnyColumn(src.table, columns) {
				a.refuse("You are not permitted to use FOR XML or FOR JSON over a table with a hidden field: the whole row is rolled into one value, leaving nowhere to put the placeholder.")
				return
			}
		}
	}
}
