package mysql

import (
	"sort"
	"strings"
	"testing"

	"flexie.io/sag/internal/sqlguard"
)

// Reading a stored routine's body, which is what this engine gives back rather
// than a statement: "BEGIN ... END" for a procedure, "RETURN <expr>" for a
// function.
//
// Each case names a table in a place a reader could plausibly miss. A table this
// does not find is a table the policy is never asked about, so every one of them
// must come back.
func TestARoutineBodyIsReadWhereverATableHides(t *testing.T) {
	for _, c := range []struct {
		body  string
		want  []string
		label string
	}{
		{"BEGIN SELECT a FROM plain_t; END", []string{"plain_t"}, "a plain body"},
		{"BEGIN SELECT a FROM t1; INSERT INTO t2 VALUES (1); END", []string{"t1", "t2"}, "several statements"},
		{"BEGIN IF 1=1 THEN SELECT a FROM if_t; ELSE SELECT b FROM else_t; END IF; END",
			[]string{"else_t", "if_t"}, "both arms of an IF"},
		{"BEGIN IF 1=1 THEN SELECT a FROM if_t; ELSEIF 2=2 THEN SELECT c FROM elif_t; ELSE SELECT b FROM else_t; END IF; END",
			[]string{"elif_t", "else_t", "if_t"}, "an ELSEIF chain"},
		{"BEGIN WHILE 1=1 DO SELECT a FROM while_t; END WHILE; END", []string{"while_t"}, "a WHILE body"},
		{"BEGIN REPEAT SELECT a FROM repeat_t; UNTIL 1=1 END REPEAT; END", []string{"repeat_t"}, "a REPEAT body"},
		{"BEGIN DECLARE c CURSOR FOR SELECT a FROM cursor_t; OPEN c; CLOSE c; END",
			[]string{"cursor_t"}, "the query a cursor runs"},
		{"BEGIN DECLARE CONTINUE HANDLER FOR NOT FOUND INSERT INTO handler_t VALUES (1); SELECT a FROM main_t; END",
			[]string{"handler_t", "main_t"}, "a handler's body"},
		{"BEGIN DECLARE x INT DEFAULT (SELECT a FROM default_t); SELECT 1; END",
			[]string{"default_t"}, "a DECLARE default"},
		{"BEGIN WHILE (SELECT COUNT(*) FROM cond_t) > 0 DO SELECT 1; END WHILE; END",
			[]string{"cond_t"}, "a subquery in a loop condition"},
		{"BEGIN SELECT a FROM outer_t WHERE b IN (SELECT c FROM inner_t); END",
			[]string{"inner_t", "outer_t"}, "a subquery in a WHERE"},
		{"BEGIN CASE 1 WHEN 1 THEN SELECT a FROM simple_when_t; ELSE SELECT b FROM simple_else_t; END CASE; END",
			[]string{"simple_else_t", "simple_when_t"}, "a simple CASE"},
		{"BEGIN CASE WHEN 1=1 THEN SELECT a FROM search_when_t; ELSE SELECT b FROM search_else_t; END CASE; END",
			[]string{"search_else_t", "search_when_t"}, "a searched CASE"},
		{"BEGIN lbl: BEGIN SELECT a FROM label_t; END lbl; END", []string{"label_t"}, "a labelled block"},
		{"BEGIN lbl: WHILE 1=1 DO SELECT a FROM labelled_t; END WHILE lbl; END", []string{"labelled_t"}, "a labelled loop"},
		{"BEGIN DECLARE EXIT HANDLER FOR SQLEXCEPTION BEGIN SELECT a FROM nested_handler_t; END; SELECT 1; END",
			[]string{"nested_handler_t"}, "a handler with a block body"},
		{"RETURN (SELECT a FROM fn_t LIMIT 1)", []string{"fn_t"}, "a function body"},
	} {
		got, ok := mysqlAnalyzer{}.ReadDefinition(c.body)
		if !ok {
			t.Errorf("%s: could not be read at all\n  %s", c.label, c.body)
			continue
		}
		var names []string
		for _, r := range got.Reads {
			names = append(names, strings.ToLower(r.Name))
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: found %v, want %v\n  %s", c.label, names, c.want, c.body)
		}
	}
}

// A body with something in it this cannot account for is UNREADABLE, never
// half read. Every caller treats unreadable as refuse.
func TestABodyThatCannotBeReadInFullIsRefused(t *testing.T) {
	for _, body := range []string{
		"BEGIN CALL other_p(); END",                                  // parses nowhere
		"BEGIN LOOP SELECT a FROM t; END LOOP; END",                  // LOOP is not in the grammar
		"BEGIN PREPARE s FROM 'SELECT 1'; EXECUTE s; END",            // built from a string
		"BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'no'; END", // not in the grammar
		"BEGIN START TRANSACTION; INSERT INTO t VALUES (1); COMMIT; END",
		"this is not sql at all",
		"",
	} {
		_, ok := mysqlAnalyzer{}.ReadDefinition(body)
		if ok {
			t.Errorf("read as accountable, and should not be: %s", body)
		}
	}
}

// And the whole point: a body that reaches a denied table is refused, and one
// that does not still runs.
func TestAProcedureBodyIsHeldToThePolicy(t *testing.T) {
	c := sqlguard.NewCatalog("mysql", "shop", []sqlguard.Table{
		{Name: "orders", Columns: []string{"id", "total"}},
		{Name: "secret_keys", Columns: []string{"id", "value"}},
		{Name: "customers", Columns: []string{"id", "ssn"}},
	}).WithQualifiedRoutines([]sqlguard.QualifiedRoutine{
		{Name: "p_clean", Body: "BEGIN DECLARE x INT; SELECT id, total FROM orders; END"},
		{Name: "p_loop", Body: "BEGIN WHILE 1=1 DO SELECT value FROM secret_keys; END WHILE; END"},
		{Name: "p_else", Body: "BEGIN IF 1=1 THEN SELECT id FROM orders; ELSE SELECT value FROM secret_keys; END IF; END"},
		{Name: "p_cursor", Body: "BEGIN DECLARE c CURSOR FOR SELECT value FROM secret_keys; OPEN c; END"},
		{Name: "p_hidden", Body: "BEGIN SELECT ssn FROM customers; END"},
	})
	g, err := sqlguard.New("mysql", sqlguard.Policy{
		TableMode: sqlguard.ModeDenylist, Tables: "secret_keys",
		FieldMode: sqlguard.ModeDenylist, Fields: "customers.ssn",
	}, c, sqlguard.MayCallRoutines())
	if err != nil {
		t.Fatal(err)
	}

	// The clean one runs. Without this the refusals prove nothing.
	_, reason, err := g.Check("CALL p_clean()")
	if err != nil || reason != "" {
		t.Fatalf("a clean procedure was refused: %v %s", err, reason)
	}
	for _, c := range []struct{ sql, because string }{
		{"CALL p_loop()", "its loop reads a denied table"},
		{"CALL p_else()", "its ELSE branch reads a denied table"},
		{"CALL p_cursor()", "its cursor reads a denied table"},
		{"CALL p_hidden()", "it reads a hidden field"},
	} {
		_, reason, _ := g.Check(c.sql)
		if reason == "" {
			t.Errorf("%s ALLOWED, and should not be: %s", c.sql, c.because)
		}
	}
}

// Every statement the GRAMMAR allows inside a body, from parser.y's own
// ProcedureStatementStmt production rather than from imagination. Nine of the
// fifteen used to refuse the whole body for want of a case.
func TestEveryStatementTheGrammarAllowsInABody(t *testing.T) {
	for _, c := range []struct {
		body string
		want []string
	}{
		{"BEGIN SELECT a FROM sel_t; END", []string{"sel_t"}},
		{"BEGIN WITH c AS (SELECT a FROM cte_t) SELECT * FROM c; END", []string{"cte_t"}},
		{"BEGIN SET @x = 1; END", nil},
		{"BEGIN UPDATE upd_t SET a = 1; END", []string{"upd_t"}},
		{"BEGIN INSERT INTO ins_t VALUES (1); END", []string{"ins_t"}},
		{"BEGIN REPLACE INTO rep_t VALUES (1); END", []string{"rep_t"}},
		{"BEGIN COMMIT; END", nil},
		{"BEGIN ROLLBACK; END", nil},
		{"BEGIN EXPLAIN SELECT a FROM exp_t; END", []string{"exp_t"}},
		{"BEGIN SELECT a FROM u1 UNION SELECT b FROM u2; END", []string{"u1", "u2"}},
		{"BEGIN DELETE FROM del_t; END", []string{"del_t"}},
		{"BEGIN ANALYZE TABLE ana_t; END", []string{"ana_t"}},
		{"BEGIN TRUNCATE TABLE trunc_t; END", []string{"trunc_t"}},
	} {
		got, ok := mysqlAnalyzer{}.ReadDefinition(c.body)
		if !ok {
			t.Errorf("could not be read: %s", c.body)
			continue
		}
		var names []string
		for _, r := range got.Reads {
			names = append(names, strings.ToLower(r.Name))
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("found %v, want %v: %s", names, c.want, c.body)
		}
	}

	// USE changes which database an unqualified name resolves in, and one tool is
	// one database, so a body that switches is not accounted for.
	_, readable := mysqlAnalyzer{}.ReadDefinition("BEGIN USE other; SELECT a FROM t; END")
	if readable {
		t.Error("a body that changes database was read as accountable")
	}
}
