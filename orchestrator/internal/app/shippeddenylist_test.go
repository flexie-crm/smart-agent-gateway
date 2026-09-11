package app

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
)

// What a terminal refuses on the day it is switched on, before anybody has
// configured anything. Both editions ship the same tool, so this is one answer
// for both.
func TestAFreshTerminalAlreadyRefusesWhatCannotBeUndone(t *testing.T) {
	resolved, failed := terminalPolicy(terminalSchema(t), nil)
	if failed != nil {
		t.Fatalf("the declared settings could not be resolved: %v", failed)
	}
	ready := resolved.WithDefaults()

	refused := []string{
		"rm -rf /",
		"rm -rf ~/Documents",
		"shutdown -h now",
		"reboot",
		"dd if=/dev/zero of=/dev/disk0",
		"mkfs.ext4 /dev/sda1",
		"diskutil eraseDisk JHFS+ gone disk2",
		"passwd root",
		// Windows, where the same damage has other names.
		"del /f /s /q C:\\",
		"vssadmin delete shadows /all",
		// And the ways round a list of names that reading the command closes.
		"ls | xargs rm -rf",
		"echo hi && rm -rf /tmp/x",
		"/bin/rm -rf /tmp/x",
		"find . -exec rm {} ;",
	}
	for _, command := range refused {
		if reason := ready.Check(command); reason == "" {
			t.Errorf("a fresh terminal permitted %q", command)
		}
	}

	// And it can still do the work people install it for. A default that
	// refuses ordinary commands is a tool people switch off, and then nothing
	// is denied at all.
	permitted := []string{
		"echo ok",
		"ls -la",
		"git status",
		"npm run build",
		"go test ./...",
		"cat README.md",
		"mkdir -p build && cd build",
		"grep -rn TODO src | head",
		"mv old.txt new.txt",
		"chmod +x script.sh",
		"kill 1234",
		"docker compose up -d",
	}
	for _, command := range permitted {
		if reason := ready.Check(command); reason != "" {
			t.Errorf("a fresh terminal refused %q: %s", command, reason)
		}
	}
}

// The headings in the shipped list are notes, not commands. Without this they
// became entries and were shown to the model as refused commands.
func TestTheShippedListsHeadingsAreNotCommands(t *testing.T) {
	for _, entry := range (cmdpolicy.Policy{Denied: cmdpolicy.DefaultDenied}).DeniedEntries() {
		if strings.HasPrefix(entry[0], "#") {
			t.Fatalf("a heading became a denied command: %q", strings.Join(entry, " "))
		}
		if len(entry) != 1 {
			t.Fatalf("a shipped entry is not a bare program name: %q", strings.Join(entry, " "))
		}
	}
}

// An administrator owns the list from then on, including emptying it.
func TestAnAdministratorCanTakeTheDefaultsBackOut(t *testing.T) {
	resolved, _ := terminalPolicy(terminalSchema(t),
		json.RawMessage(`{"policy":{"mode":"denylist","denied":""}}`))
	open := resolved.WithDefaults()
	if reason := open.Check("rm -rf /tmp/mine"); reason != "" {
		t.Fatalf("an emptied denylist still refused rm: %s", reason)
	}
}
