package licences

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// The browser is listed, with the notice its own build ships.
//
// BSD-3-Clause asks for one thing in return for shipping a binary: reproduce
// the notice in the materials that go with it. The desktop editions carry a
// headless Chromium and run it as a separate program, so that obligation is
// ours, and this screen is the only place it can be discharged.
//
// The assertions are about the SHAPE of the notice rather than its length,
// because a file that was truncated on the way into the repository would still
// be megabytes long. Chromium's own grant has to be there, and so do the
// bundled third-party notices that follow it: the file is Chromium plus a few
// hundred libraries, and copying only the first section would be a notice that
// omits almost everything it is a notice for.
func TestTheBrowserIsListedWithTheNoticeItShips(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if !strings.Contains(c.Name, "Headless Shell") {
			continue
		}
		if c.Licence != "BSD-3-Clause" {
			t.Errorf("the browser is listed as %q", c.Licence)
		}
		if !strings.Contains(c.Text, "The Chromium Project") {
			t.Error("Chromium's own notice is not there")
		}
		if !strings.Contains(c.Text, "Redistribution and use in source and binary forms") {
			t.Error("the grant we are relying on to ship it is not in the text")
		}
		// The bundled notices. Two that are far apart in the file, so a copy
		// that stopped early fails rather than passing on its first section.
		if !strings.Contains(c.Text, "@bufbuild/protobuf") {
			t.Error("the bundled third-party notices are missing or truncated")
		}
		if c.Version != browserVersion {
			t.Errorf("listed as version %q, and the notice is for %q", c.Version, browserVersion)
		}
		return
	}
	t.Fatal("the browser is not listed, and the desktop editions carry it")
}

// The version written beside the notice is the version that is pinned.
//
// The pin lives in desktop/browser.json and cannot be embedded here: it is
// outside this module, and go:embed does not reach out of one. So the constant
// is a copy, and this is what makes a copy safe. Without it, bumping the pin
// and forgetting this file would publish the notices of a build nobody runs,
// which is the kind of wrong that looks right on the screen.
//
// The same arrangement, and the same reason, as the machine tools' agreement
// with desktop/link-tools.json.
func TestTheBrowserVersionMatchesThePin(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this test's own path")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "desktop", "browser.json"))
	if err != nil {
		t.Fatalf("the browser pin is missing: %v", err)
	}
	var pin struct {
		Chrome string `json:"chrome"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatalf("the browser pin is not readable: %v", err)
	}
	if pin.Chrome == "" {
		t.Fatal("the pin names no browser version")
	}
	if pin.Chrome != browserVersion {
		t.Errorf("desktop/browser.json pins %q and this package says %q: the notice on the "+
			"open source screen would be for a build nobody is running",
			pin.Chrome, browserVersion)
	}
}

// Playwright is listed, with the attribution its own NOTICE carries.
//
// Apache 2.0 section 4(d) is the reason this is not just a licence file: a
// redistribution has to reproduce the attribution in the work's NOTICE, and
// Playwright's says two things, not one. It is Microsoft's, AND it contains
// code derived from Puppeteer. Shipping only the licence text would credit the
// first and silently drop the second.
//
// It is compiled into the desktop applications rather than downloaded, which is
// what makes this our obligation: it goes out inside something we distribute.
func TestPlaywrightIsListedWithTheAttributionItsNoticeCarries(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Name != "Playwright" {
			continue
		}
		if c.Licence != "Apache-2.0" {
			t.Errorf("Playwright is listed as %q", c.Licence)
		}
		if !strings.Contains(c.Text, "Copyright (c) Microsoft Corporation") {
			t.Error("the notice's own attribution is not there")
		}
		if !strings.Contains(c.Text, "Puppeteer") {
			t.Error("the second upstream its notice credits is missing, which is what 4(d) asks for")
		}
		if !strings.Contains(c.Text, "Apache License") {
			t.Error("the licence text is not there in full")
		}
		return
	}
	t.Fatal("Playwright is not listed, and the desktop applications carry its script")
}
