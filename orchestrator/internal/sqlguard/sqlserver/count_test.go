package sqlserver

import "testing"

// Counting the statements at the top of a text, which is not the same as
// counting semicolons and not the same as counting clauses.
//
// A routine's body is made of statements and is part of the one that creates it.
// Both shapes have to be understood: a procedure's body sits under a
// Sql_clauses, and a FUNCTION is a Batch_level_statement whose body clauses have
// no Sql_clauses above them at all. That second one was measured on a real
// session's own CREATE FUNCTION, refused here as "several statements joined
// together" while the database had accepted it.
func TestCountingStatementsAtTheTop(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"SELECT 1", 1},
		{"SELECT 1;", 1},
		{"SELECT 1; SELECT 2", 2},
		{"SELECT 1; DROP TABLE customers", 2},

		// A procedure: body clauses are part of it.
		{"CREATE PROCEDURE dbo.p AS BEGIN SET NOCOUNT ON; SELECT 1; END", 1},
		{"CREATE TRIGGER t ON orders AFTER INSERT AS BEGIN SET NOCOUNT ON; SELECT 1; END", 1},

		// A statement written after a procedure body is ONE statement, and that is
		// the engine's own reading rather than a concession. Put to a live SQL
		// Server: CREATE PROCEDURE dbo.p AS BEGIN SELECT 1 END; DROP TABLE scratch
		// was accepted, scratch was still there afterwards, and the procedure's
		// stored body contained the DROP. A T-SQL procedure body runs to the end
		// of the batch, so the DROP became part of the procedure and never ran.
		// Counting it as two was over-refusing.
		{"CREATE PROCEDURE dbo.p AS BEGIN SELECT 1 END; DROP TABLE customers", 1},

		// A function: a Batch_level_statement, whose body has no Sql_clauses above
		// it. Missing that made a real session's CREATE FUNCTION look like several
		// statements joined together.
		{"CREATE FUNCTION dbo.f() RETURNS int AS BEGIN DECLARE @b int; SET @b = 1; RETURN @b END", 1},
	} {
		got, ok := Analyzer{}.CountStatements(c.sql)
		if !ok {
			t.Errorf("could not read: %.70s", c.sql)
			continue
		}
		if got != c.want {
			t.Errorf("counted %d, want %d: %.80s", got, c.want, c.sql)
		}
	}

	// A function with a statement joined onto the end is not readable here, and
	// the ENGINE does not read it either: put to a live SQL Server, CREATE
	// FUNCTION ... END; DROP TABLE scratch came back "Incorrect syntax near the
	// keyword DROP" and scratch survived. Unreadable is the honest answer and it
	// costs nothing, because there is nothing the database would have run.
	_, readable := Analyzer{}.CountStatements(
		"CREATE FUNCTION dbo.f() RETURNS int AS BEGIN RETURN 1 END; DROP TABLE customers")
	if readable {
		t.Error("read as countable, and the engine cannot read it either")
	}
}
