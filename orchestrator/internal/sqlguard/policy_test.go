package sqlguard

import (
	"strings"
	"testing"
)

// The policy decides what an assistant may see of somebody's database, so these
// tests are written the way the list is actually typed: with blank lines, mixed
// case, and the wildcard in every position it can appear in.

func TestPolicyIsInactiveUntilItNamesSomething(t *testing.T) {
	if (Policy{}).Active() {
		t.Fatal("a blank policy governs nothing and must stay inactive")
	}
	if (Policy{TableMode: ModeDenylist}).Active() {
		t.Fatal("a mode with no lists governs nothing")
	}
	if (Policy{TableMode: ModeDenylist, Tables: "\n  \n"}).Active() {
		t.Fatal("blank lines are not entries")
	}
	if !(Policy{TableMode: ModeDenylist, Tables: "secrets"}).Active() {
		t.Fatal("a named table makes the policy active")
	}
	if !(Policy{FieldMode: ModeDenylist, Fields: "users.password"}).Active() {
		t.Fatal("a masked column alone makes the policy active")
	}
}

func TestPolicyValidate(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
		ok     bool
	}{
		{"blank is a tool without a policy", Policy{}, true},
		{"a list with no rule is read as a denylist, the way the rest of this reads one", Policy{Tables: "secrets"}, true},
		{"an allowlist must name something", Policy{TableMode: ModeAllowlist}, false},
		{"an allowlist that names something", Policy{TableMode: ModeAllowlist, Tables: "orders"}, true},
		{"an empty denylist is a tool that hides nothing", Policy{TableMode: ModeDenylist}, true},
		{"an unknown mode", Policy{TableMode: "sometimes", Tables: "x"}, false},
		{"and so is a field list", Policy{Fields: "users.password"}, true},
		{"a field denylist alone needs no table mode", Policy{FieldMode: ModeDenylist, Fields: "users.password"}, true},
		{"a field allowlist must name something", Policy{FieldMode: ModeAllowlist}, false},
		{"a field must name its table", Policy{FieldMode: ModeDenylist, Fields: "password"}, false},
		{"a field entry missing the name after the dot", Policy{FieldMode: ModeDenylist, Fields: "users."}, false},
		{"a field entry with too many parts", Policy{FieldMode: ModeDenylist, Fields: "db.users.password"}, false},
		{"a field entry missing its column", Policy{FieldMode: ModeDenylist, Fields: "users."}, false},
		{"a field entry missing its table", Policy{FieldMode: ModeDenylist, Fields: ".password"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.Validate()
			if c.ok && err != nil {
				t.Fatalf("expected it to be accepted, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected it to be refused")
			}
		})
	}
}

func TestDenylistPermitsEverythingItDoesNotName(t *testing.T) {
	p := Policy{TableMode: ModeDenylist, Tables: "secret_keys\n\naudit_log\nlog_*\n"}
	for _, name := range []string{"secret_keys", "SECRET_KEYS", "Secret_Keys", "audit_log", "log_2026", "log_"} {
		if p.AllowsTable(name) {
			t.Errorf("%q is denied and must stay denied however it is written", name)
		}
	}
	for _, name := range []string{"orders", "customers", "catalog", "backlog"} {
		if !p.AllowsTable(name) {
			t.Errorf("%q is not on the list and must be permitted", name)
		}
	}
}

func TestAllowlistPermitsOnlyWhatItNames(t *testing.T) {
	p := Policy{TableMode: ModeAllowlist, Tables: "orders\ncustomers\nreport_*"}
	for _, name := range []string{"orders", "customers", "report_daily", "REPORT_x"} {
		if !p.AllowsTable(name) {
			t.Errorf("%q is named and must be permitted", name)
		}
	}
	for _, name := range []string{"secret_keys", "orders_archive", "daily_report", "", "*"} {
		if p.AllowsTable(name) {
			t.Errorf("%q is not named and must be refused", name)
		}
	}
}

func TestMaskedColumnsMatchOnBothSides(t *testing.T) {
	p := Policy{FieldMode: ModeDenylist, Fields: "*.password\ncustomers.ssn\nusers.*\n*.secret_*"}
	masked := []struct{ table, column string }{
		{"users", "password"},      // *.column hides it in every table, said on purpose
		{"customers", "password"},  //
		{"CUSTOMERS", "SSN"},       // case is not a way past it
		{"users", "anything"},      // every column of one table
		{"orders", "secret_token"}, // a pattern on the column side
		{"anything", "secret_x"},
	}
	for _, m := range masked {
		if !p.MasksColumn(m.table, m.column) {
			t.Errorf("%s.%s must be hidden", m.table, m.column)
		}
	}
	visible := []struct{ table, column string }{
		{"orders", "id"},
		{"customers", "email"},
		{"orders", "ssn"},    // scoped to customers
		{"orders", "secret"}, // secret_* needs the underscore
	}
	for _, v := range visible {
		if p.MasksColumn(v.table, v.column) {
			t.Errorf("%s.%s must stay visible", v.table, v.column)
		}
	}
}

func TestCouldHideColumnIsTheCheapFirstHalf(t *testing.T) {
	p := Policy{FieldMode: ModeDenylist, Fields: "customers.ssn"}
	if !p.CouldHideColumn("ssn") {
		t.Fatal("ssn is named by an entry, so an unqualified ssn must be resolved rather than waved through")
	}
	if p.CouldHideColumn("email") {
		t.Fatal("email is named by no entry, so it never needs resolving")
	}
	if !p.MasksColumn("customers", "ssn") || p.MasksColumn("orders", "ssn") {
		t.Fatal("the entry is scoped to one table")
	}
	// Under an allowlist there is no shortcut: not being on the list is what
	// hides a column, so every name has to be traced back to its table.
	allow := Policy{FieldMode: ModeAllowlist, Fields: "customers.id"}
	for _, column := range []string{"id", "email", "anything"} {
		if !allow.CouldHideColumn(column) {
			t.Errorf("%q could be hidden by an allowlist and must be resolved", column)
		}
	}
}

func TestAFieldAllowlistShowsOnlyWhatItNamesAndOnlyWhereItNamesIt(t *testing.T) {
	p := Policy{FieldMode: ModeAllowlist, Fields: "customers.id\ncustomers.email\nreport_*.total"}
	visible := []struct{ table, column string }{
		{"customers", "id"},
		{"customers", "email"},
		{"CUSTOMERS", "Email"},
		{"report_daily", "total"},
		// A table the list never mentions is not governed by it at all, which is
		// what stops one named column from blanking the whole database.
		{"orders", "id"},
		{"orders", "total"},
		{"staff", "salary"},
	}
	for _, v := range visible {
		if p.MasksColumn(v.table, v.column) {
			t.Errorf("%s.%s must stay visible", v.table, v.column)
		}
	}
	hidden := []struct{ table, column string }{
		{"customers", "ssn"},
		{"customers", "profile"},
		// A column added to a governed table later is hidden the day it appears.
		{"customers", "added_next_year"},
		{"report_daily", "notes"},
	}
	for _, h := range hidden {
		if !p.MasksColumn(h.table, h.column) {
			t.Errorf("%s.%s must be hidden", h.table, h.column)
		}
	}
}

func TestMasksAnyColumnDecidesWhetherAStarWouldChangeAnything(t *testing.T) {
	p := Policy{FieldMode: ModeDenylist, Fields: "customers.ssn"}
	if !p.MasksAnyColumn("customers", []string{"id", "email", "ssn"}) {
		t.Fatal("customers has a hidden column, so its * must be expanded")
	}
	if p.MasksAnyColumn("orders", []string{"id", "total"}) {
		t.Fatal("orders has none, so its * is left exactly as it was written")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"users", "users", true},
		{"users", "users2", false},
		{"log_*", "log_2026", true},
		{"log_*", "log_", true},
		{"log_*", "catalog_x", false},
		{"*_log", "audit_log", true},
		{"*_log", "log", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"*secret*", "my_secret_column", true},
		{"*secret*", "secret", true},
		// The suffix may not overlap what an earlier part already consumed.
		{"a*a", "a", false},
		{"ab*ba", "abba", true},
	}
	for _, c := range cases {
		if got := match(c.pattern, c.name); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// A refusal from the form is read by somebody looking at the form, so it has to
// say what to change and what each way of changing it means. "Choose whether the
// field list names the fields this tool may show or the ones it may not" told
// them neither, while the control it meant was on screen showing a rule.
func TestARefusalSaysWhatToDoAboutIt(t *testing.T) {
	cases := []struct {
		policy Policy
		says   []string
	}{
		// Each one names the control on screen, says what to do, and shows what
		// the answer looks like.
		{Policy{TableMode: ModeAllowlist}, []string{"Table rule", "Tables box", "one per line", "denylist instead"}},
		{Policy{FieldMode: ModeAllowlist}, []string{"Field rule", "Fields box", "users.email", "denylist instead"}},
		{Policy{TableMode: "sometimes", Tables: "x"}, []string{"Table rule", "denylist or allowlist"}},
		{Policy{FieldMode: "sometimes", Fields: "users.email"}, []string{"Field rule", "denylist or allowlist"}},
		{Policy{FieldMode: ModeDenylist, Fields: "password"}, []string{"users.password", "*.password"}},
		{Policy{FieldMode: ModeDenylist, Fields: "a.b.c"}, []string{"too many dots", "users.email"}},
		{Policy{FieldMode: ModeDenylist, Fields: "users."}, []string{"missing the field name", "users.email"}},
		{Policy{FieldMode: ModeDenylist, Fields: ".password"}, []string{"missing the table name", "users.email"}},
	}
	for _, c := range cases {
		err := c.policy.Validate()
		if err == nil {
			t.Fatalf("%+v should have been refused", c.policy)
		}
		for _, phrase := range c.says {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("%+v\n  said  %q\n  wanted it to mention %q", c.policy, err, phrase)
			}
		}
	}
}
