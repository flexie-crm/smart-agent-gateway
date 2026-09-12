package app

import (
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

func windowsBox() *MachineEnv {
	return &MachineEnv{
		OS: "windows", Arch: "x86_64", Name: "ERIOL-PC", Shell: "cmd.exe",
		PathSeparator: "\\", CaseSensitivePaths: false, LineEnding: "\r\n",
		Home: `C:\Users\eriol`,
		Has:  []string{"git", "node"}, Missing: []string{"bash", "make"},
	}
}

// The section is about a computer the assistant can act on, so it appears only
// when it holds a tool that runs there. A linked laptop whose machine tools an
// administrator has switched off is a computer nothing can touch, and
// describing it invites work that cannot be done.
func TestTheComputerOnlyWhenSomethingCanActOnIt(t *testing.T) {
	withTerminal := []tool.Schema{{Name: "terminal"}}
	noneOfThem := []tool.Schema{{Name: "http_request"}}

	if got := theComputer(nil, withTerminal); got != "" {
		t.Errorf("no machine described, yet a section was written: %q", got)
	}
	if got := theComputer(windowsBox(), noneOfThem); got != "" {
		t.Errorf("no tool runs on that computer, yet it was described: %q", got)
	}
	if got := theComputer(windowsBox(), withTerminal); got == "" {
		t.Fatal("a computer and a tool that runs on it, and nothing was said")
	}
}

// What it actually tells the model. Asserted as the facts a wrong guess would
// get wrong: the shell (cmd.exe, not a POSIX one), the separator, and both
// halves of the program list.
func TestTheComputerSaysWhatAGuessWouldGetWrong(t *testing.T) {
	got := theComputer(windowsBox(), []tool.Schema{{Name: "terminal"}})
	for _, want := range []string{
		"Windows", "ERIOL-PC", "x86_64", "cmd.exe", `C:\Users\eriol`,
		"not case sensitive", "Installed: git, node", "Not installed: bash, make",
		"not what you are allowed to run",
		// The line that exists because it probed a list it had already been
		// given, and the exception that keeps a version question honest.
		"answer from it directly rather than running a command",
		"does not include version numbers",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the computer section never mentions %q:\n%s", want, got)
		}
	}
}

// A machine that says little still produces a usable paragraph rather than a
// sentence with holes in it: an application older than this gateway may fill in
// only some of these.
func TestTheComputerWithAlmostNothingSaid(t *testing.T) {
	got := theComputer(&MachineEnv{OS: "linux"}, []tool.Schema{{Name: "read_file"}})
	if !strings.Contains(got, "Linux") {
		t.Fatalf("a bare machine lost its system: %q", got)
	}
	for _, unwanted := range []string{"called ", "Terminal commands run in", "Installed:", "Not installed:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("said %q about a machine that reported none of it:\n%s", unwanted, got)
		}
	}
}
