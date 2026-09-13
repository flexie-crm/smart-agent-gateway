package main

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The passwordless sign-in is safe exactly while nothing but this machine can
// reach the port. That makes this function a security control rather than a
// convenience, so every way of saying "somewhere else can reach me" is pinned.
func TestRequireLoopback(t *testing.T) {
	cases := []struct {
		addr    string
		allowed bool
		why     string
	}{
		{"127.0.0.1:8080", true, "the address desktop mode chooses for itself"},
		{"localhost:8080", true, "the same thing by name"},
		{"[::1]:8080", true, "loopback over IPv6"},
		{"127.0.0.53:9000", true, "anything in the loopback range"},

		// The dangerous one, and the easiest to type: ":8080" is every address
		// on the machine, which is how a laptop becomes an open console.
		{":8080", false, "every address"},
		{"0.0.0.0:8080", false, "every address, spelled out"},
		{"[::]:8080", false, "every address over IPv6"},
		{"192.168.1.10:8080", false, "reachable from the network"},
		{"10.0.0.5:8080", false, "reachable from the network"},
		{"example.com:8080", false, "a name we cannot verify resolves only here"},
		{"nonsense", false, "not an address at all"},
	}

	for _, c := range cases {
		err := requireLoopback(c.addr)
		if c.allowed && err != nil {
			t.Errorf("%s (%s) was refused: %v", c.addr, c.why, err)
		}
		if !c.allowed && err == nil {
			t.Errorf("%s (%s) was allowed; a desktop on it signs anyone in without a password", c.addr, c.why)
		}
	}
}

func TestARefusedAddressSaysWhatToDoInstead(t *testing.T) {
	err := requireLoopback("0.0.0.0:8080")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// Somebody hitting this is mid-setup and needs the answer, not a diagnosis.
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the refusal does not say what to use instead: %v", err)
	}
}

func TestTheOwnerKeyIsMadeOnceAndKept(t *testing.T) {
	state := t.TempDir()

	first, err := ownerPassword(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 {
		t.Fatalf("the key is only %d characters", len(first))
	}

	// A second start is the same installation. A new key here would leave the
	// seeded owner unreachable, which is an application that installs fine and
	// then cannot be opened.
	again, err := ownerPassword(state)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatal("the owner key changed between calls; the seeded owner would be locked out")
	}

	info, err := os.Stat(filepath.Join(state, ownerKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	// It IS the credential, so what stands between it and everybody else is
	// worth asserting rather than assuming.
	//
	// On unix that is the file's own permissions. On Windows there are none to
	// assert: there is no POSIX mode, the one Go passes decides only the
	// read-only attribute, and every file reads back 0666. What protects it
	// there is the ACL the state directory inherits from %APPDATA%, which is
	// inside the user's own profile, and that is the same protection 0600 under
	// $HOME gives on a Mac, reached by the platform's own mechanism instead of
	// ours.
	//
	// Said out loud rather than skipped quietly, because a permission assertion
	// that silently does not run reads afterwards as one that passed.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("the owner key is %o, want 600", perm)
		}
	}
}

// The address this edition seeds has to be one the system would accept back.
//
// It is never typed and never sent to, so nothing about it is exercised by
// ordinary use until somebody opens their own row and renames themselves, at
// which point the user endpoint validates the whole record and refuses it. That
// is how "owner@localhost" got as far as a person's screen, saying "a valid
// email is required" about a field they had not touched.
//
// Asserted against the REAL rule (model.ValidEmail), which is the point: a test
// that restated the rule here would agree with a wrong constant just as happily.
func TestTheSeededAddressWouldBeAcceptedBack(t *testing.T) {
	if !model.ValidEmail(personalOwnerEmail) {
		t.Fatalf("the seeded owner's address %q would be refused by the endpoint that "+
			"validates it, so this person could not rename themselves", personalOwnerEmail)
	}
	// And it must not be somewhere a message could actually go: nothing is ever
	// sent here, and an address that resolves is one somebody could mistake for
	// an account that exists elsewhere.
	if !strings.HasSuffix(personalOwnerEmail, ".fx") && !strings.HasSuffix(personalOwnerEmail, ".invalid") {
		t.Fatalf("the seeded owner's address %q looks deliverable", personalOwnerEmail)
	}
}

// The address a personal installation calls itself by.
//
// This is not cosmetic. It is what an OAuth redirect is built from, so a wrong
// answer sends the person's browser, carrying a consent they have just granted,
// to a machine that is not this one.
func TestAPersonalInstallationCallsItselfByTheAddressItBound(t *testing.T) {
	// The port is chosen when the application starts, so a constant cannot be
	// right. This is the case that was broken: it said localhost:8080.
	if got := personalBaseURL("", "127.0.0.1:57311"); got != "http://127.0.0.1:57311" {
		t.Errorf("got %q, want the address it bound", got)
	}
	if got := personalBaseURL("", "localhost:8080"); got != "http://localhost:8080" {
		t.Errorf("got %q", got)
	}
}

func TestAnAddressSomebodyMeantIsLeftAlone(t *testing.T) {
	got := personalBaseURL("https://sag.example.test", "127.0.0.1:57311")
	if got != "https://sag.example.test" {
		t.Errorf("got %q, want what was configured", got)
	}
}

// The two settings have to agree, because the second is derived from the first
// and the first is the one with a guard on it. Anything requireLoopback accepts
// must produce an address a browser on this machine can reach.
func TestEveryAddressAllowedToListenGivesAReachableBaseURL(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.1:57311", "localhost:9000", "[::1]:8080"} {
		if err := requireLoopback(addr); err != nil {
			t.Fatalf("%s: the guard rejected an address this test assumes: %v", addr, err)
		}
		base := personalBaseURL("", addr)
		parsed, err := url.Parse(base)
		if err != nil {
			t.Errorf("%s gave %q, which is not an address: %v", addr, base, err)
			continue
		}
		if parsed.Host != addr {
			t.Errorf("%s gave host %q", addr, parsed.Host)
		}
		if parsed.Port() == "" {
			t.Errorf("%s gave %q, which has no port: a browser would go to 80", addr, base)
		}
	}
}

// Moving an installation that predates the product's name.
//
// The folder is not cosmetic: it holds the database, the keys everything sealed
// was sealed with, and the models. Renaming the constant alone would leave a
// working installation looking brand new, with its conversations and its agents
// still on disk under a name nothing reads any more.
func TestAnInstallationUnderTheOldNameIsMovedRatherThanAbandoned(t *testing.T) {
	base := t.TempDir()
	earlier := filepath.Join(base, earlierDataDirName)
	if err := os.MkdirAll(earlier, 0o700); err != nil {
		t.Fatal(err)
	}
	// Something only the real installation would have.
	if err := os.WriteFile(filepath.Join(earlier, "encryption.key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, personalDataDirName)
	if err := adoptEarlierDataDir(base, dir); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	key, err := os.ReadFile(filepath.Join(dir, "encryption.key"))
	if err != nil {
		t.Fatalf("the installation did not come with it: %v", err)
	}
	if string(key) != "secret" {
		t.Errorf("the key changed: %q", key)
	}
	if _, err := os.Stat(earlier); !os.IsNotExist(err) {
		t.Error("the old folder is still there, so there are now two installations")
	}
}

// Two data directories must NEVER be merged: the keys in one cannot open what
// was sealed with the other, so a merge is a database full of unreadable
// secrets. If the new one exists, it wins and nothing is touched.
func TestAnExistingInstallationIsNeverOverwrittenByAnOlderOne(t *testing.T) {
	base := t.TempDir()
	earlier := filepath.Join(base, earlierDataDirName)
	dir := filepath.Join(base, personalDataDirName)
	for _, d := range []string{earlier, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "encryption.key"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(earlier, "encryption.key"), []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := adoptEarlierDataDir(base, dir); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	key, _ := os.ReadFile(filepath.Join(dir, "encryption.key"))
	if string(key) != "current" {
		t.Errorf("the working installation was replaced by an older one: %q", key)
	}
}

// The common case, and it must be silent: a first run has neither folder.
func TestAFirstRunAdoptsNothingAndSaysSo(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, personalDataDirName)
	if err := adoptEarlierDataDir(base, dir); err != nil {
		t.Fatalf("a first run should not be an error: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("adoption created a directory; that is the caller's job")
	}
}

// What a fresh installation hands somebody, without them configuring anything.
//
// A deployment starts an assistant with nothing and an administrator decides
// what it may do. Nobody administers their own laptop, so these three defaults
// ARE the product on first run: every built-in on, the three that change the
// person's own disk asking first, and the model thinking as hard as it will.
//
// Pinned because they are silent: nothing fails if they drift, the assistant
// just quietly does less than it should, and the only way anybody finds out is
// by opening a screen they have no reason to open.
func TestAFreshInstallationArrivesUsable(t *testing.T) {
	all := []string{"brain", "brain_write", "current_time", "http_request",
		"machine_info", "read_file", "write_file", "edit_file", "find_files",
		"search_files", "terminal"}

	gw := gatewayFor(all)

	if len(gw.Tools) != len(all) {
		t.Errorf("the Gateway was given %d of %d built-in tools: %v", len(gw.Tools), len(all), gw.Tools)
	}
	for _, want := range []string{"terminal", "read_file", "write_file", "edit_file", "brain", "http_request"} {
		if !contains(gw.Tools, want) {
			t.Errorf("a fresh installation cannot %q", want)
		}
	}

	// Exactly the three that change something on the person's disk. Reading and
	// searching are answers; writing and running are actions.
	wantConfirm := []string{"terminal", "write_file", "edit_file"}
	if len(gw.ConfirmTools) != len(wantConfirm) {
		t.Errorf("confirmations were %v, wanted %v", gw.ConfirmTools, wantConfirm)
	}
	for _, want := range wantConfirm {
		if !contains(gw.ConfirmTools, want) {
			t.Errorf("%q runs on somebody's own machine without asking", want)
		}
	}
	// And the other side of it: a tool that only reads must NOT stop and ask,
	// or the assistant is a sequence of dialogs.
	for _, never := range []string{"read_file", "find_files", "search_files", "current_time"} {
		if contains(gw.ConfirmTools, never) {
			t.Errorf("%q asks permission to read, which makes every answer a dialog", never)
		}
	}

	if !gw.Reasoning {
		t.Error("reasoning is off, so the effort setting below is ignored")
	}
	if got := gw.Settings["reasoning_effort"]; got != "max" {
		t.Errorf("reasoning effort was %v, wanted max", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// gatewayFor builds the agent seedGateway would create, without a database.
func gatewayFor(builtins []string) *model.Agent {
	brainID := int64(1)
	return newGatewayAgent(7, brainID, builtins)
}
