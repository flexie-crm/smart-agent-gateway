package cmdpolicy

import (
	"strings"
	"testing"
)

// The shipped denylist, checked as BEHAVIOUR rather than as text. A list that
// looks right in a form and refuses nothing is the failure worth catching.
func TestTheShippedListRefusesWhatCannotBeUndone(t *testing.T) {
	ready := Policy{Mode: PolicyDenylist, Denied: DefaultDenied}.WithDefaults()

	for _, command := range []string{
		"rm -rf /", "rm -rf ~/Documents", "/bin/rm -rf /tmp/x",
		"shred -u secrets.txt", "dd if=/dev/zero of=/dev/disk0",
		"mkfs.ext4 /dev/sda1", "wipefs -a /dev/sda", "diskutil eraseDisk JHFS+ gone disk2",
		"shutdown -h now", "reboot", "poweroff", "init 0",
		"iptables -F", "ufw disable",
		"userdel alice", "passwd root",
		// Windows, where the same damage has other names.
		"del /f /s /q C:\\", "vssadmin delete shadows /all", "bcdedit /set safeboot minimal",
		// And the ways round a list of names that reading the command closes.
		"ls | xargs rm -rf", "echo hi && rm -rf /tmp/x", "find . -exec rm {} ;",
	} {
		if reason := ready.Check(command); reason == "" {
			t.Errorf("the shipped list permitted %q", command)
		}
	}
}

// And it can still do the work people install these tools for. A default that
// refuses ordinary commands is one people switch off, and then nothing is
// denied at all.
func TestTheShippedListLeavesOrdinaryWorkAlone(t *testing.T) {
	ready := Policy{Mode: PolicyDenylist, Denied: DefaultDenied}.WithDefaults()

	for _, command := range []string{
		"echo ok", "ls -la", "git status", "git push origin main",
		"npm run build", "go test ./...", "cat README.md", "tail -f app.log",
		"mkdir -p build", "mv old.txt new.txt", "cp a b", "chmod +x run.sh",
		"kill 1234", "docker compose up -d", "systemctl status app",
		"grep -rn TODO src | head",
	} {
		if reason := ready.Check(command); reason != "" {
			t.Errorf("the shipped list refused %q: %s", command, reason)
		}
	}
}

// Headings and blank lines are how the list is made readable, and neither is a
// command. Without this the heading itself became an entry, harmless to match
// and wrong everywhere the policy is displayed.
func TestTheListsNotesAreNotCommands(t *testing.T) {
	entries := Policy{Denied: DefaultDenied}.DeniedEntries()
	if len(entries) < 20 {
		t.Fatalf("only %d entries survived parsing", len(entries))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry[0], "#") {
			t.Fatalf("a heading became a denied command: %q", strings.Join(entry, " "))
		}
		if len(entry) != 1 {
			t.Fatalf("a shipped entry is not a bare program name: %q", strings.Join(entry, " "))
		}
	}
}
