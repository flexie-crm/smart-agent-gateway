package sqlserver

import (
	"strings"

	"flexie.io/sag/lib/sqlserver/antlr"

	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/lib/sqlserver/tsql"
)

// Reading a view's definition is how a rule written about a table's column
// survives being reached through a view that calls the column something else,
// and how a view over a table nobody may see is refused without the statement
// ever naming that table.
//
// Two things come out of a definition: every table it reads, and where each of
// its own columns comes from. Neither is guessed at. A definition that cannot be
// read in full leaves the view unaccounted for, and an unaccounted-for view is
// refused rather than assumed to be harmless.

// ReadDefinition reads a view's defining statement.
//
// What arrives here is a whole CREATE VIEW, because that is what SQL Server
// stores: sys.sql_modules keeps the text of the statement that made the object,
// not the query inside it. So the query is found within it rather than expected
// to be the whole of it.
func (Analyzer) ReadDefinition(definition string) (sqlguard.Definition, bool) {
	tree, ok := readDefinition(definition)
	if !ok {
		return sqlguard.Definition{}, false
	}

	reader := &definitionReader{}
	reader.read(tree)
	if reader.unreadable {
		return sqlguard.Definition{}, false
	}

	out := sqlguard.Definition{Reads: reader.reads, Calls: reader.calls}
	if spec := firstSpecification(tree); spec != nil {
		out.Origin = originsOf(spec)
	}
	return out, true
}

// readDefinition parses a stored definition. It is deliberately not
// readStatement: that one enforces the one-statement rule, and a CREATE VIEW is
// not a statement anybody asked to run. Errors still refuse, on the same
// footing: what cannot be read in full is not accounted for.
func readDefinition(definition string) (antlr.Tree, bool) {
	if strings.TrimSpace(definition) == "" {
		return nil, false
	}
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(definition))
	lexErrs := &collector{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrs)

	parser := tsql.NewTSqlParser(antlr.NewCommonTokenStream(lexer, 0))
	parseErrs := &collector{}
	parser.RemoveErrorListeners()
	parser.AddErrorListener(parseErrs)
	parser.BuildParseTrees = true

	file := parser.Tsql_file()
	if lexErrs.n > 0 || parseErrs.n > 0 || file == nil {
		return nil, false
	}
	return file, true
}

// definitionReader collects every table a definition reads.
type definitionReader struct {
	reads      []sqlguard.Reference
	seen       map[string]bool
	ctes       map[string]bool
	unreadable bool
	// calls is every function this body invokes, by name. Whether any of them is
	// the database's own stored code is the catalog's question, not this one:
	// refusing every body that calls a function would refuse every body.
	calls []string
}

func (r *definitionReader) read(tree antlr.Tree) {
	r.seen, r.ctes = map[string]bool{}, map[string]bool{}

	// What this body RUNS, which is not a table it reads and is not in the walk
	// below.
	//
	// EXEC of a named routine is recorded as a call and followed from here: the
	// routine it names is in the same snapshot, so it can be read as easily as
	// the one that was asked for. This used to leave the whole definition
	// unreadable, which meant a procedure built out of the procedures already in
	// the database could not be run, and that is most of the procedures anybody
	// writes.
	//
	// Every other shape stays unreadable, and none of it by being recognised as
	// dangerous. A string run as SQL is built somewhere this side cannot see, and
	// AT a linked server runs somewhere else. What is left is "run this routine,
	// whose body is in the catalogue", which is the only form there is anything
	// to read.
	walk(tree, func(node antlr.Tree) {
		switch v := node.(type) {
		case tsql.IExecute_statementContext:
			r.executes(v)
		case tsql.IExecute_var_stringContext, tsql.IExecute_body_batchContext:
			r.unreadable = true
		}
	})
	if r.unreadable {
		return
	}

	// Every function this body invokes. A stored one reads whatever it likes,
	// and the tables named HERE would not include one of them. A table-valued
	// function in a FROM is caught below instead, where a source with no table
	// name of its own leaves the whole body unreadable.
	walk(tree, func(node antlr.Tree) {
		name, ok := node.(tsql.IScalar_function_nameContext)
		if !ok {
			return
		}
		proc := name.Func_proc_name_server_database_schema()
		if proc == nil {
			// A built-in the grammar spells as a keyword: LEFT, RIGHT, CHECKSUM.
			return
		}
		r.called(text(proc))
	})

	// The names a WITH introduced first, because they look exactly like tables
	// and are not, and a view is free to define one.
	walk(tree, func(node antlr.Tree) {
		cte, ok := node.(tsql.ICommon_table_expressionContext)
		if !ok {
			return
		}
		if id := cte.GetExpression_name(); id != nil {
			r.ctes[strings.ToLower(ident(text(id)))] = true
		}
	})

	walk(tree, func(node antlr.Tree) {
		item, ok := node.(tsql.ITable_source_itemContext)
		if !ok {
			return
		}
		name := item.Full_table_name()
		if name == nil {
			// A view that reads something this side cannot resolve: a function, a
			// rowset, a table variable. The view is left unaccounted for rather
			// than credited with reading only what could be recognised.
			r.unreadable = true
			return
		}
		p := parts(name)
		table := tableOf(name)
		if table == "" {
			r.unreadable = true
			return
		}
		if len(p) == 1 && r.ctes[strings.ToLower(table)] {
			return
		}
		key := strings.ToLower(strings.Join(p, "."))
		if r.seen[key] {
			return
		}
		r.seen[key] = true

		ref := sqlguard.Reference{Name: table}
		switch len(p) {
		case 1:
		case 2:
			// schema.table. The schema is not the database, and the catalog's own
			// names carry no schema, so it is kept for what the guard makes of it.
		case 3, 4:
			// A view reaching another database, or another server. Nothing here can
			// account for what that returns.
			ref.Schema = databaseOf(p)
			r.unreadable = true
			return
		default:
			r.unreadable = true
			return
		}
		r.reads = append(r.reads, ref)
	})
}

// executes reads one EXEC in a body, and records the routine it names.
//
// It is the same reading the statement path gives an EXEC somebody sent, and
// deliberately so: a routine run from inside a body is a routine being run.
func (r *definitionReader) executes(node tsql.IExecute_statementContext) {
	body := node.Execute_body()
	if body == nil {
		r.unreadable = true
		return
	}
	if len(body.AllExecute_var_string()) > 0 || body.GetLinkedServer() != nil {
		r.unreadable = true
		return
	}
	name := body.Func_proc_name_server_database_schema()
	if name == nil {
		// AS LOGIN, AS USER, AS CALLER: it runs as somebody else, and what it
		// then reaches is not this pass's to say.
		r.unreadable = true
		return
	}
	r.called(text(name))
}

// called records one routine a body runs, named the way the guard reads one:
// schema AND name, because two schemas are free to offer the same routine.
//
// A name in three parts or more is another database or another server, and
// nothing here can account for what that returns. It leaves the definition
// unreadable rather than being shortened to its last two parts, which would make
// a call to somebody else's database look like a call to a routine of ours.
func (r *definitionReader) called(raw string) {
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = ident(parts[i])
	}
	switch len(parts) {
	case 1:
		r.calls = append(r.calls, parts[0])
	case 2:
		r.calls = append(r.calls, parts[0]+"."+parts[1])
	default:
		r.unreadable = true
	}
}

// firstSpecification is the query whose select list names the view's own
// columns. For a set of queries joined with UNION that is the first of them,
// which is where the database itself takes the names from.
func firstSpecification(tree antlr.Tree) tsql.IQuery_specificationContext {
	var found tsql.IQuery_specificationContext
	walk(tree, func(node antlr.Tree) {
		if found != nil {
			return
		}
		if spec, ok := node.(tsql.IQuery_specificationContext); ok {
			found = spec
		}
	})
	return found
}

// originsOf works out, for each column a view offers, which table column it
// reads.
//
// Only a plainly selected column has an origin. One built out of an expression
// is not read from a single place, and saying otherwise would be making it up: a
// column with no entry here is hidden whenever anything the view reads has
// something hidden in it, which is the safe way to be wrong. Over-hiding costs a
// column in a report; the other way costs the value.
func originsOf(spec tsql.IQuery_specificationContext) map[string]sqlguard.Column {
	list := spec.Select_list()
	if list == nil {
		return nil
	}
	// What each alias in the FROM stands for, so c.ssn is credited to customers.
	sources := map[string]string{}
	walk(spec, func(node antlr.Tree) {
		item, ok := node.(tsql.ITable_source_itemContext)
		if !ok {
			return
		}
		name := item.Full_table_name()
		if name == nil {
			return
		}
		table := tableOf(name)
		sources[strings.ToLower(table)] = table
		if as := item.As_table_alias(); as != nil {
			if t := as.Table_alias(); t != nil {
				sources[strings.ToLower(ident(text(t)))] = table
			}
		}
	})

	out := map[string]sqlguard.Column{}
	for _, elem := range list.AllSelect_list_elem() {
		expr := elem.Expression_elem()
		if expr == nil {
			continue
		}
		// Exactly one column reference, and the item is nothing but that
		// reference: anything more and it is an expression.
		var columns []tsql.IFull_column_nameContext
		walkColumns(elem, func(c tsql.IFull_column_nameContext) { columns = append(columns, c) })
		if len(columns) != 1 {
			continue
		}
		ref := columns[0]
		column := columnOf(ref)
		if column == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(text(expr.Expression())), "") {
			continue
		}
		// The item must be the column itself, not a function of it.
		if !strings.EqualFold(ident(text(expr.Expression())), ident(text(ref))) {
			continue
		}

		table := qualifierOf(ref)
		if table == "" {
			// Unqualified in a view over one table is that table.
			if len(sources) == 1 {
				for _, only := range sources {
					table = only
				}
			}
		} else if resolved, ok := sources[strings.ToLower(table)]; ok {
			table = resolved
		}
		if table == "" {
			continue
		}

		name := column
		if as := expr.As_column_alias(); as != nil {
			if alias := aliasText(as); alias != "" {
				name = alias
			}
		}
		if left := expr.GetLeftAlias(); left != nil {
			name = ident(text(left))
		}
		out[strings.ToLower(name)] = sqlguard.Column{Table: table, Name: column}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
