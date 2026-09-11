package tool

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a tool may be called.
//
// This is not our preference, it is what the model APIs accept: OpenAI and
// every dialect that copies it take `^[a-zA-Z0-9_-]+$` up to 64 characters, and
// Anthropic the same to 128. 64 with that alphabet is the answer that works
// everywhere, so it is the rule here.
//
// It is enforced rather than trusted, and the reason is worth stating plainly:
// the tools are sent as ONE array, and a single name a vendor will not accept
// fails the WHOLE request. So one unusable tool does not cost you that tool, it
// costs you every tool the agent has, on every turn, as a 400 that reaches the
// person as "I could not reach the CRM". That is exactly how it was found
// (KB/29): remote tools were projected as `prefix.name`, and the dot is not in
// the alphabet.

// MaxNameLength is the longest name every vendor we support will accept.
const MaxNameLength = 64

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidName says whether a name can be put in front of a model, and when it
// cannot, says why in words an administrator can act on.
func ValidName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a tool must have a name")
	case utf8.RuneCountInString(name) > MaxNameLength:
		return fmt.Errorf("the name is %d characters, and a model accepts at most %d",
			utf8.RuneCountInString(name), MaxNameLength)
	case !namePattern.MatchString(name):
		return fmt.Errorf("the name may hold only letters, numbers, underscores and hyphens")
	}
	return nil
}

// UsableName rewrites a name we did NOT choose into the alphabet a model
// accepts, one character at a time.
//
// It exists for names that arrive from somewhere else, which in practice means
// a remote MCP server: what it calls its tools is its business, and refusing a
// perfectly good tool over a dot would be our problem becoming theirs. What we
// call it and what we CALL are two different strings, and the real one is kept
// beside it (`tools.remote_name`), so nothing is lost by renaming here.
//
// It does not fix length, and it cannot: shortening a name is guesswork, and
// two tools shortened the same way would become one. A name that is still
// unusable afterwards is reported, not repaired.
func UsableName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}
