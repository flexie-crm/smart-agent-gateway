package mysql

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

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
		return nil, "That could not be read as a statement. Check the SQL and write it again."
	case len(stmts) == 0:
		return nil, "the statement was empty"
	case len(stmts) > 1:
		return nil, "Send one statement per call. Statements joined together are not permitted."
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
	case *ast.CallStmt:
		// Calling a stored routine, when an administrator has allowed it. The
		// body is what gets held to the policy, because the call says nothing
		// about what it does; the decision is CallAllowed's, shared by every
		// dialect.
		a.write = true
		a.call(s)
	case *ast.ProcedureInfo:
		// MAKING a stored routine, when an administrator has allowed it. The body
		// is here in the statement rather than in the catalogue, and it is held to
		// the same rules by the same code: a routine that would read a table this
		// tool keeps back is refused before it exists, rather than created and
		// refused every time somebody calls it.
		a.write = true
		a.createProcedure(s)
	case *ast.DropProcedureStmt:
		// Taking one away reads nothing, and the capability is what gates it.
		a.write = true
	case *ast.ShowStmt:
		a.show(s)
	case *ast.ExplainStmt:
		// A plan is a reading of a statement, so the statement it explains gets
		// the same reading. EXPLAIN ANALYZE is not a plan: it runs the thing, and
		// hands back how many rows each step really saw.
		if s.Analyze {
			a.refuse("You are not permitted to run EXPLAIN ANALYZE, which runs the statement as well as explaining it. Use EXPLAIN for the plan.")
			return
		}
		a.statement(s.Stmt)

	// Named, because these are the shapes an assistant reaches for on purpose
	// and a refusal that does not name them reads as a parser failure. The other
	// two dialects name their own equivalent, and this is MySQL's.
	case *ast.PrepareStmt, *ast.ExecuteStmt, *ast.DeallocateStmt:
		a.refuse("You are not permitted to run SQL built from a string. Write the statement out.")

	default:
		a.refuse("You are not permitted to run that kind of statement.")
	}
}

// show decides one SHOW. What is permitted is what describes the tables of this
// database; the rest of SHOW reaches past this tool's business entirely (the
// server's variables, its other databases, what everybody else is running right
// now, the text of a view's definition).
func (a *analysis) show(s *ast.ShowStmt) {
	if s.DBName != "" && !strings.EqualFold(s.DBName, a.guard.Catalog().Database()) {
		a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
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
				a.refuse("You are not permitted to see how %q is defined.", s.Table.Name.O)
				return
			}
		}
		a.showsTable(s.Table)
	default:
		a.refuse("You are not permitted to run that SHOW. SHOW TABLES, SHOW COLUMNS and SHOW INDEX are available.")
	}
}

func (a *analysis) showsTable(t *ast.TableName) {
	if t == nil {
		a.refuse("Name the table to describe.")
		return
	}
	if ok, reason := a.guard.Reaches(t.Name.O); !ok {
		a.refuse("%s", reason)
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

// call decides whether a stored routine may run, by what its body reads.
//
// The name is taken from the parsed call rather than from the text, so a
// qualified name, odd spacing or backquotes make no difference. One tool is one
// database, so a name that reaches out of it is refused before anything is
// looked up.
func (a *analysis) call(stmt *ast.CallStmt) {
	if stmt.Procedure == nil {
		a.refuse("Name the routine to run: which one this is could not be established.")
		return
	}
	if schema := stmt.Procedure.Schema.O; schema != "" &&
		!strings.EqualFold(schema, a.guard.Catalog().Database()) {
		a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
		return
	}
	name := stmt.Procedure.FnName.O
	if name == "" {
		a.refuse("Name the routine to run: which one this is could not be established.")
		return
	}
	if ok, why := a.guard.CallAllowed(name); !ok {
		a.refuse("%s", why)
	}
}

// qualify joins a schema to a name, when there is one. A qualified name is
// decided about as stored code whether or not the snapshot has heard of it,
// which is what stops a routine in a schema nobody read being taken for a
// built-in.
func qualify(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "." + name
}

// createProcedure decides a routine being made, by what its body would read.
//
// The same question as calling one and the same answer, because it is the same
// code: a body is code this tool cannot see through once it exists, so it is
// read now. What it may not read, it may not be written to read.
func (a *analysis) createProcedure(info *ast.ProcedureInfo) {
	name := "the routine"
	if info.ProcedureName != nil && info.ProcedureName.Name.O != "" {
		name = info.ProcedureName.Name.O
	}
	if schema := info.ProcedureName.Schema.O; schema != "" &&
		!strings.EqualFold(schema, a.guard.Catalog().Database()) {
		a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
		return
	}

	walker := &routineWalker{reader: &definitionReader{seen: map[string]bool{}}}
	walker.walk(info.ProcedureBody)
	if walker.unreadable || walker.reader.unreadable {
		a.refuse("The body of %q could not be read. Write it out of plain SQL statements.", name)
		return
	}

	// A routine that runs another routine does its real work out of sight.
	definition := sqlguard.Definition{Calls: walker.reader.calls}
	if ok, why := a.guard.BodyAllowed(name, "create", definition); !ok {
		a.refuse("%s", why)
		return
	}

	// And now each statement in the body, judged exactly as a query is.
	//
	// Knowing which TABLES a body touches is too blunt: a body that READS
	// customers to filter on a hidden field hands nothing back, and refusing it
	// would refuse most of the procedures worth writing. So every statement goes
	// through the same descent and the same decision as if somebody had sent it,
	// and the ONE difference is that a body cannot be rewritten: where a query
	// would have had the value replaced with a stand-in, a routine is refused,
	// because there is no moment later at which to do the replacing.
	for _, stmt := range walker.stmts {
		inner := &analysis{guard: a.guard}
		inner.statement(stmt)
		if inner.reason == "" {
			stmt.Accept(inner)
		}
		if inner.reason == "" {
			inner.decide()
		}
		// And the rewrite, which is where a STAR becomes the columns it stands
		// for. Without it, SELECT * over a table with a hidden column looked
		// clean: nothing had yet worked out what the star meant. The rewriting
		// only ever touches the throwaway copy, and only when something was
		// hidden, in which case this refuses and the copy is dropped.
		if inner.reason == "" {
			inner.rewrite()
		}
		if inner.reason != "" {
			a.refuse("The body of %q is not permitted. %s", name, inner.reason)
			return
		}
		// A hidden field in the body is REPLACED, not refused.
		//
		// The policy says what a hidden field is worth to anyone reading through
		// this tool, and a routine made through this tool is no different from a
		// query run through it: the value is swapped for the stand-in and the rest
		// of the routine is created as written. Refusing instead would mean the
		// policy made the tool unusable for the one thing an administrator turned
		// on, which is not what a policy is for.
		//
		// The replacing already happened: the sub-analysis rewrote the body's AST
		// in place, and the body belongs to the CREATE, so saying so here is
		// enough for the outer pass to write the whole statement back out.
		//
		// changed is the signal, not the list of names: a star is expanded into
		// the columns it stands for rather than hidden one at a time, so a body of
		// SELECT * over a table with a hidden column sets this and names nothing.
		if inner.changed {
			a.changed = true
			a.hiddenFields = append(a.hiddenFields, inner.hiddenNames()...)
		}
	}
}

// routineCall decides a stored function invoked inside a statement.
//
// CALL announces itself and a function call does not: `SELECT f()` is a SELECT,
// and the body it runs is the database's own code, free to read whatever the
// policy keeps back. Measured before it was written: with a policy hiding
// customers.ssn, a function returning that column handed it straight over.
//
// Only a name the catalog KNOWS is a routine gets here. Everything else is a
// built-in, and refusing lower() would refuse every statement worth running.
func (a *analysis) routineCall(name string) {
	if name == "" || !a.guard.IsRoutine(name) {
		return
	}
	if ok, why := a.guard.CallAllowed(name); !ok {
		a.refuse("%s", why)
	}
}

// CountStatements reads the text with this dialect's own parser.
//
// It answers for CREATE PROCEDURE, and cannot for CREATE FUNCTION or CREATE
// TRIGGER, which this parser does not read at all (measured: both are a parse
// error). Those fall back to the caller, which is the one place a body's
// semicolons cannot be told from a statement joined onto the end.
func (mysqlAnalyzer) CountStatements(sql string) (int, bool) {
	p := readers.Get().(*parser.Parser)
	defer readers.Put(p)
	stmts, _, err := p.Parse(sql, "", "")
	if err != nil {
		return 0, false
	}
	return len(stmts), true
}

// BodyKeepsPolicy reads a stored body and reports whether running it would hand
// back anything the policy keeps back. Same reading as the create path, from the
// text this engine stores rather than from a statement somebody sent.
func (mysqlAnalyzer) BodyKeepsPolicy(g *sqlguard.Guard, body string) (bool, string) {
	stmts := bodyStatements(body)
	if stmts == nil {
		return false, "has a body that could not be read, and only routines with a readable SQL body are permitted"
	}
	for _, stmt := range stmts {
		inner := &analysis{guard: g}
		inner.statement(stmt)
		if inner.reason == "" {
			stmt.Accept(inner)
		}
		if inner.reason == "" {
			inner.decide()
		}
		if inner.reason == "" {
			inner.rewrite()
		}
		if inner.reason != "" {
			return false, "has a body that is not permitted: " + strings.TrimSuffix(lowerFirst(inner.reason), ".")
		}
		if inner.changed {
			what := "a hidden field"
			if names := inner.hiddenNames(); len(names) > 0 {
				what = "the hidden " + plural(names, "field", "fields") + " " + quoteAll(names)
			}
			return false, fmt.Sprintf("returns %s. A value inside a routine that already "+
				"exists cannot be hidden, so it cannot be run", what)
		}
	}
	return true, ""
}

// bodyStatements reads a stored body into the statements it is made of, or nil
// when it cannot be read in full.
func bodyStatements(body string) []ast.StmtNode {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	if rest, isReturn := cutWord(trimmed, "RETURN"); isReturn {
		stmt, reason := readStatement("SELECT " + rest)
		if reason != "" {
			return nil
		}
		return []ast.StmtNode{stmt}
	}
	if stmt, reason := readStatement(trimmed); reason == "" {
		return []ast.StmtNode{stmt}
	}
	reader := readers.Get().(*parser.Parser)
	parsed, _, err := reader.Parse("CREATE PROCEDURE sag_read_body() "+trimmed, "", "")
	readers.Put(reader)
	if err != nil || len(parsed) != 1 {
		return nil
	}
	info, ok := parsed[0].(*ast.ProcedureInfo)
	if !ok {
		return nil
	}
	walker := &routineWalker{reader: &definitionReader{seen: map[string]bool{}}}
	walker.walk(info.ProcedureBody)
	if walker.unreadable || walker.reader.unreadable {
		return nil
	}
	return walker.stmts
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
