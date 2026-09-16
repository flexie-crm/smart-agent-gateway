package tsql_test

// The test upstream shipped is not here: it needed testify, which this project
// does not use. What it proved is worth keeping, so it is rewritten here with
// the standard library, and two things it never checked are added, because both
// are load-bearing for the only caller this package has.
//
// This tests the VENDORED FILES, not the parser's design. It is what tells us a
// regeneration (lib/tsql/README.md) landed intact rather than half-written.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"flexie.io/sag/lib/sqlserver/antlr"

	"flexie.io/sag/lib/sqlserver/tsql"
)

// collector counts what the parser could not read. A parse error is the whole
// signal here: ANTLR hands back a tree either way, so a caller that only looks
// at the tree cannot tell a statement it understood from one it did not.
type collector struct {
	*antlr.DefaultErrorListener
	errs []string
}

func (c *collector) SyntaxError(_ antlr.Recognizer, _ any, line, col int, msg string, _ antlr.RecognitionException) {
	c.errs = append(c.errs, msg)
	_ = line
	_ = col
}

// read parses one statement and reports what went wrong, if anything. The
// listeners are replaced on BOTH the lexer and the parser: leave either one in
// place and its errors go to stderr while this reports success.
func read(sql string) (tsql.ITsql_fileContext, []string) {
	lexer := tsql.NewTSqlLexer(antlr.NewInputStream(sql))
	lexErrs := &collector{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrs)

	p := tsql.NewTSqlParser(antlr.NewCommonTokenStream(lexer, 0))
	parseErrs := &collector{}
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrs)
	p.BuildParseTrees = true

	tree := p.Tsql_file()
	return tree, append(lexErrs.errs, parseErrs.errs...)
}

// TestEveryExampleParses is upstream's test, rewritten. The examples are the
// corpus the grammar was built against, so this says only that the generated
// files here are the ones that grammar produces: it is a regeneration check, and
// no evidence at all about how much T-SQL the grammar covers.
func TestEveryExampleParses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("examples", "*.sql"))
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no examples found: the vendored corpus is missing")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			if _, errs := read(string(body)); len(errs) > 0 {
				t.Errorf("%s did not parse: %s", filepath.Base(file), strings.Join(errs, "; "))
			}
		})
	}
}

// TestUnreadableStatementsAreRefused is the half upstream never tested, and the
// half the policy rests on. ANTLR recovers from errors by design and returns a
// tree regardless, so "it parsed" is not a thing the tree can tell you. If this
// ever fails, a statement nothing understood would be read as one that needs
// nothing hidden.
func TestUnreadableStatementsAreRefused(t *testing.T) {
	for _, sql := range []string{
		"SELECT FROM WHERE",
		"SELECT * FROM (",
		"THIS IS NOT SQL AT ALL",
		"SELECT id FROM t WHERE )))",
	} {
		t.Run(sql, func(t *testing.T) {
			tree, errs := read(sql)
			if len(errs) == 0 {
				t.Fatalf("no error reported for %q, so nothing downstream can tell it was not understood", sql)
			}
			if tree == nil {
				t.Fatalf("tree was nil for %q; the point of this test is that it is NOT, "+
					"and that the error count is the only signal", sql)
			}
		})
	}
}

// TestSeveralStatementsStaySeveral guards the one-statement rule at its source.
// The policy refuses a batch, and it can only do that if the reader shows it a
// batch rather than quietly reading the first statement and stopping.
func TestSeveralStatementsStaySeveral(t *testing.T) {
	batches := func(sql string) int {
		tree, errs := read(sql)
		if len(errs) > 0 {
			t.Fatalf("%q did not parse: %v", sql, errs)
		}
		n := 0
		var walk func(antlr.Tree)
		walk = func(node antlr.Tree) {
			if _, ok := node.(tsql.ISql_clausesContext); ok {
				n++
			}
			for _, child := range node.GetChildren() {
				walk(child)
			}
		}
		walk(tree)
		return n
	}
	if got := batches("SELECT 1"); got != 1 {
		t.Errorf("one statement read as %d", got)
	}
	// Both separators, because T-SQL has two and only one of them is punctuation.
	if got := batches("SELECT 1; SELECT 2"); got != 2 {
		t.Errorf("two statements separated by ; read as %d", got)
	}
	if got := batches("SELECT 1\nGO\nSELECT 2"); got != 2 {
		t.Errorf("two statements separated by GO read as %d", got)
	}
}

// TestAParserPerStatementIsSafe records a property the MySQL side does not have:
// TiDB's parser is not goroutine-safe and readers come from a pool for that
// reason. This one needs no pool, and that is worth a test rather than a note,
// because it is the kind of thing a regeneration could silently change.
//
// Run under -race for this to mean anything. Without it, it proves only that
// nothing crashed.
func TestAParserPerStatementIsSafe(t *testing.T) {
	const sql = `SELECT TOP 10 c.id, c.ssn FROM dbo.customers AS c ` +
		`JOIN dbo.orders o ON o.customer_id = c.id WHERE c.id > @p1 ORDER BY c.id`
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, errs := read(sql); len(errs) > 0 {
					t.Errorf("parse failed under concurrency: %v", errs)
					return
				}
			}
		}()
	}
	wg.Wait()
}
