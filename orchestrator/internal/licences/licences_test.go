package licences

import (
	"strings"
	"testing"
)

// The list is compiled in, so these run against exactly what would ship.
//
// What is worth testing here is not that a list exists: it is that the list is
// not quietly empty, and that the three things somebody could remove without
// noticing are all present. An attribution screen that shows nothing looks
// exactly like an attribution screen with nothing to show.

func TestEverythingCarriesItsLicenceInFull(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("read the list: %v", err)
	}
	// The generated half alone is over a hundred. A number far below that means
	// components.json was regenerated from a broken tree and committed empty,
	// which is the failure that would otherwise look like success.
	if len(all) < 100 {
		t.Fatalf("only %d components: the generated list looks empty or truncated", len(all))
	}

	var noText []string
	for _, c := range all {
		if c.Name == "" {
			t.Error("a component with no name")
		}
		if c.Part == "" {
			t.Errorf("%s: no part, so the screen cannot group it", c.Name)
		}
		if strings.TrimSpace(c.Text) == "" {
			noText = append(noText, c.Part+"/"+c.Name)
		}
	}
	// The whole point of the screen is the text. A component without one is not
	// fatal (some packages genuinely ship none) but it must not become normal.
	if len(noText) > 0 {
		t.Errorf("%d components carry no licence text: %s", len(noText), strings.Join(noText, ", "))
	}
}

// The three parts of the product each have to be represented, because each is
// something a person can install on its own.
func TestEveryPartOfTheProductIsCovered(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, c := range all {
		seen[c.Part]++
	}
	for _, part := range []string{"Server", "Console", "Chat", "Desktop"} {
		if seen[part] == 0 {
			t.Errorf("nothing listed for %q", part)
		}
	}
}

// The source held in lib/ is the half no manifest describes, so it is the half
// that would silently go missing. Its licences are embedded from the folders the
// code sits in, which is what makes that hard.
func TestTheSourceHeldInTheTreeIsListed(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ANTLR", "bytebase", "grammars-v4"} {
		found := false
		for _, c := range all {
			if strings.Contains(c.Name, want) {
				found = true
				if strings.TrimSpace(c.Text) == "" {
					t.Errorf("%s is listed with no licence text", c.Name)
				}
				if c.Note == "" {
					t.Errorf("%s does not say it is held in this repository", c.Name)
				}
			}
		}
		if !found {
			t.Errorf("nothing listed for %q, which is vendored in lib/sqlserver", want)
		}
	}
}

// MariaDB is the one component under a copyleft licence, and the only one whose
// licence asks for more than a notice. If this ever stops being listed with its
// source offer, the desktop editions are shipping a GPLv2 program with nothing
// saying where its source is.
func TestMariaDBIsListedWithItsSourceOffer(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if !strings.Contains(c.Name, "MariaDB") {
			continue
		}
		if c.Licence != "GPL-2.0" {
			t.Errorf("MariaDB is listed as %q", c.Licence)
		}
		if !strings.Contains(c.Text, "GNU GENERAL PUBLIC LICENSE") {
			t.Error("the GPL text is not there in full")
		}
		if !strings.Contains(c.Note, "source") {
			t.Error("no offer of source, which is what this licence asks for beyond a notice")
		}
		return
	}
	t.Fatal("MariaDB is not listed, and the desktop editions carry it")
}

// Reading it twice gives the same answer and does not append to itself. It is
// built once behind a sync.Once, and a mistake there would double the list on
// the second request rather than on the first.
func TestTheListIsStable(t *testing.T) {
	first, err := All()
	if err != nil {
		t.Fatal(err)
	}
	second, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("the list grew between reads: %d then %d", len(first), len(second))
	}
}
