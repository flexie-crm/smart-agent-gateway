package cmdpolicy

import (
	"strings"
	"testing"
)

// The policy is the only thing standing between an assistant and a shell on the
// server, so these are the tests that matter most. Each one is a way a text
// match would have been fooled: a second command after a semicolon, a pipeline,
// a substitution that decides on the server what runs, a denied command hidden
// behind sudo.

func allowlist(commands string) Policy {
	return Policy{Mode: PolicyAllowlist, Allowed: commands, Sudo: SudoDeny}
}

func denylist(commands, sudo string) Policy {
	return Policy{Mode: PolicyDenylist, Denied: commands, Sudo: sudo}
}

func TestAllowlistPermitsWhatItNames(t *testing.T) {
	policy := allowlist("systemctl restart application\nuptime\n/usr/bin/df")

	permitted := []string{
		"uptime",
		"uptime -p",
		"systemctl restart application",
		"/usr/bin/df -h", // called the way it was named
		"uptime  -p",     // spacing is the shell's business, not a difference
	}
	for _, command := range permitted {
		if reason := policy.Check(command); reason != "" {
			t.Fatalf("%q should be permitted, refused with: %s", command, reason)
		}
	}

	// A name and a path are not the same permission: an entry naming a path
	// permits that path, not whatever a bare name happens to resolve to on the
	// server, and an entry naming a program does not permit a program of the
	// same name living somewhere else.
	if reason := policy.Check("df -h"); reason == "" {
		t.Fatal("naming /usr/bin/df permitted a bare df, which resolves wherever the server's path points")
	}
	if reason := policy.Check("/tmp/uptime"); reason == "" {
		t.Fatal("naming uptime permitted a program of the same name somewhere else")
	}
}

func TestAllowlistRefusesEverythingElse(t *testing.T) {
	policy := allowlist("systemctl restart application\nuptime")

	cases := []struct {
		name    string
		command string
	}{
		{"another program", "rm -rf /"},
		{"a different subcommand of a permitted program", "systemctl stop database"},
		{"a second command after a semicolon", "uptime; rm -rf /"},
		{"a second command after and", "uptime && rm -rf /"},
		{"a command hidden in a pipeline", "uptime | rm -rf /"},
		{"a redirection", "uptime > /etc/passwd"},
		{"a subshell", "(rm -rf /)"},
		{"a substitution deciding what runs on the server", "$(echo uptime)"},
		{"an argument built on the server", "uptime `whoami`"},
		{"a variable standing in for the command", "$SHELL"},
		{"backgrounding", "uptime &"},
		{"an environment prefix", "PATH=/tmp uptime"},
		{"a permitted name reached through a path that is not the named one", "../../bin/rm -rf /"},
		{"nothing at all", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if reason := policy.Check(tc.command); reason == "" {
				t.Fatalf("%q was permitted", tc.command)
			}
		})
	}
}

// A denylist has to look at every command the line would run, not just the one
// at the front, or naming a command achieves nothing.
func TestDenylistLooksAtEveryCommand(t *testing.T) {
	policy := denylist("rm\nmkfs\nshutdown", SudoDeny)

	refused := []string{
		"rm -rf /",
		"uptime; rm -rf /",
		"uptime && rm -rf /",
		"uptime || rm -rf /",
		"cat /etc/passwd | rm -rf /",
		"(cd /tmp && rm -rf .)",
		"if true; then rm -rf /; fi",
		"for f in a b; do rm $f; done",
		"/bin/rm -rf /",
		// Reached as an argument rather than as the program: a check that only
		// read the first word of each command would let all of these through.
		"xargs rm -rf",
		"find . -exec rm {} ;",
		"nice -n 10 rm -rf /",
	}
	for _, command := range refused {
		if reason := policy.Check(command); reason == "" {
			t.Fatalf("%q was permitted by a denylist naming rm", command)
		}
	}

	permitted := []string{"uptime", "systemctl status application", "cat /var/log/application.log | tail -n 50"}
	for _, command := range permitted {
		if reason := policy.Check(command); reason != "" {
			t.Fatalf("%q should be permitted, refused with: %s", command, reason)
		}
	}
}

// Running as another user is refused by default, and a denied command cannot be
// smuggled in behind a wrapper that is not itself denied.
func TestDenylistLooksThroughSudo(t *testing.T) {
	denied := denylist("rm", SudoDeny)
	for _, command := range []string{"sudo rm -rf /", "sudo uptime", "doas rm -rf /", "su -c uptime"} {
		if reason := denied.Check(command); reason == "" {
			t.Fatalf("%q was permitted while running as another user is refused", command)
		}
	}

	allowed := denylist("rm", SudoAllow)
	if reason := allowed.Check("sudo systemctl restart application"); reason != "" {
		t.Fatalf("sudo is permitted here, but the command was refused: %s", reason)
	}
	// Permitting sudo does not permit what the denylist names.
	if reason := allowed.Check("sudo rm -rf /"); reason == "" {
		t.Fatal("a denied command ran behind sudo")
	}
	if reason := allowed.Check("sudo -u deploy rm -rf /"); reason == "" {
		t.Fatal("a denied command ran behind sudo with options")
	}
}

// A word that is put together on the server cannot be checked here, so it is
// refused rather than guessed at.
func TestDenylistRefusesWhatItCannotRead(t *testing.T) {
	policy := denylist("rm", SudoDeny)
	for _, command := range []string{"$(echo rm) -rf /", "`echo rm` -rf /", "$CMD -rf /"} {
		if reason := policy.Check(command); reason == "" {
			t.Fatalf("%q was permitted although what it runs is decided on the server", command)
		}
	}
}

// An argument is not a command, and refusing every one the shell decides made
// ordinary use of a shell impossible.
//
// `ls *.log` and `grep "$pattern" file` were refused with "put together on the
// server, so what would run cannot be checked", which is true of a command NAME
// and false of an argument: a glob becomes filenames, a variable becomes text,
// and neither can run anything.
func TestArgumentsMayBeLeftToTheShell(t *testing.T) {
	// `date` is on the list because the substitution below RUNS it, which is the
	// rule working rather than an exception to it.
	allow := allowlist("ls\ngrep\ngit\ndate")
	for _, command := range []string{
		`ls *.log`,
		`grep "$pattern" notes.txt`,
		`git log --since=$(date -d yesterday +%F)`,
		`ls "${HOME}/logs"`,
		`grep -c $((1 + 1)) file`,
	} {
		if reason := allow.Check(command); reason != "" {
			t.Errorf("%q was refused: %s", command, reason)
		}
	}
}

// The one thing an argument CAN do is run something, and that is checked rather
// than waved through: a substitution is a command of its own.
func TestACommandInsideAnArgumentIsStillChecked(t *testing.T) {
	deny := denylist("rm", SudoDeny)
	for _, command := range []string{
		`echo $(rm -rf /)`,
		"echo `rm -rf /`",
		`echo $(echo $(rm -rf /))`,
	} {
		if reason := deny.Check(command); reason == "" {
			t.Errorf("%q ran a denied command inside an argument", command)
		}
	}
	// And the allowlist wants every command permitted, not just the outer one.
	allow := allowlist("echo")
	if reason := allow.Check(`echo $(rm -rf /)`); reason == "" {
		t.Error("an allowlist permitted a command it does not name, inside a substitution")
	}
}

// Anything that runs what it is handed keeps the strict rule, because for those
// an argument IS a command and reading this line cannot see it.
func TestWhatRunsItsArgumentsMustSayWhatTheyAre(t *testing.T) {
	deny := denylist("rm", SudoDeny)
	for _, command := range []string{
		`xargs $CMD`,
		`sh -c "$CMD"`,
		`env $CMD`,
		`timeout 5 $CMD`,
		`find . -exec $CMD {} ;`,
	} {
		if reason := deny.Check(command); reason == "" {
			t.Errorf("%q handed the shell a command nobody could read", command)
		}
	}
	// The same in allow mode, where it would otherwise be the way past the list.
	allow := allowlist("xargs\nsh")
	if reason := allow.Check(`xargs $CMD`); reason == "" {
		t.Error("an allowlist let a command through by way of xargs")
	}
}

// Quoting is the shell's, and the check reads the word the shell would.
func TestQuotedCommandsAreReadAsWritten(t *testing.T) {
	policy := allowlist("systemctl restart application")
	if reason := policy.Check(`systemctl restart "application"`); reason != "" {
		t.Fatalf("a quoted argument was not read: %s", reason)
	}
	if reason := policy.Check(`"rm" -rf /`); reason == "" {
		t.Fatal("quoting the program name got it past the allowlist")
	}
}

// A refusal is written for the assistant to report, not to work around.
func TestRefusalTellsTheAssistantNotToRetry(t *testing.T) {
	reason := allowlist("uptime").Check("rm -rf /")
	if !strings.Contains(reason, "not permitted") {
		t.Fatalf("the refusal does not say the command is not permitted: %q", reason)
	}
	if !strings.Contains(reason, "rewording will not help") {
		t.Fatalf("the refusal does not head off a retry: %q", reason)
	}
}

func TestUnreadableCommandIsRefused(t *testing.T) {
	if reason := allowlist("uptime").Check("uptime '"); reason == "" {
		t.Fatal("a command that is not a command was permitted")
	}
}

// A list is written by a person, so it arrives with blank lines between groups
// and stray whitespace on the ends. None of that may become an entry: an empty
// entry in a matcher is the worst kind of bug, because it matches by accident.
func TestBlankLinesAndWhitespaceAreNotEntries(t *testing.T) {
	// Written the way somebody actually pastes a list: grouped, indented, with a
	// trailing newline and a line that is only spaces.
	spaced := denylist("rm\nshred\n\n   \nshutdown  \n\treboot\n", SudoDeny)
	tidy := denylist("rm\nshred\nshutdown\nreboot", SudoDeny)

	if got, want := len(spaced.DeniedEntries()), len(tidy.DeniedEntries()); got != want {
		t.Fatalf("the spaced-out list parsed to %d entries, want the same %d as the tidy one: %v",
			got, want, spaced.DeniedEntries())
	}
	for _, entry := range spaced.DeniedEntries() {
		if len(entry) == 0 || entry[0] == "" {
			t.Fatalf("a blank line became an entry: %q", spaced.DeniedEntries())
		}
	}

	// The whitespace did not stop it refusing what it names, on either side of a
	// blank line, and trailing spaces did not make an entry that matches nothing.
	for _, denied := range []string{"rm -rf /tmp/x", "shutdown now", "reboot"} {
		if reason := spaced.Check(denied); reason == "" {
			t.Fatalf("%q was permitted; whitespace broke the entry", denied)
		}
	}
	// And it still permits everything it does not name: a blank line must not
	// have turned into an entry that matches anything.
	for _, allowed := range []string{"uptime", "df -h", "systemctl status application"} {
		if reason := spaced.Check(allowed); reason != "" {
			t.Fatalf("%q was refused by a denylist that never named it: %s", allowed, reason)
		}
	}

	// The same for an allowlist, where an empty entry would be the other kind of
	// disaster: one that permits something nobody wrote down.
	permitting := allowlist("uptime\n\n  \ndf -h\n")
	for _, entry := range permitting.AllowedEntries() {
		if len(entry) == 0 || entry[0] == "" {
			t.Fatalf("a blank line became an allowed entry: %q", permitting.AllowedEntries())
		}
	}
	if reason := permitting.Check("rm -rf /"); reason == "" {
		t.Fatal("an allowlist with blank lines permitted a command it never named")
	}
	if reason := permitting.Check("uptime"); reason != "" {
		t.Fatalf("an allowlist with blank lines refused what it does name: %s", reason)
	}
}
