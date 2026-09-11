package machine

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
)

// What the model is told about a tool has to be true of the tool.
//
// A guide is prompt text: it is read once, believed, and acted on for the rest
// of a conversation. A guide describing the tool as it USED to be is worse than
// none, because the assistant follows it confidently into a refusal.

// Every guide must be readable, since tool_guide hands it over as it is.
func TestEveryGuideIsReadable(t *testing.T) {
	for _, machineTool := range Tools(nil) {
		if len(machineTool.Schema.Guide) == 0 {
			continue
		}
		var read map[string]any
		if err := json.Unmarshal(machineTool.Schema.Guide, &read); err != nil {
			t.Errorf("%s has a guide that cannot be read: %v", machineTool.Schema.Name, err)
		}
	}
}

// The terminal's guide must describe the terminal it IS: one that remembers,
// that can come back still running, and that can be answered. Every one of
// these was untrue of the tool a version ago, which is exactly why they are
// asserted rather than assumed.
func TestTheTerminalGuideDescribesTheToolItIs(t *testing.T) {
	guide := string(terminalGuide)
	for _, must := range []struct{ says, why string }{
		{"export", "a plain assignment does not survive, and the model has to be told to export"},
		{"running: true", "a command can come back unfinished, which is the new shape"},
		{"input", "something that asks can be answered"},
		{"stop", "something that will not finish can be ended"},
		{"one command at a time", "starting a second in the same terminal is refused"},
		{"session", "a conversation may have several terminals, which is how work runs in parallel"},
		{"status", "with several going, one answer about all of them is the question to ask"},
	} {
		if !strings.Contains(strings.ToLower(guide), strings.ToLower(must.says)) {
			t.Errorf("the terminal's guide never mentions %q: %s", must.says, must.why)
		}
	}
}

// And the schema the model reads every turn must agree with it. The guide is
// asked for; the description always arrives, so the description carries the
// facts a call cannot be made without.
func TestTheTerminalDescriptionCarriesWhatACallNeeds(t *testing.T) {
	description := strings.ToLower(terminalSchema().Description)
	for _, must := range []string{"remembers", "export", "wait", "input", "stop"} {
		if !strings.Contains(description, must) {
			t.Errorf("the terminal's description never mentions %q, and it is read every turn", must)
		}
	}
}

// A guide must not promise an argument the tool does not take. This is the
// drift that a contract change creates: the shape moves, the prose does not.
func TestNoGuidePromisesAnArgumentTheToolDoesNotTake(t *testing.T) {
	for _, machineTool := range Tools(nil) {
		if len(machineTool.Schema.Guide) == 0 {
			continue
		}
		var guide struct {
			Parameters map[string]string `json:"parameters"`
		}
		if err := json.Unmarshal(machineTool.Schema.Guide, &guide); err != nil {
			t.Fatalf("%s: %v", machineTool.Schema.Name, err)
		}
		takes := argumentsOf(t, machineTool.Schema)
		for named := range guide.Parameters {
			if !takes[named] {
				t.Errorf("%s's guide describes %q, which the tool does not take",
					machineTool.Schema.Name, named)
			}
		}
	}
}

// argumentsOf is what a tool's schema says it accepts.
func argumentsOf(t *testing.T, schema tool.Schema) map[string]bool {
	t.Helper()
	var input struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schema.InputSchema, &input); err != nil {
		t.Fatalf("%s has an unreadable input schema: %v", schema.Name, err)
	}
	takes := make(map[string]bool, len(input.Properties))
	for named := range input.Properties {
		takes[named] = true
	}
	return takes
}

// The other half of the seam.
//
// The link gate asks the REAL tools and checks their answers carry every field
// the panel is told to show. That catches a rename on the machine's side. It
// cannot catch one HERE: change a display declaration to a field the tool does
// not answer and the gate still passes, while the panel quietly shows nothing.
//
// So the declaration is written down twice, on purpose, in the two places a
// rename has to reach. Whichever one somebody forgets, a test fails.
func TestTheDisplayDeclarationsAreWhatTheGateChecks(t *testing.T) {
	expected := map[string]struct{ where, sent, answered []string }{
		TerminalName: {
			where:    []string{"directory"},
			sent:     []string{"session", "command", "input", "wait", "stop", "status"},
			answered: []string{"output", "running_command", "running_for_seconds", "stopped", "terminals", "note"},
		},
		ReadFileName: {
			where:    []string{"path"},
			sent:     []string{"offset", "limit"},
			answered: []string{"content", "more", "hash"},
		},
		WriteFileName: {
			where:    []string{"path"},
			sent:     []string{"content"},
			answered: []string{"created", "created_folder", "bytes", "hash"},
		},
		EditFileName: {
			where:    []string{"path"},
			sent:     []string{"find", "replace", "all", "start_line", "end_line"},
			answered: []string{"replacements", "hash", "lines"},
		},
		FindFilesName: {
			sent:     []string{"pattern", "folder"},
			answered: []string{"files", "count", "more"},
		},
		SearchFileName: {
			sent:     []string{"pattern", "glob", "folder", "ignore_case"},
			answered: []string{"matches", "count", "more"},
		},
	}

	for _, machineTool := range Tools(nil) {
		want, named := expected[machineTool.Schema.Name]
		if !named {
			continue // machine_info shows everything, and declares nothing
		}
		shown := machineTool.Schema.Shown
		same(t, machineTool.Schema.Name, "where", want.where, shown.Where)
		same(t, machineTool.Schema.Name, "sent", want.sent, namesOf(shown.Sent))
		same(t, machineTool.Schema.Name, "answered", want.answered, namesOf(shown.Answered))
	}
}

func namesOf(shown []tool.Shown) []string {
	out := make([]string, 0, len(shown))
	for _, one := range shown {
		out = append(out, one.Field)
	}
	return out
}

func same(t *testing.T, who, half string, want, got []string) {
	t.Helper()
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("%s's %s fields are %v, and the gate checks %v. A rename has to reach both.",
			who, half, got, want)
	}
}
