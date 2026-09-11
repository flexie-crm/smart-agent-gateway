package query

import "testing"

// The gate is a security boundary, so its decisions are pinned exactly: each
// mode allows only its class of statement, unknown statements are refused in
// every mode, comments cannot smuggle a statement past the classifier, and
// stacked statements are refused.
func TestAccessGate(t *testing.T) {
	cases := []struct {
		name string
		mode Access
		sql  string
		ok   bool
	}{
		// READ mode: reads pass, writes refused.
		{"read allows select", AccessRead, "SELECT * FROM orders", true},
		{"read allows cte", AccessRead, "WITH x AS (SELECT 1) SELECT * FROM x", true},
		{"read allows explain", AccessRead, "EXPLAIN SELECT 1", true},
		{"read allows show", AccessRead, "SHOW TABLES", true},
		{"read refuses insert", AccessRead, "INSERT INTO t VALUES (1)", false},
		{"read refuses update", AccessRead, "UPDATE t SET a = 1", false},
		{"read refuses delete", AccessRead, "DELETE FROM t", false},
		{"read refuses drop", AccessRead, "DROP TABLE t", false},

		// WRITE mode: writes pass, reads refused.
		{"write allows insert", AccessWrite, "INSERT INTO t VALUES (1)", true},
		{"write allows update", AccessWrite, "UPDATE t SET a = 1 WHERE id = 2", true},
		{"write refuses select", AccessWrite, "SELECT * FROM t", false},

		// BOTH mode: either passes.
		{"both allows select", AccessBoth, "SELECT 1", true},
		{"both allows delete", AccessBoth, "DELETE FROM t WHERE id = 1", true},

		// Unknown statements are refused in every mode.
		{"both refuses unknown", AccessBoth, "VACUUM", false},
		{"both refuses gibberish", AccessBoth, "wibble foo bar", false},
		{"read refuses empty", AccessRead, "   ", false},

		// A comment must not smuggle a write past a read gate.
		{"comment then insert", AccessRead, "-- harmless\nINSERT INTO t VALUES (1)", false},
		{"block comment then update", AccessRead, "/* note */ UPDATE t SET a=1", false},
		{"comment then select ok", AccessRead, "-- fetch\nSELECT * FROM t", true},

		// Case and whitespace do not matter.
		{"lowercase select", AccessRead, "  select 1", true},
		{"mixed case insert", AccessWrite, "InSeRt INTO t VALUES (1)", true},

		// Stacked statements are refused even when both parts would pass alone.
		{"stacked selects", AccessRead, "SELECT 1; SELECT 2", false},
		{"stacked drop after select", AccessRead, "SELECT 1; DROP TABLE t", false},
		{"trailing semicolon ok", AccessRead, "SELECT 1;", true},
		{"semicolon inside string ok", AccessRead, "SELECT ';' AS sep", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := check(tc.mode, tc.sql)
			if ok != tc.ok {
				t.Fatalf("check(%q, %q) = %v (%q), want %v", tc.mode, tc.sql, ok, reason, tc.ok)
			}
			if !ok && reason == "" {
				t.Fatal("a refusal must carry a reason")
			}
		})
	}
}

func TestIsReadOnly(t *testing.T) {
	if !isReadOnly("SELECT 1") || !isReadOnly("  with x as (select 1) select * from x") {
		t.Fatal("a read statement was not recognised as read-only")
	}
	if isReadOnly("UPDATE t SET a = 1") || isReadOnly("INSERT INTO t VALUES (1)") {
		t.Fatal("a write statement was called read-only")
	}
}

func TestAccessValid(t *testing.T) {
	for _, a := range []Access{AccessRead, AccessWrite, AccessBoth} {
		if !a.Valid() {
			t.Fatalf("%q should be valid", a)
		}
	}
	if Access("admin").Valid() || Access("").Valid() {
		t.Fatal("an unknown access mode was accepted")
	}
}
