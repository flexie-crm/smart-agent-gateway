package mysql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Is the statement we are about to send still the statement that was asked for?
//
// A rewritten statement is written back out by the reader's own printer, and a
// printer is a second implementation of the language: it can write something
// that does not mean what was read. Two ways it does, both found by running it:
//
//	WHERE note = 'a\\b'   comes back as   WHERE note = 'a\b'
//	CHAR(65)              comes back as   CHAR_FUNC(65, NULL)
//
// The first quietly returns the wrong rows, which is the worse of the two: an
// answer that is confidently wrong and carries nothing to say so. The second
// fails at the database, which at least announces itself.
//
// Neither is caught by comparing what we sent with what we meant, because both
// survive a round trip in their corrupted form. What catches them is asking
// whether the printer is faithful FOR THIS STATEMENT at all: write the statement
// as it arrived back out, read it again, and see whether the same values and the
// same functions come back. If they do not, this side cannot say what would run,
// so nothing runs.
//
// It is checked only when something had to be rewritten. A statement nothing
// changed is sent on exactly as it was typed and never goes near the printer.

// faithful reports why a statement cannot be safely rebuilt, or "" when it can.
func faithful(sql string) string {
	first, reason := readStatement(sql)
	if reason != "" {
		return reason
	}
	written, err := restore(first)
	if err != nil {
		return "That statement could not be written back out with the hidden field replaced."
	}
	second, reason := readStatement(written)
	if reason != "" {
		return "The statement could not be read back after the hidden field was replaced."
	}

	before, after := &surface{}, &surface{}
	first.Accept(before)
	second.Accept(after)

	if lost := before.missingFrom(after); lost != "" {
		return fmt.Sprintf("%s cannot be kept as it is while a hidden field is replaced. "+
			"Write the value a different way, or name the columns you want without the hidden ones.", lost)
	}
	// A function the reader calls something else. CHAR(65) is read as a call to
	// char_func, which is the reader's own name for it and not a name the
	// database has, so writing the statement back out produces something the
	// database will not run. The reader and its printer agree with each other
	// here, which is why this is asked of the ORIGINAL TEXT: a name nobody wrote
	// is a name that was invented on the way through.
	if invented := after.functionNotWrittenIn(sql); invented != "" {
		return fmt.Sprintf("%s cannot be kept as it is while a hidden field is replaced. "+
			"Name the columns you want without the hidden ones, and the statement runs untouched.", strings.ToUpper(invented))
	}
	return ""
}

// surface is what a statement says that a printer could get wrong: the values
// written into it, and the functions it calls. Nothing else is compared, because
// everything else is allowed to be written differently (backquotes, COUNT(*) as
// COUNT(1)) without meaning anything different.
type surface struct {
	values    []string
	functions map[string]bool
}

func (s *surface) Enter(n ast.Node) (ast.Node, bool) {
	switch node := n.(type) {
	case ast.ValueExpr:
		s.values = append(s.values, fmt.Sprintf("%#v", node.GetValue()))
	case *ast.FuncCallExpr:
		s.note(node.FnName.L)
	case *ast.AggregateFuncExpr:
		s.note(strings.ToLower(node.F))
	}
	return n, false
}

func (s *surface) Leave(n ast.Node) (ast.Node, bool) { return n, true }

func (s *surface) note(name string) {
	if s.functions == nil {
		s.functions = map[string]bool{}
	}
	s.functions[name] = true
}

// missingFrom names a value that went in and did not come out. Values are
// compared as a multiset: one of two identical literals going missing matters
// just as much as the only one.
func (s *surface) missingFrom(other *surface) string {
	remaining := map[string]int{}
	for _, v := range other.values {
		remaining[v]++
	}
	for _, v := range s.values {
		if remaining[v] == 0 {
			return "the value " + readable(v)
		}
		remaining[v]--
	}
	return ""
}

// functionNotWrittenIn names a function that appears in what would run and
// nowhere in what was asked for.
func (s *surface) functionNotWrittenIn(sql string) string {
	lower := strings.ToLower(sql)
	var invented []string
	for name := range s.functions {
		if !strings.Contains(lower, name) {
			invented = append(invented, name)
		}
	}
	if len(invented) == 0 {
		return ""
	}
	sort.Strings(invented)
	return invented[0]
}

// readable turns the internal spelling of a value back into something worth
// putting in a message, without promising it is SQL.
func readable(value string) string {
	if trimmed := strings.TrimPrefix(value, `"`); trimmed != value {
		return `"` + strings.TrimSuffix(trimmed, `"`) + `"`
	}
	return value
}
