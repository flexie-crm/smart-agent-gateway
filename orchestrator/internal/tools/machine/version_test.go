package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Every machine tool must have a version, and the failure if one does not is
// invisible: Offers compares what this build declares against what the
// application says it speaks, and a tool with no version here declares 0,
// which no application ever answers. The tool is then simply never offered.
// Nothing errors, nothing logs, and the ability quietly does not exist.
func TestEveryMachineToolDeclaresItsVersion(t *testing.T) {
	tools := Tools(nil)
	if len(tools) < 7 {
		t.Fatalf("this build ships %d machine tools; the file tools are missing", len(tools))
	}
	for _, machineTool := range tools {
		if versionOf(machineTool.Schema.Name) == 0 {
			t.Errorf("%q has no version, so no application will ever be offered it",
				machineTool.Schema.Name)
		}
	}
}

// And Runs answers for each of them, because the loadout asks it whether a
// tool needs a computer to run on.
func TestEveryMachineToolIsKnownToRunOnAComputer(t *testing.T) {
	for _, machineTool := range Tools(nil) {
		if !Runs(machineTool.Schema.Name) {
			t.Errorf("%q is a machine tool that Runs does not recognise", machineTool.Schema.Name)
		}
	}
}

// And what this gateway speaks, held against what the application speaks.
//
// The per-tool version exists so an application a release behind is not offered
// a tool it has never heard of (KB/39). What it cannot see is the two halves of
// ONE release disagreeing, and that is not hypothetical: the terminal grew
// `session` and `status` in the application, this side moved to version three
// for them, and the application's own constant stayed at two. Every test here
// passed, because every test here asks THIS side what it speaks and is told the
// same number twice. The link opened. The terminal was offered to nobody, and
// there is no error for that by design: an absent tool is the mechanism working.
//
// So the numbers live in a file both halves read (desktop/link-tools.json) and
// both halves assert. A Rust test does the same against the same file, so
// whichever half somebody forgets, a test fails rather than a tool disappearing.
func TestTheTwoHalvesAgreeOnEveryToolsVersion(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this test's own path")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "desktop", "link-tools.json"))
	if err != nil {
		t.Fatalf("the link tool contract is missing: %v", err)
	}
	var agreed map[string]int
	if err := json.Unmarshal(raw, &agreed); err != nil {
		t.Fatalf("the link tool contract is not readable: %v", err)
	}

	speaks := Versions()
	for name, version := range agreed {
		if speaks[name] != version {
			t.Errorf("the contract says %q is version %d and this gateway speaks %d: "+
				"an application built to the contract is offered the tool and this one refuses it, "+
				"or the other way round, and neither says so",
				name, version, speaks[name])
		}
	}
	for name, version := range speaks {
		if _, agreedOn := agreed[name]; !agreedOn {
			t.Errorf("this gateway speaks %q at %d, which the contract has never heard of: "+
				"no application will ever be offered it", name, version)
		}
	}
}
