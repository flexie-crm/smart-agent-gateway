package app

import (
	"context"
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/link"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/skills"
)

// A computer that speaks whatever it is told to, or nothing at all.
type speaks struct{ runs map[string]int }

func (s speaks) Call(context.Context, int64, int64, string, string, json.RawMessage, string) (link.Result, error) {
	return link.Result{}, nil
}
func (s speaks) Runs(int64, int64, string) map[string]int { return s.runs }

// The two skill tools are there when skills are, and not when they are not.
//
// Appended rather than registered-and-rebound, which is the difference from the
// brain tools: a tool whose only possible answer is "you hold none" should not
// be there at all, so the model never learns of an ability it cannot use. This
// pins that, because the alternative is invisible: an always-present tool works
// perfectly well and quietly costs a schema in every prompt of every workspace
// that has never imported a skill.
func TestTheSkillToolsRideOnTheAssignment(t *testing.T) {
	names := func(loadout tool.Loadout) map[string]bool {
		got := map[string]bool{}
		for _, s := range loadout.Schemas {
			got[s.Name] = true
		}
		return got
	}

	nowhere := tools.Computer{WorkspaceID: 1, UserID: 2}

	none := tool.Loadout{Handlers: map[string]tool.Handler{}}
	tools.AppendSkills(&none, nil, nil, nowhere, nil)
	if got := names(none); got[skills.LoadName] || got[skills.SearchName] {
		t.Errorf("an agent with no skills carries the tools anyway: %+v", got)
	}
	if len(none.Handlers) != 0 {
		t.Errorf("it wired handlers for them: %+v", none.Handlers)
	}

	held := tool.Loadout{Handlers: map[string]tool.Handler{}}
	tools.AppendSkills(&held, nil, nil, nowhere, []int64{7})
	got := names(held)
	if !got[skills.LoadName] || !got[skills.SearchName] {
		t.Fatalf("an agent with a skill is offered %+v", got)
	}
	// Both callable: a schema with no handler is a tool the model calls and the
	// loop cannot run.
	for _, name := range []string{skills.LoadName, skills.SearchName} {
		if held.Handlers[name] == nil {
			t.Errorf("%s has no handler", name)
		}
	}
	// A nil Handlers map is the shape a caller can hand over, and appending has
	// to cope: the brain tools are bound into a map somebody else made, these
	// make their own.
	fresh := tool.Loadout{}
	tools.AppendSkills(&fresh, nil, nil, nowhere, []int64{7})
	if fresh.Handlers[skills.LoadName] == nil {
		t.Error("appending onto a loadout with no handler map wires nothing")
	}
}

// RUNNING a script needs a computer, so it is offered only where there is one
// that can. READING a skill does not: its instructions are readable anywhere.
//
// Four states, each a different answer, which is why this is a test rather than
// a line somebody trusts: no computer at all (a browser), an application a
// release behind that has never heard of the call, one that speaks another
// version of it, and one that speaks ours.
func TestRunningAScriptIsOfferedOnlyWhereAComputerCanRunOne(t *testing.T) {
	has := func(loadout tool.Loadout, name string) bool {
		for _, s := range loadout.Schemas {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	on := tools.Computer{WorkspaceID: 1, UserID: 2, DeviceID: "the-laptop"}

	browser := tool.Loadout{}
	tools.AppendSkills(&browser, nil, speaks{}, tools.Computer{WorkspaceID: 1, UserID: 2}, []int64{7})
	if !has(browser, skills.LoadName) {
		t.Error("a conversation with no computer cannot read a skill either")
	}
	if has(browser, skills.ExecName) {
		t.Error("a conversation with no computer is offered a way to run a script")
	}

	// An application that has never heard of the call. Its absence is the
	// version mechanism working rather than a failure.
	behind := tool.Loadout{}
	tools.AppendSkills(&behind, nil, speaks{runs: map[string]int{"terminal": 3}}, on, []int64{7})
	if has(behind, skills.ExecName) {
		t.Error("an application that does not speak skill_run is offered it anyway")
	}

	// One that speaks it at a version this gateway does not. Same answer, and
	// the whole point of a per-tool number.
	wrong := tool.Loadout{}
	tools.AppendSkills(&wrong, nil,
		speaks{runs: map[string]int{machine.SkillRunName: 99}}, on, []int64{7})
	if has(wrong, skills.ExecName) {
		t.Error("an application speaking another version of skill_run is offered it")
	}

	// And one that does.
	ready := tool.Loadout{}
	tools.AppendSkills(&ready, nil,
		speaks{runs: map[string]int{machine.SkillRunName: machine.VersionOf(machine.SkillRunName)}},
		on, []int64{7})
	if !has(ready, skills.ExecName) {
		t.Fatal("a computer that speaks skill_run is not offered it")
	}
	if ready.Handlers[skills.ExecName] == nil {
		t.Error("it is offered with no handler")
	}
}
