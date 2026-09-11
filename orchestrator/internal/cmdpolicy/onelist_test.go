package cmdpolicy_test

import (
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/sshtool"
	"flexie.io/sag/internal/tools/template"
)

// Both tools that run commands ship the SAME list.
//
// They shipped two. The server tool had one and the terminal was given another,
// which is two answers to "what is too dangerous to run" that drift within a
// release: somebody hardens one after an incident and the other keeps the hole.
// This is what stops that, and it is asserted against what each tool actually
// declares rather than against a constant, so seeding one from something else
// fails here.
func TestBothToolsShipTheSameDenylist(t *testing.T) {
	ssh := denied(t, sshFields(t))
	term := denied(t, terminalSections(t))

	if ssh == "" || term == "" {
		t.Fatal("a tool that runs commands ships no denylist at all")
	}
	if ssh != term {
		t.Fatal("the server tool and the terminal ship different denylists")
	}
	if ssh != cmdpolicy.DefaultDenied {
		t.Fatal("the shipped list is not the shared one")
	}
}

func sshFields(t *testing.T) []tool.Section {
	t.Helper()
	sections, err := sshtool.New(nil).Fields(sshtool.VariantSSH)
	if err != nil {
		t.Fatalf("the server tool has no form: %v", err)
	}
	return asSections(sections)
}

func terminalSections(t *testing.T) []tool.Section {
	t.Helper()
	for _, registered := range machine.Tools(nil) {
		if registered.Schema.Name == machine.TerminalName {
			return registered.Schema.Settings
		}
	}
	t.Fatal("the terminal is not among the machine tools")
	return nil
}

func asSections(in []template.Section) []tool.Section { return in }

func denied(t *testing.T, sections []tool.Section) string {
	t.Helper()
	for _, section := range sections {
		for _, field := range section.Fields {
			if field.Key == "policy.denied" {
				return field.Default
			}
		}
	}
	return ""
}
