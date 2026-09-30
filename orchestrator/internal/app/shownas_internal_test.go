package app

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// What a projected tool is CALLED, once a workspace has more than one
// connection.
//
// "Flexie Search" does not say whose search it is. Two connected services can
// both offer one, and the assistant reporting that it used "Search" left a
// person with no way to tell which service had just been reached. The name
// carries the connection now.

func TestAProjectedToolIsNamedAfterItsConnection(t *testing.T) {
	if got, want := shownAs("NLI", "Flexie Search"), "NLI / Flexie Search"; got != want {
		t.Fatalf("shownAs = %q, want %q", got, want)
	}
}

// The control, and the reason there is a condition at all: a built-in IS the
// product, so prefixing it would name the product inside its own interface.
func TestAToolOfOursIsLeftAlone(t *testing.T) {
	if got, want := shownAs("", "Fetch a web page"), "Fetch a web page"; got != want {
		t.Fatalf("shownAs = %q, want %q", got, want)
	}
	if strings.Contains(shownAs("", "Fetch a web page"), "/") {
		t.Fatal("a built-in was given a prefix")
	}
}

// Two columns joined into one have to fit the column they are stored in.
//
// Both halves are varchar(255) and the result is written to a varchar(255)
// (agent_tool_calls.friendly_name), so an unbounded join reaches 513
// characters. The database runs in STRICT_TRANS_TABLES, where that is error
// 1406 and a failed insert, which would take the tool call down with it rather
// than merely shortening a label.
func TestALongPairStillFitsTheColumnItIsStoredIn(t *testing.T) {
	const column = 255
	service := strings.Repeat("s", column)
	friendly := strings.Repeat("f", column)

	got := shownAs(service, friendly)
	if len(got) > column {
		t.Fatalf("shownAs produced %d bytes, which does not fit varchar(%d)", len(got), column)
	}
	// The service is the half that tells two connections apart, so it is the
	// half that survives.
	if !strings.HasPrefix(got, service[:20]) {
		t.Fatalf("the connection's name was the part cut: %q", got[:40])
	}
}

// And it is cut BETWEEN characters, never through one.
//
// A name is somebody's text and can be in any script. Cutting a multi-byte
// rune in half produces a string that is not valid UTF-8, which MariaDB
// refuses on a utf8mb4 column, so a careless bound fails in exactly the way
// the bound exists to prevent.
func TestTheCutLandsOnACharacterBoundary(t *testing.T) {
	// Three bytes per character, so a naive cut at 254 lands mid-character.
	friendly := strings.Repeat("あ", 200)
	got := shownAs(strings.Repeat("s", 100), friendly)

	if len(got) > 255 {
		t.Fatalf("%d bytes does not fit", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("the cut went through a character: %q", got)
	}
}
