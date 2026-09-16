// Package sqlserver reads Transact-SQL for the guard: it parses a statement,
// works out every table and column it would touch, and writes back a statement
// that keeps the policy. One dialect, one folder.
//
// The parser is a vendored ANTLR reader built from the community T-SQL grammar
// (lib/sqlserver/tsql). That is a weaker footing than the Postgres side, where the parser
// is the server's own and cannot disagree with it by construction, and it is the
// best that can be had: Microsoft's own parser is .NET, and nothing reaches .NET
// from a single cross-compiled Go binary. What follows is that this package has
// to be more careful rather than less, and it shows in two places. The sweep
// (scope.go) refuses anything the descent did not account for. And the statement
// list below is an allowlist, so a shape the grammar reads and this package does
// not understand is refused for not being on it.
package sqlserver

import (
	"fmt"
	"strings"
	"unicode"

	"flexie.io/sag/lib/sqlserver/antlr"

	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/lib/sqlserver/tsql"
)

// The SQL Server analyzer. It registers itself, so a build can hold a SQL Server
// database to a policy only if something imports this package.
func init() { sqlguard.Register("sqlserver", Analyzer{}) }

// Analyzer reads Transact-SQL.
type Analyzer struct{}

// Check reads one statement and decides what may run.
//
// The order is the same as every dialect's: read the statement, establish what
// it would touch, decide what may be done with it, and only then rewrite.
func (Analyzer) Check(g *sqlguard.Guard, sql string) (sqlguard.Decision, string, error) {
	clause, stream, reason := readStatement(sql)
	if reason != "" {
		return sqlguard.Decision{}, reason, nil
	}

	a := newAnalysis(g, stream, clause)
	// A routine being MADE is decided by what its body would read, the same way a
	// routine being RUN is. Its body is what readStatement handed back, because a
	// CREATE is a Batch_level_statement and the clause inside it is the body.
	if name, isRoutine := routineBeingMade(sql); isRoutine {
		a.createRoutine(name, clause)
		if a.reason != "" {
			return sqlguard.Decision{}, a.reason, nil
		}
		out := sqlguard.Decision{SQL: sql}
		if a.changed {
			// The body was rewritten to keep the policy, so the CREATE that runs
			// is the rewritten one. Read back before it is sent, like any other
			// rewrite here: nothing downstream would catch a splice that came out
			// as SQL the server cannot parse.
			text := a.rewriter.GetTextDefault()
			if _, _, reason := readStatement(text); reason != "" {
				return sqlguard.Decision{}, "", fmt.Errorf("the rewritten routine could not be read back")
			}
			out.SQL, out.Rewritten = text, true
		}
		return out, "", nil
	}
	a.statement(clause)
	if a.reason == "" {
		a.sweep(clause)
	}
	if a.reason == "" {
		a.decide()
	}
	if a.reason == "" {
		a.rewrite()
	}
	if a.reason != "" {
		return sqlguard.Decision{}, a.reason, nil
	}

	out := sqlguard.Decision{SQL: sql, SawUnknownTable: a.sawUnknown, ListsTables: a.listsTables}
	if a.changed {
		text := a.rewriter.GetTextDefault()
		// What was written back out is read again before it is sent. A rewrite
		// splices text into somebody else's statement, and nothing downstream
		// would catch one that came out as SQL the server cannot parse: the caller
		// would be told the database rejected THEIR query when what it rejected
		// was ours.
		if _, _, reason := readStatement(text); reason != "" {
			return sqlguard.Decision{}, "", fmt.Errorf("the rewritten statement could not be read back")
		}
		out.SQL, out.Rewritten = text, true
	}
	return out, "", nil
}

// newAnalysis starts a reading of one statement.
func newAnalysis(g *sqlguard.Guard, stream *antlr.CommonTokenStream, root antlr.Tree) *analysis {
	return &analysis{
		guard:       g,
		stream:      stream,
		root:        root,
		seenTables:  map[antlr.Tree]bool{},
		seenColumns: map[antlr.Tree]bool{},
		seenStars:   map[antlr.Tree]bool{},
		seenNames:   map[antlr.Tree]bool{},
		replaced:    map[antlr.Tree]bool{},
	}
}

// readStatement reads exactly one statement, and hands back the clause it is
// along with the token stream a rewrite would splice into.
//
// Two things make the one-statement rule this package's own job rather than
// something it can lean on. The parser reads a whole batch file and reports as
// many statements as it finds, so "a; b" parses happily and returns two. And the
// driver cannot help here: Postgres refuses a second command in a prepared
// statement, but T-SQL is a batch language, sp_prepare takes a batch, and a
// semicolon is not a wall on this engine. So it is counted here, and here is the
// only place it is counted.
//
// The error listeners are replaced on BOTH the lexer and the parser, and that is
// the load-bearing line in this function. ANTLR recovers from errors and hands
// back a tree regardless, so a statement nobody understood is indistinguishable
// from a clean one unless somebody counts the errors. Leave either listener in
// place and its errors go to stderr while this reports success.
func readStatement(sql string) (tsql.ISql_clausesContext, *antlr.CommonTokenStream, string) {
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(sql))
	lexErrs := &collector{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrs)

	stream := antlr.NewCommonTokenStream(lexer, 0)
	parser := tsql.NewTSqlParser(stream)
	parseErrs := &collector{}
	parser.RemoveErrorListeners()
	parser.AddErrorListener(parseErrs)
	parser.BuildParseTrees = true

	file := parser.Tsql_file()
	if lexErrs.n > 0 || parseErrs.n > 0 {
		return nil, nil, "That could not be read as a statement. Check the SQL and write it again."
	}
	if file == nil {
		return nil, nil, "the statement was empty"
	}

	var clauses []tsql.ISql_clausesContext
	walk(file, func(node antlr.Tree) {
		c, ok := node.(tsql.ISql_clausesContext)
		if !ok {
			return
		}
		// A lone semicolon is a clause with nothing in it.
		if strings.TrimSpace(text(c)) == ";" {
			return
		}
		// Only the ones at the TOP. A routine's body is made of clauses, and they
		// are part of the one statement that creates it, not statements somebody
		// joined together: counting them made every CREATE PROCEDURE with a real
		// body unreadable, which is most of them.
		for parent := c.GetParent(); parent != nil; parent = parent.GetParent() {
			if _, nested := parent.(tsql.ISql_clausesContext); nested {
				return
			}
		}
		clauses = append(clauses, c)
	})
	switch {
	case len(clauses) == 0:
		return nil, nil, "the statement was empty"
	case len(clauses) > 1:
		// Both separators, because T-SQL has two: the semicolon, and GO, which is
		// not a statement at all but a word the client is supposed to split on.
		return nil, nil, "Send one statement per call. Statements joined together are not permitted."
	}
	return clauses[0], stream, ""
}

// collector counts what the parser could not read. Only the count is used: the
// messages are ANTLR's and name grammar rules, which is not something to put in
// front of anybody.
type collector struct {
	*antlr.DefaultErrorListener
	n int
}

func (c *collector) SyntaxError(antlr.Recognizer, any, int, int, string, antlr.RecognitionException) {
	c.n++
}

// statement decides whether this kind of statement may run at all.
//
// The list is what this package can read end to end and hold to a policy.
// Everything else is refused for not being on it, never because it was
// recognised as dangerous. That direction matters more here than on either other
// dialect, because T-SQL has more ways to reach a table without naming it: EXEC
// and sp_executesql build a statement out of a string this side never sees, a
// procedure or function body hides what it runs, OPENQUERY and OPENROWSET and a
// four-part name reach another server, and a rename moves a table out from under
// its own policy. Not one of them has to be enumerated to be refused.
func (a *analysis) statement(clause tsql.ISql_clausesContext) {
	// Calling a stored routine, when an administrator has allowed it. The body
	// is what gets held to the policy, because the call says nothing about what
	// it does; the decision is CallAllowed's, shared by every dialect.
	if other := clause.Another_statement(); other != nil {
		if exec := other.Execute_statement(); exec != nil {
			a.execute(exec)
			return
		}
	}
	dml := clause.Dml_clause()
	if dml == nil {
		a.refuse("You are not permitted to run that kind of statement.")
		return
	}
	a.dml(dml)
}

// dml decides one data statement. Split out from statement so that a routine's
// body, which is a list of these, is read exactly the same way.
func (a *analysis) dml(dml tsql.IDml_clauseContext) {
	switch {
	case dml.Select_statement_standalone() != nil:
		a.selectStandalone(dml.Select_statement_standalone(), nil)
	case dml.Insert_statement() != nil:
		a.write = true
		a.insert(dml.Insert_statement())
	case dml.Update_statement() != nil:
		a.write = true
		a.update(dml.Update_statement())
	case dml.Delete_statement() != nil:
		a.write = true
		a.del(dml.Delete_statement())
	case dml.Merge_statement() != nil:
		// MERGE is an INSERT, an UPDATE and a DELETE at once, each with its own
		// rows and its own columns, and it can hand rows back with OUTPUT on top.
		// It is refused rather than half-read: what this package would have to be
		// sure of to allow it is every one of those paths at once, and being
		// nearly sure is the thing this whole design exists to avoid.
		a.refuse("You are not permitted to run MERGE.")
	default:
		a.refuse("You are not permitted to run that kind of statement.")
	}
}

// execute reads EXEC, and allows exactly one shape of it: a named routine.
//
// Everything else this statement can be is refused, and none of it by being
// recognised as dangerous. A string executed as SQL is built somewhere this side
// cannot see. AT a linked server runs it on another machine. AS a login runs it
// as somebody else. A name in four parts is on another server. What is left is
// "run this routine, whose body is in the catalogue", which is the only form
// there is anything to read.
func (a *analysis) execute(node tsql.IExecute_statementContext) {
	body := node.Execute_body()
	if body == nil {
		a.refuse("That EXEC could not be read.")
		return
	}
	if len(body.AllExecute_var_string()) > 0 {
		a.refuse("You are not permitted to run SQL built from a string. Write the statement out.")
		return
	}
	if body.GetLinkedServer() != nil {
		a.refuse("You can reach one database on one server, and this runs somewhere else.")
		return
	}
	name := body.Func_proc_name_server_database_schema()
	if name == nil {
		a.refuse("Name the routine to run: which one this is could not be established.")
		return
	}
	a.write = true
	a.callNamed(text(name))
}

// callNamed decides one routine, written the way a table is: up to four parts,
// and the same rule applies, one tool is one database on one server.
func (a *analysis) callNamed(raw string) {
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = ident(parts[i])
	}
	switch len(parts) {
	case 1, 2:
	case 3:
		if !strings.EqualFold(parts[0], a.guard.Catalog().Database()) {
			a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
			return
		}
	default:
		a.refuse("You can reach one database on one server, and this name is on another.")
		return
	}
	// Schema AND name, because two schemas are free to offer the same routine,
	// and a three-part name has already been checked against the database above.
	name := parts[len(parts)-1]
	if len(parts) >= 2 {
		name = parts[len(parts)-2] + "." + name
	}
	if ok, why := a.guard.CallAllowed(name); !ok {
		a.refuse("%s", why)
	}
}

// routineCall decides a stored function invoked inside a statement.
//
// EXEC announces itself and a function call does not: `SELECT dbo.f()` is a
// SELECT, and the body it runs is the database's own code, free to read whatever
// the policy keeps back. Measured before it was written: with a policy hiding
// customers.ssn, a function returning that column handed it straight over. The
// same node covers a table-valued function in a FROM, which is the other way in.
//
// It rides the sweep rather than the descent because a call can sit anywhere an
// expression can, and the sweep is the pass that visits everything. Only a name
// the catalog KNOWS is a routine is decided about: everything else is a built-in,
// and refusing LOWER() would refuse every statement worth running.
func (a *analysis) routineCall(node tsql.IScalar_function_nameContext) {
	name := node.Func_proc_name_server_database_schema()
	if name == nil {
		// A built-in the grammar spells as a keyword: LEFT, RIGHT, CHECKSUM.
		return
	}
	raw := text(name)
	if !a.guard.IsRoutine(unquoteName(raw)) {
		return
	}
	a.callNamed(raw)
}

// unquoteName takes the quoting off each PART of a dotted name.
//
// Per part, because ident() reads the ENDS of what it is given: handed
// [dbo].[f_leak] whole it strips the outer brackets and returns dbo].[f_leak.
// That happens to still contain a dot, which is the only reason the name was
// decided about at all, and leaning on that is not a thing to leave in place.
func unquoteName(raw string) string {
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = ident(parts[i])
	}
	return strings.Join(parts, ".")
}

// CountStatements reads the text with the vendored grammar and counts the
// statements at the TOP of it.
//
// It counts for itself rather than leaning on readStatement, because the two are
// asking different questions. readStatement wants the one clause the analyzer
// will walk; this wants to know whether somebody joined a second statement onto
// the end of the first, and a routine's body is full of clauses that are part of
// the one statement that creates it.
//
// Both shapes have to be counted. A procedure body's clauses sit under a
// Sql_clauses, and a FUNCTION does not: it is a Batch_level_statement, so its
// body's clauses have no Sql_clauses above them at all. Measured on a real
// session's own CREATE FUNCTION, which this refused as "several statements
// joined together" while the database had accepted it happily.
func (Analyzer) CountStatements(sql string) (int, bool) {
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(sql))
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
		return 0, false
	}

	count := 0
	walk(file, func(node antlr.Tree) {
		switch c := node.(type) {
		case tsql.IBatch_level_statementContext:
			if !insideAStatement(c) {
				count++
			}
		case tsql.ISql_clausesContext:
			// A lone semicolon is a clause with nothing in it.
			if strings.TrimSpace(text(c)) != ";" && !insideAStatement(c) {
				count++
			}
		}
	})
	return count, true
}

// insideAStatement reports whether a node sits within another statement, and is
// therefore part of it rather than one of its own.
func insideAStatement(node antlr.Tree) bool {
	for parent := node.GetParent(); parent != nil; parent = parent.GetParent() {
		switch parent.(type) {
		case tsql.ISql_clausesContext, tsql.IBatch_level_statementContext:
			return true
		}
	}
	return false
}

// routineBeingMade reports whether this text creates or alters a stored routine
// or a trigger, and what it is called.
func routineBeingMade(sql string) (string, bool) {
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(sql))
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
		return "", false
	}
	name, found := "", false
	walk(file, func(node antlr.Tree) {
		if found {
			return
		}
		switch v := node.(type) {
		case tsql.ICreate_or_alter_procedureContext:
			name, found = unquoteName(text(v.Func_proc_name_schema())), true
		case tsql.ICreate_or_alter_functionContext:
			name, found = unquoteName(text(v.Func_proc_name_schema())), true
		case tsql.ICreate_or_alter_triggerContext:
			name, found = "the trigger", true
		}
	})
	return name, found
}

// createRoutine decides a routine being made, by what its body would do.
//
// Every statement in the body is read exactly as a query is, and the one
// difference is that a body cannot be rewritten: where a query would have had a
// hidden value replaced with a stand-in, a routine is refused, because there is
// no later moment at which to do the replacing.
func (a *analysis) createRoutine(name string, body tsql.ISql_clausesContext) {
	a.write = true
	if name == "" {
		name = "the routine"
	}
	var inner []tsql.IDml_clauseContext
	walk(body, func(node antlr.Tree) {
		if dml, ok := node.(tsql.IDml_clauseContext); ok {
			inner = append(inner, dml)
		}
	})

	// What the body RUNS, which the loop below never meets: an EXEC is not a
	// Dml_clause, and neither is a stored function called from a DECLARE or a
	// SET. Measured before this was written, with a policy denying payroll:
	//
	//	CREATE PROCEDURE dbo.p AS BEGIN EXEC dbo.reads_payroll; END
	//
	// was made without anything reading the routine it runs, so the checkbox that
	// lets a tool create procedures also let it wrap every procedure it was not
	// allowed to call. A body's calls are decided here the same way a statement's
	// are, and the chain is followed from each of them.
	covered := map[antlr.Tree]bool{}
	for _, dml := range inner {
		walk(dml, func(n antlr.Tree) { covered[n] = true })
	}
	walk(body, func(node antlr.Tree) {
		if a.reason != "" || covered[node] {
			return
		}
		switch ref := node.(type) {
		case tsql.IExecute_statementContext:
			a.execute(ref)
		case tsql.IScalar_function_nameContext:
			a.routineCall(ref)
		}
	})
	if a.reason != "" {
		a.reason = fmt.Sprintf("The body of %q is not permitted. %s", name, a.reason)
		return
	}

	// And a query that is not one of the body statements below, which is the
	// shape the loop would walk straight past.
	if uncoveredSelects(body, inner) {
		a.refuse("The body of %q could not be read.", name)
		return
	}

	if len(inner) == 0 {
		// A body that reads nothing is nothing to object to.
		return
	}
	// One rewriter for the whole statement, so every body statement's replacement
	// lands in the same output.
	a.rewriter = antlr.NewTokenStreamRewriter(a.stream)
	for _, dml := range inner {
		sub := newAnalysis(a.guard, a.stream, dml)
		sub.rewriter = a.rewriter
		sub.dml(dml)
		if sub.reason == "" {
			sub.decide()
		}
		if sub.reason == "" {
			sub.rewrite()
		}
		if sub.reason != "" {
			a.refuse("The body of %q is not permitted. %s", name, sub.reason)
			return
		}
		// A hidden field in the body is REPLACED, not refused, exactly as in a
		// query: the policy says what the field is worth to anyone reading through
		// this tool, and a routine made through this tool is no different from a
		// query run through it.
		if sub.changed {
			a.changed = true
		}
	}
}

// BodyKeepsPolicy reads a stored body and reports whether running it would hand
// back anything the policy keeps back. This engine stores the whole CREATE, so
// the statements are found inside it.
func (Analyzer) BodyKeepsPolicy(g *sqlguard.Guard, body string) (bool, string) {
	// One parse, and the stream it was read from. The rewriter works by token
	// INDEX, so a tree from one parse and a stream from another line up only by
	// accident: handing it a second stream panicked with "range invalid".
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(body))
	lexErrs := &collector{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrs)
	stream := antlr.NewCommonTokenStream(lexer, 0)
	parser := tsql.NewTSqlParser(stream)
	parseErrs := &collector{}
	parser.RemoveErrorListeners()
	parser.AddErrorListener(parseErrs)
	parser.BuildParseTrees = true

	tree := parser.Tsql_file()
	if lexErrs.n > 0 || parseErrs.n > 0 || tree == nil {
		return false, "has a body that could not be read, and only routines with a readable SQL body are permitted"
	}

	var inner []tsql.IDml_clauseContext
	walk(tree, func(node antlr.Tree) {
		if dml, is := node.(tsql.IDml_clauseContext); is {
			inner = append(inner, dml)
		}
	})
	for _, dml := range inner {
		sub := newAnalysis(g, stream, dml)
		sub.dml(dml)
		if sub.reason == "" {
			sub.decide()
		}
		if sub.reason == "" {
			sub.rewrite()
		}
		if sub.reason != "" {
			return false, "has a body that is not permitted: " + sub.reason
		}
		if sub.changed {
			what := "a hidden field"
			if names := sub.hiddenNames(); len(names) > 0 {
				what = "the hidden " + plural(names, "field", "fields") + " " + quoteAll(names)
			}
			return false, fmt.Sprintf("returns %s. A value inside a routine that already "+
				"exists cannot be hidden, so it cannot be run", what)
		}
	}

	// Not every query in a T-SQL body is a statement. A scalar function's whole
	// body can be RETURN (SELECT ... ), and that SELECT is an expression, so the
	// walk above never sees it. Measured: with that missed, a function returning
	// a hidden column ran and handed the value over.
	//
	// So anything this walk did not cover falls back to the blunt rule, which is
	// the one this replaced: a body that READS a table with a hidden column in it
	// is refused, whether or not it returns that column. Less precise, and it
	// only applies where precision was not available.
	if uncoveredSelects(tree, inner) {
		definition, ok := Analyzer{}.ReadDefinition(body)
		if !ok {
			return false, "has a body that could not be read, and only routines with a readable SQL body are permitted"
		}
		for _, read := range definition.Reads {
			columns, known := g.Catalog().Columns(read.Name)
			if !known {
				return false, "reads something that could not be accounted for"
			}
			if g.MasksAnyColumn(read.Name, columns) {
				return false, "reads a hidden field somewhere it cannot be established whether the " +
					"value comes back"
			}
		}
	}
	return true, ""
}

// uncoveredSelects reports whether the body holds a query that is not one of the
// statements already read, which is how a T-SQL scalar function hides its whole
// body inside a RETURN.
func uncoveredSelects(tree antlr.Tree, covered []tsql.IDml_clauseContext) bool {
	seen := map[antlr.Tree]bool{}
	for _, dml := range covered {
		walk(dml, func(node antlr.Tree) { seen[node] = true })
	}
	loose := false
	walk(tree, func(node antlr.Tree) {
		if loose {
			return
		}
		if _, is := node.(tsql.IQuery_specificationContext); is && !seen[node] {
			loose = true
		}
	})
	return loose
}

// lowerFirst drops the capital off a sentence that is about to be used as a
// clause inside another one, so a message does not read "... is not permitted:
// You cannot ...".
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if len(r) > 1 && unicode.IsUpper(r[1]) {
		// An acronym or a quoted name: leave it alone.
		return s
	}
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// plural picks the word that agrees with how many there are, and quoteAll lists
// them the way a sentence does, so a message about one field does not say
// "fields" and a message about two does not read as one long name.
func plural(items []string, one, many string) string {
	if len(items) == 1 {
		return one
	}
	return many
}

func quoteAll(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fmt.Sprintf("%q", s)
	}
	if len(out) <= 1 {
		return strings.Join(out, "")
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}
