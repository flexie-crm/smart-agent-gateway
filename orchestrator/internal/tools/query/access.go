// Package query is the native tool template for querying a database. A custom
// tool created from this template is bound to one configured connection (a
// driver, a host, credentials sealed in its config) and runs SQL against it,
// within the access mode its creator chose.
package query

import (
	"strings"
	"unicode"
)

// Access is what a query tool instance is allowed to do against its database.
// It is chosen when the tool is created and enforced on every statement, the
// first of the two guards (the second being a database credential scoped to
// match). READ is the safe default.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
	AccessBoth  Access = "both"
)

// Valid reports whether a is one of the three modes.
func (a Access) Valid() bool {
	return a == AccessRead || a == AccessWrite || a == AccessBoth
}

// kind classifies a statement by intent.
type kind int

const (
	kindUnknown kind = iota
	kindRead
	kindWrite
)

// readVerbs and writeVerbs classify a statement by its leading keyword. A verb
// not in either set is unknown and refused in every mode: the gate allows only
// what it recognises, never what it merely fails to recognise as dangerous.
var readVerbs = map[string]bool{
	"SELECT": true, "WITH": true, "EXPLAIN": true, "SHOW": true,
	"DESCRIBE": true, "DESC": true, "TABLE": true, "VALUES": true,
}

var writeVerbs = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true,
	"RENAME": true, "CALL": true, "LOAD": true, "IMPORT": true,
	"GRANT": true, "REVOKE": true, "SET": true, "MERGE": true,
}

// check enforces the access mode on a statement. It returns a model-safe reason
// when the statement is refused, which the tool turns into a bad-arguments
// result so the model corrects itself rather than the turn failing.
func check(mode Access, sql string) (ok bool, reason string) {
	trimmed := stripLeading(sql)
	if trimmed == "" {
		return false, "the query was empty"
	}
	if hasMultipleStatements(trimmed) {
		return false, "run one statement at a time; multiple statements separated by ';' are not allowed"
	}

	verb := leadingWord(trimmed)
	var k kind
	switch {
	case readVerbs[verb]:
		k = kindRead
	case writeVerbs[verb]:
		k = kindWrite
	default:
		k = kindUnknown
	}

	switch {
	case k == kindUnknown:
		return false, "that kind of statement is not allowed by this tool"
	case k == kindRead && (mode == AccessRead || mode == AccessBoth):
		return true, ""
	case k == kindWrite && (mode == AccessWrite || mode == AccessBoth):
		return true, ""
	case k == kindRead && mode == AccessWrite:
		return false, "this tool is configured for writes only; it cannot run a read query"
	case k == kindWrite && mode == AccessRead:
		return false, "this tool is read-only; it cannot change data"
	default:
		return false, "that statement is not allowed in this tool's access mode"
	}
}

// isReadOnly reports whether a statement only reads, so the runner can wrap it
// in a read-only transaction for an extra guarantee the database enforces.
func isReadOnly(sql string) bool {
	return readVerbs[leadingWord(stripLeading(sql))]
}

// leadingWord returns the upper-cased first keyword of a statement.
func leadingWord(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ';'
	})
	if end < 0 {
		end = len(s)
	}
	return strings.ToUpper(s[:end])
}

// stripLeading removes leading whitespace and leading SQL comments (-- to end
// of line, and /* */ blocks), so the classifier sees the real first keyword and
// a comment cannot smuggle a statement past it.
func stripLeading(s string) string {
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		switch {
		case strings.HasPrefix(s, "--"):
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "#"):
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*"):
			if end := strings.Index(s, "*/"); end >= 0 {
				s = s[end+2:]
			} else {
				return ""
			}
		default:
			return s
		}
	}
}

// hasMultipleStatements reports whether more than one statement is present. It
// scans for a ';' that is followed by more SQL, skipping semicolons inside
// string and identifier quotes so a value containing ';' does not trip it. The
// driver is also configured to reject stacked statements; this is the belt to
// that suspenders, with a clear reason.
func hasMultipleStatements(s string) bool {
	var quote rune
	for i, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case ';':
			if strings.TrimSpace(stripLeading(s[i+1:])) != "" {
				return true
			}
		}
	}
	return false
}
