// Package sqlguard is what a database tool may see of the database it is
// connected to.
//
// A connection gives an assistant everything the account can reach. That is the
// right default for a database somebody set up for it, and the wrong one for a
// production database with a payroll table in it. This package is how an
// administrator says which tables are out of reach and which columns come back
// hidden, and it enforces that by READING THE STATEMENT rather than looking at
// its text: the statement is parsed, every table it would touch is resolved
// (through views, through CTEs, through subqueries), and the decision is made on
// what would actually run.
//
// The rule is the one the query tool's access gate and the server tool's command
// policy already follow: allow only what is recognised. A statement this package
// cannot read in full is refused, because nothing here could honestly say what
// it would do.
//
// What this is not: the security boundary. That is the account the tool signs in
// as, and an administrator who denies a table here should also take the grant
// away. This stops a mistake, closes the routes a text match would miss, and
// gives the model a refusal it can report. Hiding a VALUE while leaving the row
// readable is the one thing a grant cannot express, which is why masking lives
// here and nowhere else.
package sqlguard

import (
	"fmt"
	"strings"
)

// Hidden is what a hidden field comes back as. It is a value no field could be
// mistaken for, so a reader of the result is never left wondering whether the
// row really said that. It is here rather than with an analyzer because it is
// the same answer whatever the database.
const Hidden = "[hidden]"

// Mode is how the table list is read: an allowlist permits only what it names, a
// denylist permits everything except what it names.
type Mode string

const (
	ModeAllowlist Mode = "allowlist"
	ModeDenylist  Mode = "denylist"
)

// Policy is what an administrator permits a tool to see: which tables it may
// reach, and which of their columns come back with a value in them. The lists
// are kept as they were typed, one entry per line, so the edit form shows back
// exactly what was entered (the same shape the server tool's command policy
// uses).
//
// Each list is read the way its own mode says. Tables: an allowlist names the
// only tables that may be reached, a denylist names the ones that may not.
// Fields: a denylist names the columns that come back hidden; an allowlist names
// the columns that come back at all, and is scoped to the tables it mentions, so
// a table the field list never names keeps every column. That scoping is what
// makes an allowlist usable: naming one column of one table hides the rest of
// that table, not the rest of the database.
//
// A name may use * to stand for any run of characters, so log_* covers every
// table whose name starts with log_. A field entry is written table.column, and
// either side may be a pattern: customers.* is every column of one table,
// *.password is that column wherever it appears. The table half is never
// optional, because a bare column name would quietly mean "wherever this turns
// up", which is a much wider rule than somebody typing a column name is likely
// to have meant.
type Policy struct {
	TableMode Mode   `json:"table_mode,omitempty"`
	Tables    string `json:"tables,omitempty"`
	FieldMode Mode   `json:"field_mode,omitempty"`
	Fields    string `json:"fields,omitempty"`
}

// Active reports whether this policy governs anything. A tool that names no
// table and no column is unrestricted, and is left exactly as it was: the
// statement is not parsed, nothing is rewritten, and the tool behaves as it did
// before there was a policy at all.
func (p Policy) Active() bool {
	return len(p.tableEntries()) > 0 || len(p.fieldEntries()) > 0
}

// Validate reports what is wrong with a policy, in words meant for the person
// filling in the form.
//
// A blank rule is not one of the things that can be wrong. Everything in this
// package that reads a rule already treats a blank one as a denylist (see
// AllowsTable and MasksColumn: allowlist is the case they test for, and
// everything else is the other one), which is also what a new tool starts as and
// what the form shows. Refusing it here would have been this one function
// disagreeing with the rest of the package about a question the rest had already
// settled, and the person on the form paying for the disagreement.
func (p Policy) Validate() error {
	if err := mode(p.TableMode, len(p.tableEntries()),
		"the Table rule is allowlist, so the Tables box cannot be empty. List the tables this tool may use, one per line. To let it use every table, set the Table rule to denylist instead.",
		"the Table rule must be denylist or allowlist"); err != nil {
		return err
	}
	if err := mode(p.FieldMode, len(p.fieldEntries()),
		"the Field rule is allowlist, so the Fields box cannot be empty. List the fields this tool may show, one per line, each as table.field (for example users.email). To hide nothing, set the Field rule to denylist instead.",
		"the Field rule must be denylist or allowlist"); err != nil {
		return err
	}
	for _, entry := range p.fieldEntries() {
		if entry.err != "" {
			return fmt.Errorf("%s", entry.err)
		}
	}
	return nil
}

// mode checks one list against the rule it is read by. An allowlist naming
// nothing is the one combination that cannot mean anything: it would put every
// table, or every field, out of reach.
func mode(m Mode, entries int, empty, unknown string) error {
	switch m {
	case "", ModeDenylist:
	case ModeAllowlist:
		if entries == 0 {
			return fmt.Errorf("%s", empty)
		}
	default:
		return fmt.Errorf("%s", unknown)
	}
	return nil
}

// AllowsTable reports whether a table may be reached at all. The name is the one
// the database itself reports, resolved from the statement beforehand, so a
// difference of quoting or case cannot decide this.
func (p Policy) AllowsTable(name string) bool {
	name = strings.ToLower(name)
	listed := false
	for _, entry := range p.tableEntries() {
		if match(entry, name) {
			listed = true
			break
		}
	}
	if p.TableMode == ModeAllowlist {
		return listed
	}
	return !listed
}

// MasksColumn reports whether a column of a table comes back hidden.
//
// Under an allowlist a column is hidden unless it was named, but only for a
// table the list mentions at all. So a column added to a governed table next
// year is hidden the day it appears, which is the whole reason to write the list
// that way round.
func (p Policy) MasksColumn(table, column string) bool {
	table, column = strings.ToLower(table), strings.ToLower(column)
	governed, named := false, false
	for _, entry := range p.fieldEntries() {
		if entry.err != "" || !match(entry.table, table) {
			continue
		}
		governed = true
		if match(entry.column, column) {
			named = true
			break
		}
	}
	if p.FieldMode == ModeAllowlist {
		return governed && !named
	}
	return named
}

// CouldHideColumn reports whether any rule could apply to a column of this name,
// whatever table it turns out to belong to. It is the cheap first half of
// resolving an unqualified column: a name no rule could reach needs no resolving
// at all, and only a name that could forces the guard to work out which table it
// came from (and to refuse when it cannot).
//
// Under an allowlist every name could, because being absent from the list is
// what hides a column there.
func (p Policy) CouldHideColumn(column string) bool {
	entries := p.fieldEntries()
	if p.FieldMode == ModeAllowlist {
		return len(entries) > 0
	}
	column = strings.ToLower(column)
	for _, entry := range entries {
		if entry.err == "" && match(entry.column, column) {
			return true
		}
	}
	return false
}

// MasksAnyColumn reports whether a table has a column that comes back hidden, so
// a caller knows whether expanding a * over it would change anything.
func (p Policy) MasksAnyColumn(table string, columns []string) bool {
	for _, c := range columns {
		if p.MasksColumn(table, c) {
			return true
		}
	}
	return false
}

// maskEntry is one line of the masked list, split into the two patterns it
// matches on. A line that cannot be read carries the reason, so the form can say
// which line is wrong rather than refusing the whole list.
type maskEntry struct {
	table  string
	column string
	err    string
}

func (p Policy) tableEntries() []string { return lines(p.Tables) }

func (p Policy) fieldEntries() []maskEntry {
	var out []maskEntry
	for _, line := range lines(p.Fields) {
		parts := strings.Split(line, ".")
		switch {
		case len(parts) == 1:
			// A field is always named with the table it belongs to. A bare name
			// would quietly mean "wherever this turns up", which is a much wider
			// rule than somebody typing a field name is likely to have meant.
			// Writing *.name says it on purpose.
			out = append(out, maskEntry{err: fmt.Sprintf(
				"write %q as table.field, for example users.%s. To cover that field in every table, write *.%s.", line, line, line)})
		case len(parts) > 2:
			out = append(out, maskEntry{err: fmt.Sprintf(
				"%q has too many dots. Write each line as table.field, for example users.email.", line)})
		case parts[1] == "":
			out = append(out, maskEntry{err: fmt.Sprintf(
				"%q is missing the field name after the dot. Write each line as table.field, for example users.email.", line)})
		case parts[0] == "":
			out = append(out, maskEntry{err: fmt.Sprintf(
				"%q is missing the table name before the dot. Write each line as table.field, for example users.email.", line)})
		default:
			out = append(out, maskEntry{table: parts[0], column: parts[1]})
		}
	}
	return out
}

// lines splits a list into one entry per line, lowercased, with blank lines
// skipped so an administrator can space the list out. An empty entry in a
// matcher is the worst kind of bug, because it matches by accident.
func lines(list string) []string {
	var out []string
	for _, line := range strings.Split(list, "\n") {
		if trimmed := strings.ToLower(strings.TrimSpace(line)); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// match reports whether a name matches a pattern, where * stands for any run of
// characters, including none. It is deliberately the only wildcard: a database
// identifier can contain almost anything, and a richer pattern language would
// make a policy harder to read than the thing it protects.
func match(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	// The first and last parts are anchored; the ones between may appear
	// anywhere after what came before them.
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(name, part)
		if i < 0 {
			return false
		}
		name = name[i+len(part):]
	}
	return strings.HasSuffix(name, last) && len(name) >= len(last)
}
