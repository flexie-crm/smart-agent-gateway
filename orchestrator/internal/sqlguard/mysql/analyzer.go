package mysql

import (
	"fmt"
	"strings"
	"sync"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"

	// The parser needs a driver for the literal values inside a statement, and
	// the one it ships for use outside its own server carries this name. It is a
	// third-party import path, so it keeps the name its authors gave it.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

// Package mysql reads MySQL and MariaDB SQL for the guard: it parses a
// statement, works out every table and column it would touch, and writes back a
// statement that keeps the policy. It is one dialect in its own folder, and the
// only one this build has. It registers itself, so a build can read this
// dialect only if something imports it.
func init() { sqlguard.Register("mysql", mysqlAnalyzer{}) }

type mysqlAnalyzer struct{}

// A reader holds the state of the statement it is reading, so one is never
// shared between two calls. They cost microseconds to make, which is why they
// are pooled rather than kept on anything longer-lived.
var readers = sync.Pool{New: func() any { return parser.New() }}

// Check reads one statement and decides what may run.
//
// The order is deliberate. Nothing is decided from the text: the statement is
// read first, and a statement that cannot be read is refused, because a check
// that guessed at what the server would do with it would be a check in name
// only. Then what it would touch is established (every table, through views and
// through the names a WITH introduces), then what it may do with what it
// touches, and only then is anything rewritten.
func (mysqlAnalyzer) Check(g *sqlguard.Guard, sql string) (sqlguard.Decision, string, error) {
	// Whatever is decided about has to be what runs, so the text is settled
	// before anything reads it: a comment the server would run is taken out here,
	// and everything after this point (including what is sent to the database)
	// works on what is left.
	sql, stripped := stripExecutableComments(sql)

	stmt, reason := readStatement(sql)
	if reason != "" {
		return sqlguard.Decision{}, reason, nil
	}

	a := &analysis{guard: g}
	a.statement(stmt)
	if a.reason == "" {
		stmt.Accept(a)
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

	out := sqlguard.Decision{SQL: sql, Rewritten: stripped, ListsTables: a.listsTables, SawUnknownTable: a.sawUnknown}
	if a.changed {
		// The statement is about to be written back out by the reader's own
		// printer, which is a second implementation of the language and can write
		// something that does not mean what was read. Ask first.
		if reason := faithful(sql); reason != "" {
			return sqlguard.Decision{}, reason, nil
		}
		text, err := restore(stmt)
		if err != nil {
			return sqlguard.Decision{}, "", fmt.Errorf("rebuild statement: %w", err)
		}
		out.SQL, out.Rewritten = text, true
	}
	return out, "", nil
}

// readStatement reads exactly one statement. Two statements in one string come
// back from the reader without complaint, so the one-statement rule is enforced
// here rather than assumed; the driver refuses stacked statements as well, and
// this is the belt to that suspenders, with a reason the assistant can act on.
func readStatement(sql string) (ast.StmtNode, string) {
	p := readers.Get().(*parser.Parser)
	defer readers.Put(p)

	stmts, _, err := p.Parse(sql, "", "")
	switch {
	case err != nil:
		return nil, "this tool could not read that as a statement, so it was not run. Check the SQL and write it again."
	case len(stmts) == 0:
		return nil, "the statement was empty"
	case len(stmts) > 1:
		return nil, "run one statement at a time; several statements joined together are not allowed"
	}
	return stmts[0], ""
}

// statement decides whether this kind of statement may run at all.
//
// The list is what the guard can read end to end and hold to a policy. Anything
// else is refused because it is not on the list, never because it was recognised
// as dangerous: a stored program's body, a statement built at run time from a
// string, and a rename are all ways to reach a table without ever naming it, and
// no reading of the statement in front of us would show it.
func (a *analysis) statement(stmt ast.StmtNode) {
	switch s := stmt.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt:
		a.write = true
	case *ast.ShowStmt:
		a.show(s)
	case *ast.ExplainStmt:
		// A plan is a reading of a statement, so the statement it explains gets
		// the same reading. EXPLAIN ANALYZE is not a plan: it runs the thing, and
		// hands back how many rows each step really saw.
		if s.Analyze {
			a.refuse("this tool does not run EXPLAIN ANALYZE, because it runs the statement as well as explaining it. Ask for the plan with EXPLAIN.")
			return
		}
		a.statement(s.Stmt)
	default:
		a.refuse("that kind of statement is not allowed by this tool")
	}
}

// show decides one SHOW. What is permitted is what describes the tables of this
// database; the rest of SHOW reaches past this tool's business entirely (the
// server's variables, its other databases, what everybody else is running right
// now, the text of a view's definition).
func (a *analysis) show(s *ast.ShowStmt) {
	if s.DBName != "" && !strings.EqualFold(s.DBName, a.guard.Catalog().Database()) {
		a.refuse("this tool reaches one database, %q", a.guard.Catalog().Database())
		return
	}
	switch s.Tp {
	case ast.ShowTables, ast.ShowTableStatus:
		// A list of tables cannot be narrowed in the asking, so it is narrowed in
		// the answering: the rows come back filtered.
		a.listsTables = true
	case ast.ShowColumns, ast.ShowIndex:
		a.showsTable(s.Table)
	case ast.ShowCreateTable:
		// The definition of a view names the tables underneath it, which is the
		// one thing hiding a table is meant to prevent.
		if s.Table != nil {
			if t, ok := a.guard.Catalog().Lookup(s.Table.Name.O); ok && t.View {
				a.refuse("this tool does not show how %q is defined", s.Table.Name.O)
				return
			}
		}
		a.showsTable(s.Table)
	default:
		a.refuse("that kind of SHOW is not allowed by this tool. It can list tables, and describe the columns and indexes of one.")
	}
}

func (a *analysis) showsTable(t *ast.TableName) {
	if t == nil {
		a.refuse("name the table to describe")
		return
	}
	if ok, reason := a.guard.Reaches(t.Name.O); !ok {
		a.refuse("%s", withoutRetry(reason))
	}
}

// restore writes a statement back out as SQL. What comes back is tidied rather
// than as it was typed, which is why a statement nothing changed is sent on
// exactly as it arrived.
func restore(n ast.Node) (string, error) {
	var b strings.Builder
	if err := n.Restore(format.NewRestoreCtx(format.DefaultRestoreFlags, &b)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// withoutRetry closes a refusal the way the server tool's does: with the one
// sentence that stops an assistant from spending the rest of the turn looking
// for another way to ask the same question.
func withoutRetry(reason string) string {
	return reason + ". What this tool may reach is fixed by its configuration, so rewording will not help; report that it is not available."
}

// stripExecutableComments takes out every comment the SERVER might run, and
// reports whether it took anything.
//
// This is the one place where reading a statement is not enough, because the two
// readers disagree by design. A /*! ... */ comment is executed by a server whose
// version is at least the number after it, and MariaDB's /*M! ... */ is executed
// by MariaDB and ignored by everything else. So:
//
//	SELECT id FROM orders /*M!100000 UNION SELECT value FROM secret_keys */
//
// is one statement to whatever reads it here and two to MariaDB, and every rule
// in this package would have been deciding about the wrong one.
//
// Taking it out rather than refusing it settles the disagreement instead of
// working around it: what is left is parsed, decided about, AND sent to the
// database, so there is only one text and nothing left for two readers to
// differ over. It is also the kinder answer, because a database dump is full of
// /*!40101 ... */ and somebody pasting one has done nothing wrong.
//
// Getting the extent of a comment wrong cannot reopen the gap, only close a
// statement down: what comes out either parses, and is then checked as exactly
// the thing that will run, or does not, and is refused.
//
// The scan is quote-aware, so a statement that merely has the characters inside
// a value keeps them.
func stripExecutableComments(sql string) (string, bool) {
	var out strings.Builder
	var quote rune
	var escaped bool
	stripped := false

	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			out.WriteRune(r)
			switch {
			case escaped:
				escaped = false
			case r == '\\' && quote != '`':
				// A backslash escapes the next character in a string, and means
				// nothing inside a quoted identifier.
				escaped = true
			case r == quote:
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			quote = r
			out.WriteRune(r)
			continue
		}
		if r == '/' && isExecutableOpener(runes[i:]) {
			if end := closingComment(runes, i); end >= 0 {
				// A comment is whitespace to the statement around it, so what
				// stood either side of it does not run together.
				out.WriteRune(' ')
				i = end
				stripped = true
				continue
			}
			// An opener with no close is not a comment the server would run
			// either; leaving it be lets the parser call it what it is.
		}
		out.WriteRune(r)
	}
	return out.String(), stripped
}

// isExecutableOpener reports whether a run of text begins /*! or /*M!.
func isExecutableOpener(runes []rune) bool {
	if len(runes) < 3 || runes[1] != '*' {
		return false
	}
	if runes[2] == '!' {
		return true
	}
	return len(runes) > 3 && runes[2] == 'M' && runes[3] == '!'
}

// closingComment is the index of the last character of the comment that opens at
// start, or -1 when it is never closed. A comment ends at the first */ after it:
// they do not nest, and the server ends one the same way.
func closingComment(runes []rune, start int) int {
	for i := start + 2; i+1 < len(runes); i++ {
		if runes[i] == '*' && runes[i+1] == '/' {
			return i + 1
		}
	}
	return -1
}
