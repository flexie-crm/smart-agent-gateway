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
		return nil, "this tool could not read that as a statement, so it was not run. Check the SQL and write it again."
	case tree == nil || len(tree.Stmts) == 0:
		return nil, "the statement was empty"
	case len(tree.Stmts) > 1:
		return nil, "run one statement at a time; several statements joined together are not allowed"
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
	case *pgq.Node_ExplainStmt:
		// A plan is a reading of a statement, so the statement it explains gets
		// the same reading. EXPLAIN ANALYZE is not a plan: it runs the thing.
		explain := node.GetExplainStmt()
		for _, option := range explain.GetOptions() {
			if def := option.GetDefElem(); def != nil && strings.EqualFold(def.GetDefname(), "analyze") {
				a.refuse("this tool does not run EXPLAIN ANALYZE, because it runs the statement as well as explaining it. Ask for the plan with EXPLAIN.")
				return
			}
		}
		a.statement(explain.GetQuery())
	default:
		a.refuse("that kind of statement is not allowed by this tool")
	}
}

// withoutRetry closes a refusal with the one sentence that stops an assistant
// spending the rest of the turn looking for another way to ask.
func withoutRetry(reason string) string {
	return reason + ". What this tool may reach is fixed by its configuration, so rewording will not help; report that it is not available."
}
