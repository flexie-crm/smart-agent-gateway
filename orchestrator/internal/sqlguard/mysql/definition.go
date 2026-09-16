package mysql

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser/ast"
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

// ReadDefinition reads a view's defining statement, or a routine's body.
//
// A view arrives as one statement and is read directly. A ROUTINE arrives as
// what this engine stores, which is neither: information_schema.ROUTINES gives
// back the body alone, so a procedure is "BEGIN ... END" and a function is
// "RETURN <expr>". Neither is a statement, and for a long while neither could be
// read at all, which meant every procedure worth writing was refused.
//
// Both are readable, and the parser this package already carries does it. See
// readRoutineBody.
func (mysqlAnalyzer) ReadDefinition(definition string) (sqlguard.Definition, bool) {
	stmt, reason := readStatement(definition)
	if reason != "" {
		return readRoutineBody(definition)
	}

	reader := &definitionReader{seen: map[string]bool{}}
	stmt.Accept(reader)
	if reader.unreadable {
		return sqlguard.Definition{}, false
	}

	out := sqlguard.Definition{Reads: reader.reads, Calls: reader.calls}
	if projection := projectionOf(stmt); projection != nil {
		out.Origin = originsOf(projection)
	}
	return out, true
}

// readRoutineBody reads what this engine stores for a stored routine.
//
// Two shapes, and the parser handles both once it is handed something it
// recognises. A FUNCTION body is "RETURN <expr>", and an expression is readable
// as the select list of a query, so RETURN becomes SELECT. A PROCEDURE body is
// "BEGIN ... END", which is not a statement on its own but IS the body of one,
// so it is given the CREATE PROCEDURE it was stored without.
//
// That much was always available. What made it look impossible is that Accept
// visits NOTHING on any of the procedure nodes, so the ordinary walk came back
// saying a body reads no tables, which is worse than failing. The walk below is
// written by hand for that reason, and it names every node type: anything it
// does not recognise leaves the body unreadable rather than half read.
func readRoutineBody(body string) (sqlguard.Definition, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return sqlguard.Definition{}, false
	}

	// A function: RETURN <expr>, read as the query that returns it.
	if rest, isReturn := cutWord(trimmed, "RETURN"); isReturn {
		stmt, reason := readStatement("SELECT " + rest)
		if reason != "" {
			return sqlguard.Definition{}, false
		}
		reader := &definitionReader{seen: map[string]bool{}}
		stmt.Accept(reader)
		if reader.unreadable {
			return sqlguard.Definition{}, false
		}
		return sqlguard.Definition{Reads: reader.reads, Calls: reader.calls}, true
	}

	// A procedure: the body without the CREATE it was stored without. The name
	// is ours and is never used for anything; only the shape matters.
	reader := readers.Get().(*parser.Parser)
	stmts, _, err := reader.Parse("CREATE PROCEDURE sag_read_body() "+trimmed, "", "")
	readers.Put(reader)
	if err != nil || len(stmts) != 1 {
		return sqlguard.Definition{}, false
	}
	info, ok := stmts[0].(*ast.ProcedureInfo)
	if !ok {
		return sqlguard.Definition{}, false
	}

	walker := &routineWalker{reader: &definitionReader{seen: map[string]bool{}}}
	walker.walk(info.ProcedureBody)
	if walker.unreadable || walker.reader.unreadable {
		return sqlguard.Definition{}, false
	}
	return sqlguard.Definition{Reads: walker.reader.reads, Calls: walker.reader.calls}, true
}

// cutWord takes a leading keyword off, when it is there as a whole word.
func cutWord(s, word string) (string, bool) {
	if len(s) <= len(word) || !strings.EqualFold(s[:len(word)], word) {
		return s, false
	}
	next := rune(s[len(word)])
	if next != ' ' && next != '\t' && next != '\n' && next != '(' {
		return s, false
	}
	return strings.TrimSpace(s[len(word):]), true
}

// routineWalker descends a stored routine's body.
//
// Every node type is named. A node this does not know leaves the body
// UNREADABLE, and that is the whole discipline: the first cut of this had a
// catch-all for anything that satisfied ast.StmtNode, and ELSE is stored as a
// ProcedureElseIfBlock which satisfies it, so an entire else branch was read as
// reading nothing at all. A body that is half read looks accounted for, which is
// worse than one that is refused.
type routineWalker struct {
	reader     *definitionReader
	unreadable bool
	// stmts are the ordinary statements found in the body, in the order they
	// appear. A body that is being CREATED is judged by these, one at a time and
	// exactly as if each had been written as a query, because that is what they
	// are: knowing only which TABLES a body touches is too blunt to say whether
	// it hands a hidden field back.
	stmts []ast.StmtNode
}

func (w *routineWalker) walk(n ast.Node) {
	if n == nil || w.unreadable {
		return
	}
	switch v := n.(type) {
	case *ast.ProcedureInfo:
		w.walk(v.ProcedureBody)
	case *ast.ProcedureBlock:
		for _, d := range v.ProcedureVars {
			w.walk(d)
		}
		w.all(v.ProcedureProcStmts)

	// Branching. The condition counts too: it is free to hold a subquery.
	case *ast.ProcedureIfInfo:
		w.walk(v.IfBody)
	case *ast.ProcedureIfBlock:
		w.expr(v.IfExpr)
		w.all(v.ProcedureIfStmts)
		w.walk(v.ProcedureElseStmt)
	case *ast.ProcedureElseIfBlock:
		w.walk(v.ProcedureIfStmt)
	case *ast.ProcedureElseBlock:
		w.all(v.ProcedureIfStmts)
	case *ast.SimpleCaseStmt:
		w.expr(v.Condition)
		for _, when := range v.WhenCases {
			w.expr(when.Expr)
			w.all(when.ProcedureStmts)
		}
		w.all(v.ElseCases)
	case *ast.SearchCaseStmt:
		for _, when := range v.WhenCases {
			w.expr(when.Expr)
			w.all(when.ProcedureStmts)
		}
		w.all(v.ElseCases)

	// Looping.
	case *ast.ProcedureWhileStmt:
		w.expr(v.Condition)
		w.all(v.Body)
	case *ast.ProcedureRepeatStmt:
		w.all(v.Body)
		w.expr(v.Condition)
	case *ast.ProcedureLabelBlock:
		w.walk(v.Block)
	case *ast.ProcedureLabelLoop:
		w.walk(v.Block)

	// A cursor holds a query, and a handler holds statements. Both read tables
	// and neither is written where anybody looks for one.
	case *ast.ProcedureCursor:
		w.walk(v.Selectstring)
	case *ast.ProcedureErrorControl:
		w.walk(v.Operate)
	case *ast.ProcedureDecl:
		w.expr(v.DeclDefault)

	// Nothing of their own to read.
	case *ast.ProcedureJump, *ast.ProcedureOpenCur, *ast.ProcedureCloseCur, *ast.ProcedureFetchInto:

	// An ordinary statement, read the way any statement is.
	//
	// The list is the grammar's own and was taken from it rather than guessed:
	// parser.y, ProcedureStatementStmt, names exactly fifteen things a body may
	// contain, and these are the Go types they become. Six were handled here and
	// the other nine fell to the default below, which refused whole bodies over a
	// TRUNCATE or a COMMIT. A REPLACE is an InsertStmt and a DELETE FROM is a
	// DeleteStmt, so the fifteen are eleven types.
	case *ast.SelectStmt, *ast.SetOprStmt, *ast.InsertStmt, *ast.UpdateStmt,
		*ast.DeleteStmt, *ast.SetStmt, *ast.ExplainStmt, *ast.TruncateTableStmt,
		*ast.AnalyzeTableStmt:
		v.Accept(w.reader)
		if stmt, ok := v.(ast.StmtNode); ok {
			w.stmts = append(w.stmts, stmt)
		}

	// Allowed in a body and reading no table of their own. USE is the exception
	// worth naming: it changes which database unqualified names resolve in, and
	// one tool is one database, so a body that switches is not accounted for.
	case *ast.CommitStmt, *ast.RollbackStmt:
	case *ast.UseStmt:
		w.unreadable = true

	default:
		w.unreadable = true
	}
}

func (w *routineWalker) all(stmts []ast.StmtNode) {
	for _, s := range stmts {
		w.walk(s)
	}
}

// expr reads an expression for the tables a subquery in it would reach.
func (w *routineWalker) expr(e ast.ExprNode) {
	if e == nil {
		return
	}
	e.Accept(w.reader)
}

// projectionOf is the query whose select list names the view's own columns. For
// a set of queries joined together that is the first of them, which is where the
// database itself takes the names from.
func projectionOf(stmt ast.StmtNode) *ast.SelectStmt {
	switch v := stmt.(type) {
	case *ast.SelectStmt:
		return v
	case *ast.SetOprStmt:
		if v.SelectList == nil || len(v.SelectList.Selects) == 0 {
			return nil
		}
		return projectionOf(v.SelectList.Selects[0].(ast.StmtNode))
	}
	return nil
}

// originsOf works out, for each column the view offers, which table column it
// reads. Only a plainly selected column has an origin: one built out of an
// expression is not read from a single place, and saying otherwise would be
// making it up. A column with no origin here is hidden whenever anything the
// view reads has something hidden in it, which is the safe way to be wrong.
func originsOf(sel *ast.SelectStmt) map[string]sqlguard.Column {
	if sel.Fields == nil {
		return nil
	}
	sources := baseSources(sel)
	out := map[string]sqlguard.Column{}
	for _, field := range sel.Fields.Fields {
		if field.WildCard != nil {
			// A star in a stored definition is unusual: the database expands one
			// when the view is made. An unexpanded one names no column, so there is
			// nothing here to record.
			continue
		}
		column, ok := field.Expr.(*ast.ColumnNameExpr)
		if !ok || column.Name == nil {
			continue
		}
		name := field.AsName.O
		if name == "" {
			name = column.Name.Name.O
		}
		table, ok := resolveAgainst(sources, column.Name)
		if !ok {
			continue
		}
		out[strings.ToLower(name)] = sqlguard.Column{Table: table, Name: column.Name.Name.O}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// baseSources is the tables a query selects from, by the name they can be
// qualified with. A subquery or a name a WITH introduced is deliberately absent:
// a column read through one of those cannot be traced to a table from here.
func baseSources(sel *ast.SelectStmt) map[string]string {
	out := map[string]string{}
	if sel.From == nil {
		return out
	}
	var walk func(n ast.ResultSetNode)
	walk = func(n ast.ResultSetNode) {
		switch v := n.(type) {
		case *ast.Join:
			walk(v.Left)
			if v.Right != nil {
				walk(v.Right)
			}
		case *ast.TableSource:
			name, ok := v.Source.(*ast.TableName)
			if !ok {
				return
			}
			alias := strings.ToLower(v.AsName.O)
			if alias == "" {
				alias = strings.ToLower(name.Name.O)
			}
			out[alias] = name.Name.O
		}
	}
	walk(sel.From.TableRefs)
	return out
}

// resolveAgainst names the table a column reads from, when one query's own FROM
// settles it. An unqualified column with more than one table to choose from is
// left unresolved rather than picked.
func resolveAgainst(sources map[string]string, name *ast.ColumnName) (string, bool) {
	if qualifier := strings.ToLower(name.Table.O); qualifier != "" {
		table, ok := sources[qualifier]
		return table, ok
	}
	if len(sources) != 1 {
		return "", false
	}
	for _, table := range sources {
		return table, true
	}
	return "", false
}

// definitionReader collects every table a definition reads. The names a WITH
// introduces are tracked as they come into and go out of scope, because one of
// those looks exactly like a table and is not one; anything else that looks like
// a table is recorded, and it is the catalog that decides afterwards whether it
// knows the name.
type definitionReader struct {
	frames []frame
	reads  []sqlguard.Reference
	seen   map[string]bool
	// unreadable marks a definition with something in it this cannot account
	// for, which leaves the view unvouched for rather than quietly allowed.
	unreadable bool
	// calls is every function this body invokes, by name. Whether any of them is
	// the database's own stored code is the catalog's question, not this one:
	// refusing every body that calls a function would refuse every body.
	calls []string
}

type frame struct {
	node ast.Node
	ctes map[string]bool
}

func (d *definitionReader) Enter(n ast.Node) (ast.Node, bool) {
	switch node := n.(type) {
	case *ast.SelectStmt:
		d.push(node, node.With)
	case *ast.SetOprStmt:
		d.push(node, node.With)
	case *ast.TableName:
		d.read(node)
	case *ast.FuncCallExpr:
		// A stored function called from a body reads whatever it likes, and the
		// tables named HERE would not include one of them.
		d.calls = append(d.calls, node.FnName.O)
	case *ast.CallStmt, *ast.PrepareStmt, *ast.ExecuteStmt:
		// What this body does that cannot be read from it.
		//
		// The tables collected here are only the ones named HERE. A body that
		// runs another routine, or builds a statement out of a string, does its
		// real work somewhere this pass never sees, and crediting it with the
		// tables it happens to mention would be worse than knowing nothing: it
		// would look accounted for. Any of these leaves the whole definition
		// unreadable, which every caller already treats as refuse.
		d.unreadable = true
	}
	return n, false
}

func (d *definitionReader) Leave(n ast.Node) (ast.Node, bool) {
	if len(d.frames) > 0 && d.frames[len(d.frames)-1].node == n {
		d.frames = d.frames[:len(d.frames)-1]
	}
	return n, true
}

func (d *definitionReader) push(node ast.Node, with *ast.WithClause) {
	f := frame{node: node, ctes: map[string]bool{}}
	if with != nil {
		for _, cte := range with.CTEs {
			f.ctes[strings.ToLower(cte.Name.O)] = true
		}
	}
	d.frames = append(d.frames, f)
}

func (d *definitionReader) read(name *ast.TableName) {
	if name.Schema.O == "" {
		key := strings.ToLower(name.Name.O)
		for i := len(d.frames) - 1; i >= 0; i-- {
			if d.frames[i].ctes[key] {
				return
			}
		}
	}
	key := strings.ToLower(name.Schema.O) + "." + strings.ToLower(name.Name.O)
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	d.reads = append(d.reads, sqlguard.Reference{Schema: name.Schema.O, Name: name.Name.O})
}
