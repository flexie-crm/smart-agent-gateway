// Package postgres reads PostgreSQL SQL for the guard: it parses a statement,
// works out every table and column it would touch, and writes back a statement
// that keeps the policy. One dialect, one folder.
//
// The parser is PostgreSQL's own, compiled to WebAssembly and run in-process, so
// what this side reads is what the server would read. That matters more here
// than anywhere: the whole design rests on the two readings agreeing, and the
// one place they disagreed on the MySQL side (a comment the server executed and
// the reader did not) cost a hole. Here there is only one reader.
package postgres

import (
	"fmt"
	"strings"
	"unicode"

	"flexie.io/sag/internal/sqlguard"
	pgq "github.com/pganalyze/pg_query_go/v6"
	pg "github.com/wasilibs/go-pgquery"
)

// The PostgreSQL analyzer. It registers itself, so a build can hold a Postgres
// database to a policy only if something imports this package.
func init() { sqlguard.Register("postgres", Analyzer{}) }

// Analyzer reads PostgreSQL SQL.
type Analyzer struct{}

// Check reads one statement and decides what may run.
//
// The order is the same as every dialect's: read the statement, establish what
// it would touch, decide what may be done with it, and only then rewrite.
func (Analyzer) Check(g *sqlguard.Guard, sql string) (sqlguard.Decision, string, error) {
	tree, reason := readStatement(sql)
	if reason != "" {
		return sqlguard.Decision{}, reason, nil
	}

	a := &analysis{guard: g}
	a.statement(tree.Stmts[0].Stmt)
	if a.reason == "" {
		a.sweep(tree)
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

	out := sqlguard.Decision{SQL: sql, SawUnknownTable: a.sawUnknown}
	if a.changed {
		text, err := pg.Deparse(tree)
		if err != nil {
			return sqlguard.Decision{}, "", fmt.Errorf("rebuild statement: %w", err)
		}
		// What was written back out is read again before it is sent. Nothing
		// downstream would catch a rewrite that came out as SQL the server cannot
		// parse, and the caller would be told the database rejected their query
		// when what it rejected was ours.
		if _, err := pg.Parse(text); err != nil {
			return sqlguard.Decision{}, "", fmt.Errorf("rebuilt statement is not readable: %w", err)
		}
		out.SQL, out.Rewritten = text, true
	}
	return out, "", nil
}

// readStatement reads exactly one statement.
//
// An empty string parses to nothing without complaint and two statements parse
// to two, so the one-statement rule is enforced here rather than assumed. The
// driver prepares every statement, which is Postgres refusing a second one for
// its own reasons; this is the belt to that suspenders, with a reason an
// assistant can act on.
func readStatement(sql string) (*pgq.ParseResult, string) {
	tree, err := pg.Parse(sql)
	switch {
	case err != nil:
		return nil, "That could not be read as a statement. Check the SQL and write it again."
	case tree == nil || len(tree.Stmts) == 0:
		return nil, "the statement was empty"
	case len(tree.Stmts) > 1:
		return nil, "Send one statement per call. Statements joined together are not permitted."
	case tree.Stmts[0].Stmt == nil:
		return nil, "the statement was empty"
	}
	return tree, ""
}

// statement decides whether this kind of statement may run at all.
//
// The list is what this can read end to end and hold to a policy. Everything
// else is refused because it is not on the list, never because it was
// recognised as dangerous: a function body, a statement built at run time from a
// string, and a rename are all ways to reach a table without naming it.
func (a *analysis) statement(node *pgq.Node) {
	switch v := node.Node.(type) {
	case *pgq.Node_SelectStmt:
		a.query(v.SelectStmt, nil)
	case *pgq.Node_InsertStmt:
		a.write = true
		a.insert(v.InsertStmt, nil)
	case *pgq.Node_UpdateStmt:
		a.write = true
		a.update(v.UpdateStmt, nil)
	case *pgq.Node_DeleteStmt:
		a.write = true
		a.delete(v.DeleteStmt, nil)
	case *pgq.Node_CallStmt:
		// Calling a stored routine, when an administrator has allowed it. The
		// body is what gets held to the policy, because the call says nothing
		// about what it does; the decision is CallAllowed's, shared by every
		// dialect.
		a.write = true
		a.call(v.CallStmt)
	case *pgq.Node_CreateFunctionStmt:
		// MAKING a routine, when an administrator has allowed it. The body is here
		// in the statement rather than in the catalogue, and it is held to the
		// same rules: a routine that would read what this tool cannot see is
		// refused before it exists, rather than created and refused on every call.
		a.write = true
		a.createFunction(v.CreateFunctionStmt)
	case *pgq.Node_ExplainStmt:
		// A plan is a reading of a statement, so the statement it explains gets
		// the same reading. EXPLAIN ANALYZE is not a plan: it runs the thing.
		explain := node.GetExplainStmt()
		for _, option := range explain.GetOptions() {
			if def := option.GetDefElem(); def != nil && strings.EqualFold(def.GetDefname(), "analyze") {
				a.refuse("You are not permitted to run EXPLAIN ANALYZE, which runs the statement as well as explaining it. Use EXPLAIN for the plan.")
				return
			}
		}
		a.statement(explain.GetQuery())
	default:
		a.refuse("You are not permitted to run that kind of statement.")
	}
}

// createFunction decides a routine being made, by what its body would read.
//
// Postgres keeps the body in an option called "as", as text, in whatever
// language the "language" option names. Only a body written in SQL can be read;
// a plpgsql one is a different language this does not speak, and an unread body
// is refused rather than assumed harmless.
func (a *analysis) createFunction(stmt *pgq.CreateFunctionStmt) {
	name := "the routine"
	if parts := stmt.GetFuncname(); len(parts) > 0 {
		if str := parts[len(parts)-1].GetString_(); str != nil {
			name = str.GetSval()
		}
	}

	language, body := "", ""
	for _, option := range stmt.GetOptions() {
		def := option.GetDefElem()
		switch strings.ToLower(def.GetDefname()) {
		case "language":
			language = strings.ToLower(def.GetArg().GetString_().GetSval())
		case "as":
			for _, item := range def.GetArg().GetList().GetItems() {
				if str := item.GetString_(); str != nil {
					body = str.GetSval()
				}
			}
		}
	}
	if language != "sql" {
		a.refuse("%q is written in %s. Only routines written in plain SQL are permitted.", name, language)
		return
	}

	definition, ok := Analyzer{}.ReadDefinition(body)
	if !ok {
		a.refuse("The body of %q could not be read. Write it out of plain SQL statements.", name)
		return
	}
	// Only the nested-routine question here. Which tables it may read and which
	// fields it may return are decided below, statement by statement, because a
	// body that READS customers to filter on a hidden field returns nothing and
	// must not be refused for it.
	if allowed, why := a.guard.BodyAllowed(name, "create", sqlguard.Definition{Calls: definition.Calls}); !allowed {
		a.refuse("%s", why)
		return
	}
	// And the body itself, judged exactly as a query is, because knowing which
	// TABLES it touches cannot say whether it hands a hidden field back.
	inner, reason := readStatement(body)
	if reason != "" {
		a.refuse("The body of %q could not be read.", name)
		return
	}
	sub := &analysis{guard: a.guard}
	sub.statement(inner.Stmts[0].Stmt)
	if sub.reason == "" {
		sub.sweep(inner)
	}
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
	if !sub.changed {
		return
	}
	// A hidden field in the body is REPLACED, not refused, exactly as in a query:
	// the policy says what the field is worth to anyone reading through this
	// tool, and a routine made through this tool is no different from a query run
	// through it.
	//
	// Postgres keeps the body as TEXT inside the statement, so the rewritten body
	// is written back into that option and the whole CREATE is deparsed from the
	// tree, rather than spliced into the text by hand.
	rewritten, err := pg.Deparse(inner)
	if err != nil {
		a.refuse("The body of %q could not be rewritten with its hidden fields replaced.", name)
		return
	}
	if !setFunctionBody(stmt, rewritten) {
		a.refuse("The body of %q could not be rewritten with its hidden fields replaced.", name)
		return
	}
	a.changed = true
}

// setFunctionBody writes a new body into the "as" option, and reports whether it
// found one to write into.
func setFunctionBody(stmt *pgq.CreateFunctionStmt, body string) bool {
	for _, option := range stmt.GetOptions() {
		def := option.GetDefElem()
		if !strings.EqualFold(def.GetDefname(), "as") {
			continue
		}
		for _, item := range def.GetArg().GetList().GetItems() {
			if str := item.GetString_(); str != nil {
				str.Sval = body
				return true
			}
		}
	}
	return false
}

// call decides whether a stored routine may run, by what its body reads.
//
// The name comes from the parsed call rather than the text. Postgres qualifies
// with a schema rather than a database, and one tool is one database, so a name
// in more parts than this engine can mean here is refused rather than guessed
// at.
func (a *analysis) call(stmt *pgq.CallStmt) {
	fn := stmt.GetFunccall()
	if fn == nil {
		a.refuse("Name the routine to run: which one this is could not be established.")
		return
	}
	var parts []string
	for _, n := range fn.GetFuncname() {
		if s := n.GetString_(); s != nil {
			parts = append(parts, s.GetSval())
		}
	}
	if len(parts) == 0 {
		a.refuse("Name the routine to run: which one this is could not be established.")
		return
	}
	if len(parts) > 2 {
		a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
		return
	}
	if ok, why := a.guard.CallAllowed(parts[len(parts)-1]); !ok {
		a.refuse("%s", why)
	}
}

// CountStatements reads the text with the server's own parser, so a dollar
// quoted body counts as the one statement it is.
func (Analyzer) CountStatements(sql string) (int, bool) {
	tree, err := pg.Parse(sql)
	if err != nil || tree == nil {
		return 0, false
	}
	return len(tree.Stmts), true
}

// BodyKeepsPolicy reads a stored body and reports whether running it would hand
// back anything the policy keeps back.
func (Analyzer) BodyKeepsPolicy(g *sqlguard.Guard, body string) (bool, string) {
	tree, reason := readStatement(body)
	if reason != "" {
		return false, "has a body that could not be read, and only routines with a readable SQL body are permitted"
	}
	sub := &analysis{guard: g}
	sub.statement(tree.Stmts[0].Stmt)
	if sub.reason == "" {
		sub.sweep(tree)
	}
	if sub.reason == "" {
		sub.decide()
	}
	if sub.reason == "" {
		sub.rewrite()
	}
	if sub.reason != "" {
		return false, "has a body that is not permitted: " + strings.TrimSuffix(lowerFirst(sub.reason), ".")
	}
	if sub.changed {
		what := "a hidden field"
		if names := sub.hiddenNames(); len(names) > 0 {
			what = "the hidden " + plural(names, "field", "fields") + " " + quoteAll(names)
		}
		return false, fmt.Sprintf("returns %s. A value inside a routine that already "+
			"exists cannot be hidden, so it cannot be run", what)
	}
	return true, ""
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
