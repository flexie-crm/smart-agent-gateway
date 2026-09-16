// Package query is the native tool template for querying a database. A custom
// tool created from this template is bound to one configured connection (a
// driver, a host, credentials sealed in its config) and runs SQL against it,
// within the access mode its creator chose.
package query

import (
	"fmt"
	"strings"
	"unicode"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
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

// CALL is deliberately NOT here. It was, which meant a MySQL or PostgreSQL tool
// could run any stored procedure from the day it was made, with nothing reading
// the body: the one kind of statement whose effect this package cannot see from
// its text. It is now a capability an administrator ticks, like EXEC on SQL
// Server, and off until they do.
var writeVerbs = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true,
	"RENAME": true, "LOAD": true, "IMPORT": true,
	"GRANT": true, "REVOKE": true, "SET": true, "MERGE": true,
}

// check enforces the access mode on a statement. It returns a model-safe reason
// when the statement is refused, which the tool turns into a bad-arguments
// result so the model corrects itself rather than the turn failing.
// unlocked is every leading keyword the ticked capabilities add, taken from the
// DRIVER rather than from a list here: CALL on MySQL, EXEC on SQL Server, and
// whatever a database added tomorrow says about itself.
//
// A verb only becomes allowed when its capability is ticked AND the engine
// offers it, so a setting carried over from another driver grants nothing.
func unlocked(settings Settings) map[string]bool {
	out := map[string]bool{}
	d, ok := datasource.Get(settings.Connection.Driver)
	if !ok {
		return out
	}
	for _, c := range d.Capabilities() {
		if !settings.May(c.Key) {
			continue
		}
		for _, v := range c.Verbs {
			out[strings.ToUpper(v)] = true
		}
	}
	return out
}

func check(mode Access, sql string, extra map[string]bool) (ok bool, reason string) {
	trimmed := stripLeading(sql)
	if trimmed == "" {
		return false, "The query was empty."
	}
	// The one-statement rule is oneStatement's, because answering it for a
	// routine body needs the dialect rather than a separator count.
	verb := leadingWord(trimmed)
	var k kind
	switch {
	case readVerbs[verb]:
		k = kindRead
	case writeVerbs[verb], extra[verb]:
		// A verb a capability unlocked is a WRITE: calling a routine can change
		// anything the routine touches, and nothing here can tell from the verb
		// whether it will. A tool set to read only stays read only.
		k = kindWrite
	default:
		k = kindUnknown
	}

	switch {
	case k == kindUnknown:
		return false, fmt.Sprintf("You are not permitted to run %s.", strings.ToUpper(verb))
	case k == kindRead && (mode == AccessRead || mode == AccessBoth):
		return true, ""
	case k == kindWrite && (mode == AccessWrite || mode == AccessBoth):
		return true, ""
	case k == kindRead && mode == AccessWrite:
		return false, fmt.Sprintf("You are not permitted to run %s here: this tool is set to writes only.", strings.ToUpper(verb))
	case k == kindWrite && mode == AccessRead:
		return false, fmt.Sprintf("You are not permitted to run %s here: this tool is read-only.", strings.ToUpper(verb))
	default:
		return false, fmt.Sprintf("You are not permitted to run %s here.", strings.ToUpper(verb))
	}
}

// permitted is the whole of the statement gate that runs before anything is
// opened: the access mode, then the object kinds an administrator has to tick
// for. Both are cheap and neither needs a database.
func permitted(settings Settings, sql string) (bool, string) {
	if ok, reason := oneStatement(settings, sql); !ok {
		return false, reason
	}
	if ok, reason := check(settings.Access, sql, unlocked(settings)); !ok {
		return false, reason
	}
	return objectAllowed(settings, sql)
}

// oneStatement enforces the one-statement rule, asking the DIALECT when the
// statement is one that creates stored code.
//
// Counting semicolons cannot answer that question and must not be asked to. A
// routine's body is made of statements and is part of the single statement that
// creates it, so the separator count refused every procedure worth writing:
//
//	CREATE PROCEDURE dbo.p AS BEGIN SET NOCOUNT ON; SELECT 1; END
//
// came back as several statements joined together. That is not a refusal a
// person works around, it is one they write worse SQL around: an agent given
// this tool wrote its validation with RAISERROR and RETURN because THROW needs a
// semicolon before it, and left out the semicolons the language expects.
//
// So where a dialect can read the text, it decides, and it still tells a body
// apart from a statement joined onto the end (measured on all three). Where it
// cannot, the separator count stands, which is all there is.
func oneStatement(settings Settings, sql string) (bool, string) {
	const joined = "Send one statement per call. Statements joined with ';' are not permitted."
	if !makesStoredCode(settings, sql) {
		if hasMultipleStatements(stripLeading(sql)) {
			return false, joined
		}
		return true, ""
	}
	if n, ok := sqlguard.CountStatements(settings.Connection.Driver, sql); ok {
		if n > 1 {
			return false, joined
		}
		return true, ""
	}
	// No reader for this dialect, or it could not read the text. A body's
	// semicolons cannot be told from a statement joined onto the end here, and
	// refusing every routine body is the worse of the two, because the body is
	// arbitrary code either way and the capability is what gates writing it.
	return true, ""
}

// makesStoredCode reports whether a statement creates, changes or removes the
// kind of object whose body is written in statements.
func makesStoredCode(settings Settings, sql string) bool {
	d, ok := datasource.Get(settings.Connection.Driver)
	if !ok {
		return false
	}
	words, readable := bareWords(sql)
	if !readable || len(words) == 0 {
		return false
	}
	switch words[0] {
	case "CREATE", "ALTER", "DROP":
	default:
		return false
	}
	for _, c := range d.Capabilities() {
		for _, object := range c.Objects {
			for _, word := range words[1:] {
				if word == strings.ToUpper(object) {
					return true
				}
			}
		}
	}
	return false
}

// objectAllowed refuses a statement that makes, changes or removes a kind of
// object a capability governs, unless that capability is ticked.
//
// The leading word cannot answer this on its own. CREATE is a write, so a tool
// allowed to write was allowed to CREATE TRIGGER from the day it was made, and a
// trigger runs on somebody else's write where no policy can see it. The object
// kind is what separates CREATE TABLE from CREATE TRIGGER.
//
// It reads WORDS rather than a parse, and that is not a shortcut. Measured: the
// MySQL parser this build carries cannot parse CREATE TRIGGER, CREATE FUNCTION,
// DROP TRIGGER, DROP FUNCTION or ALTER PROCEDURE at all, so a parse would have
// nothing to read for exactly the statements this exists to catch. This also has
// to work on a tool with NO policy, which is where nothing is parsed at all,
// because a guard is built to enforce a policy and a tool without one never
// reads its database's catalog.
//
// So it reads words, and it FAILS CLOSED. The first word after the verb that is
// not a modifier is the object kind: one a capability governs needs that
// capability, one this knows to be harmless is allowed, and anything else is
// refused. A list that is missing a word therefore costs a refusal somebody can
// report, never a hole.
//
// The first cut of this read the first four words and did not strip comments,
// which let CREATE /* four words of padding */ TRIGGER through.
func objectAllowed(settings Settings, sql string) (bool, string) {
	d, ok := datasource.Get(settings.Connection.Driver)
	if !ok {
		return true, ""
	}
	words, readable := bareWords(sql)
	if !readable {
		return false, "That could not be read as a statement. " +
			"Check the quoting and the comments and write it again."
	}
	if len(words) == 0 {
		return true, ""
	}
	switch words[0] {
	case "CREATE", "ALTER", "DROP":
	default:
		return true, ""
	}

	governed := map[string]datasource.Capability{}
	for _, c := range d.Capabilities() {
		for _, object := range c.Objects {
			governed[strings.ToUpper(object)] = c
		}
	}

	for _, word := range words[1:] {
		if ddlModifiers[word] {
			continue
		}
		if c, ok := governed[word]; ok {
			if settings.May(c.Key) {
				return true, ""
			}
			return false, "You are not permitted to " + strings.ToLower(words[0]) +
				" a " + objectWord(word) + "."
		}
		if plainObjects[word] {
			return true, ""
		}
		return false, "What that statement makes could not be established."
	}
	return false, "What that statement makes could not be established."
}

// ddlModifiers are the words that can stand between CREATE, ALTER or DROP and
// the kind of thing it names. Missing one costs a refusal, never a hole.
var ddlModifiers = map[string]bool{
	"OR": true, "REPLACE": true, "ALTER": true, "IF": true, "NOT": true, "EXISTS": true,
	"TEMP": true, "TEMPORARY": true, "UNLOGGED": true, "GLOBAL": true, "LOCAL": true,
	"UNIQUE": true, "CLUSTERED": true, "NONCLUSTERED": true, "COLUMNSTORE": true,
	"FULLTEXT": true, "SPATIAL": true, "MATERIALIZED": true, "RECURSIVE": true,
	"CONSTRAINT": true, "EVENT": true, "FOREIGN": true, "CONCURRENTLY": true, "ONLY": true,
	// MySQL writes a view's and a routine's attributes before the word VIEW.
	"DEFINER": true, "ALGORITHM": true, "SQL": true, "SECURITY": true, "INVOKER": true,
	"MERGE": true, "TEMPTABLE": true, "UNDEFINED": true, "CURRENT_USER": true,
}

// plainObjects are the kinds no capability governs, which are left exactly as
// they were. A kind that is on neither list is refused rather than guessed at.
var plainObjects = map[string]bool{
	"TABLE": true, "VIEW": true, "INDEX": true, "DATABASE": true, "SCHEMA": true,
	"SEQUENCE": true, "TYPE": true, "DOMAIN": true, "EXTENSION": true, "COLUMN": true,
	"USER": true, "ROLE": true, "GROUP": true, "TABLESPACE": true, "SERVER": true,
	"PUBLICATION": true, "SUBSCRIPTION": true, "POLICY": true, "RULE": true,
	"AGGREGATE": true, "OPERATOR": true, "COLLATION": true, "STATISTICS": true,
	"SYNONYM": true, "LOGIN": true, "CAST": true, "LANGUAGE": true, "CONVERSION": true,
}

// bareWords is a statement's words, upper-cased, with comments taken out and
// quoted things skipped. It reports false when the statement cannot be read to
// the end, which happens when a quote or a comment is never closed.
//
// Quoting is how a name that happens to be a keyword is written, so a quoted
// word is not a keyword: DROP TABLE `trigger` names a table. All three
// spellings are skipped whichever engine this is, because skipping one the
// engine does not have can only cost a refusal.
//
// An ordinary comment is not run by anybody, so its contents go. A comment that
// OPENS with /*! or /*M! is run by MariaDB and MySQL, so its contents STAY: a
// keyword hidden in one of those is a keyword the server will act on.
func bareWords(sql string) ([]string, bool) {
	var out []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			out = append(out, strings.ToUpper(word.String()))
			word.Reset()
		}
	}

	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' || r == '"' || r == '`' || r == '[':
			flush()
			end, ok := closingQuote(runes, i)
			if !ok {
				return nil, false
			}
			i = end
		case r == '-' && i+1 < len(runes) && runes[i+1] == '-',
			r == '#':
			flush()
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < len(runes) && runes[i+1] == '*':
			flush()
			end := closingBlockComment(runes, i)
			if end < 0 {
				return nil, false
			}
			if executableComment(runes[i:]) {
				// The server runs what is inside, so this side reads it too.
				// Skip only the opener and the closer.
				opener := i + 2
				for opener < len(runes) && runes[opener] != '!' {
					opener++
				}
				inner, ok := bareWordsOf(runes[opener+1 : end-1])
				if !ok {
					return nil, false
				}
				out = append(out, inner...)
			}
			i = end
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out, true
}

// bareWordsOf reads the inside of an executable comment.
func bareWordsOf(runes []rune) ([]string, bool) { return bareWords(string(runes)) }

// closingQuote is the index of the character that closes the quote opening at
// start. A doubled quote inside one is an escaped quote and does not close it,
// and a backslash escapes the next character in a string.
func closingQuote(runes []rune, start int) (int, bool) {
	open := runes[start]
	close := open
	if open == '[' {
		close = ']'
	}
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == '\\' && open == '\'' && i+1 < len(runes) {
			i++
			continue
		}
		if runes[i] != close {
			continue
		}
		if i+1 < len(runes) && runes[i+1] == close && open != '[' {
			i++
			continue
		}
		return i, true
	}
	return 0, false
}

// closingBlockComment is the index of the last character of the comment opening
// at start, or -1 when it is never closed. They do not nest on MySQL or SQL
// Server; PostgreSQL nests them, so the depth is counted, which is the safe
// reading for all three (a nested one read as flat ends the comment early and
// leaves the rest to be read as words, which can only refuse).
func closingBlockComment(runes []rune, start int) int {
	depth := 0
	for i := start; i+1 < len(runes); i++ {
		switch {
		case runes[i] == '/' && runes[i+1] == '*':
			depth++
			i++
		case runes[i] == '*' && runes[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// executableComment reports whether a comment opens /*! or /*M!, which MySQL and
// MariaDB RUN rather than ignore.
func executableComment(runes []rune) bool {
	if len(runes) < 3 {
		return false
	}
	if runes[2] == '!' {
		return true
	}
	return len(runes) > 3 && (runes[2] == 'M' || runes[2] == 'm') && runes[3] == '!'
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
			// Counted rather than closed at the first */, because PostgreSQL
			// NESTS block comments. Read flat, /* /* */ SELECT 1 */ CALL p()
			// looks like a SELECT to this side and is a CALL to the server, which
			// is how a routine ran on a tool that had not been allowed to run one.
			// Counting is the safe reading on the two engines that do not nest as
			// well: there the extra */ is left in the statement, which the engine
			// rejects and this refuses as unreadable.
			runes := []rune(s)
			end := closingBlockComment(runes, 0)
			if end < 0 {
				return ""
			}
			s = string(runes[end+1:])
		default:
			return s
		}
	}
}

// hasMultipleStatements reports whether more than one statement is present.
//
// It skips comments as well as quotes, and that is the load-bearing part. The
// first cut tracked three quote characters and knew nothing about comments, so
// an apostrophe INSIDE a comment opened a string that never closed and every
// later character was skipped, the real semicolon included. Measured:
//
//	SELECT 1 /* don't stop */ ; INSERT INTO scratch VALUES (97, 'smuggled')
//
// came back as one statement and the INSERT ran. A bracket-quoted T-SQL name
// containing an apostrophe, SELECT 1 AS [it's] ; ..., did the same.
//
// A statement that cannot be read to the end is reported as several, so it is
// refused rather than guessed at. The driver is also configured to reject
// stacked statements where it can; this is the belt to that suspenders, with a
// reason a person can act on.
func hasMultipleStatements(s string) bool {
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' || r == '"' || r == '`' || r == '[':
			end, ok := closingQuote(runes, i)
			if !ok {
				return true
			}
			i = end
		case r == '-' && i+1 < len(runes) && runes[i+1] == '-', r == '#':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < len(runes) && runes[i+1] == '*':
			if executableComment(runes[i:]) {
				// MySQL and MariaDB RUN what is inside one of these, so a
				// semicolon in there separates two statements just as it would
				// anywhere else. Step past the opener and keep reading.
				for i < len(runes) && runes[i] != '!' {
					i++
				}
				continue
			}
			end := closingBlockComment(runes, i)
			if end < 0 {
				return true
			}
			i = end
		case r == ';':
			if strings.TrimSpace(stripLeading(string(runes[i+1:]))) != "" {
				return true
			}
		}
	}
	return false
}

// callsRoutine reports whether a statement's leading word is one a capability
// unlocked, which is this package's way of asking "can this come back with
// rows even though it is not a read".
func callsRoutine(settings Settings, sql string) bool {
	return unlocked(settings)[leadingWord(stripLeading(sql))]
}

// objectWord is what to call the kind of thing a statement makes, in the words
// a person uses. The statement's own spelling is an abbreviation often enough
// that "you are not permitted to create a proc" reads like a typo.
func objectWord(word string) string {
	switch strings.ToUpper(word) {
	case "PROC":
		return "procedure"
	case "FUNC":
		return "function"
	}
	return strings.ToLower(word)
}
