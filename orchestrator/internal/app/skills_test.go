package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/skills"
)

// What an agent can do with a skill, resolved against a real database.
//
// These are here rather than beside the handlers because the whole chain is what
// is under test: an assignment written by the config store, carried onto the
// profile, turned into a prompt section and a pair of scoped tools. Each half
// has unit tests; nothing but this says they meet.

// aSkill imports one package, the way the console does, and returns its row.
func (e *env) aSkill(handle, title, description string) *model.Skill {
	e.t.Helper()
	pkg := &model.SkillPackage{
		Name: handle, Title: title, Description: description,
		SHA256: "sha-" + handle,
		Files: []model.PackageFile{
			{
				Path: "SKILL.md", FileType: model.SkillFileSkill,
				MIMEType: "text/markdown; charset=utf-8",
				Text: "---\nname: " + handle + "\ndescription: " + description +
					"\n---\n\n# " + title + "\n\nRead the document first.\n",
				Size: 80, SHA256: "sha-manifest-" + handle,
				Sections: []model.PackageSection{
					{Heading: title, Path: title, Body: "# " + title + "\n", LineStart: 5, LineEnd: 7, SHA256: "s1-" + handle},
				},
			},
			{
				Path: "references/deeper.md", FileType: model.SkillFileReference,
				MIMEType: "text/markdown; charset=utf-8",
				Text:     "# Deeper\n\nThe detail.\n",
				Size:     24, SHA256: "sha-ref-" + handle,
				Sections: []model.PackageSection{
					{Heading: "Deeper", Path: "Deeper", Body: "# Deeper\n", LineStart: 1, LineEnd: 3, SHA256: "s2-" + handle},
				},
			},
		},
	}
	stored, _, err := e.app.Store.Skills().Import(context.Background(), e.ws.ID, pkg, model.Nobody())
	if err != nil {
		e.t.Fatalf("import %s: %v", handle, err)
	}
	return stored
}

// assign puts skills on the Gateway, making it on first use.
//
// A scratch workspace has no default agent at all: the resolver falls back to
// the code defaults, which is a real state and not this test's subject. Reading
// the row back before writing is the habit that matters, so a later call
// changes the assignment and nothing else about it.
func (e *env) assign(ids []int64) {
	e.t.Helper()
	ctx := context.Background()
	gateway, err := e.app.Store.Agents().GetByKey(ctx, e.ws.ID, model.DefaultAgentKey)
	if errors.Is(err, store.ErrNotFound) {
		gateway = &model.Agent{
			WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Gateway",
			Status: model.StatusActive, Skills: ids,
		}
		if err := e.app.Store.Agents().Create(ctx, gateway, model.Nobody()); err != nil {
			e.t.Fatalf("create the gateway: %v", err)
		}
		return
	}
	if err != nil {
		e.t.Fatalf("read the gateway: %v", err)
	}
	gateway.Skills = ids
	if err := e.app.Store.Agents().Update(ctx, gateway, model.Nobody()); err != nil {
		e.t.Fatalf("assign skills: %v", err)
	}
}

// callTool calls one of the Gateway's tools the way the loop does, WITH the
// workspace on the call.
//
// The shared `look` helper builds a bare tool.Call, which is right for
// agent_guide (it answers from a closure) and silently wrong for anything that
// reads the store: a workspace of 0 owns no rows, so every answer came back
// empty and read exactly like a scoping bug.
func (e *env) callTool(loadout tool.Loadout, u *model.User, name string, args map[string]any) (string, tool.Result) {
	e.t.Helper()
	handler, ok := loadout.Handlers[name]
	if !ok {
		e.t.Fatalf("%s is not in the Gateway's loadout", name)
	}
	raw, _ := json.Marshal(args)
	res, err := handler(context.Background(), tool.Call{
		WorkspaceID: e.ws.ID, UserID: u.ID, Args: raw,
	})
	if err != nil {
		e.t.Fatalf("system error from %s: %v", name, err)
	}
	return string(res.Content), res
}

// resolved is what the Gateway runs as: its prompt and its tools.
func (e *env) resolved(u *model.User) (string, tool.Loadout) {
	e.t.Helper()
	ctx := context.Background()
	req := app.ProfileRequest{WorkspaceID: e.ws.ID, UserID: u.ID, Channel: model.ChannelChat}
	profile, err := e.app.ResolveProfile(ctx, req)
	if err != nil {
		e.t.Fatalf("resolve profile: %v", err)
	}
	loadout, err := e.app.ResolveTools(ctx, req, profile)
	if err != nil {
		e.t.Fatalf("resolve tools: %v", err)
	}
	return profile.SystemPrompt, loadout
}

func TestAnAssignedSkillReachesThePromptAndTheTools(t *testing.T) {
	e := newEnv(t)
	u := e.user("person@acme.test")

	held := e.aSkill("pdf-processing", "PDF Toolkit",
		"Extract totals from supplier invoices before they are filed.")
	// A second skill in the workspace that is NOT assigned: the control for
	// every scoping assertion here.
	e.aSkill("payroll-checks", "Payroll", "Check payroll before it is filed. Invoices too.")

	// Nothing assigned yet, so nothing about skills exists at all.
	prompt, loadout := e.resolved(u)
	if strings.Contains(prompt, "Skills you hold") {
		t.Errorf("the prompt names skills before any was assigned:\n%s", prompt)
	}
	if loadout.Handlers[skills.LoadName] != nil {
		t.Error("load_skill is wired before any skill was assigned")
	}

	e.assign([]int64{held.ID})
	prompt, loadout = e.resolved(u)

	// The map, in the prompt: the handle it addresses the skill by, the name a
	// person gave it, and no description.
	if !strings.Contains(prompt, "- pdf-processing (PDF Toolkit)") {
		t.Errorf("the roster is not in the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "supplier invoices") {
		t.Error("the description reached the prompt")
	}
	// And not the one it was not given.
	if strings.Contains(prompt, "payroll-checks") {
		t.Errorf("an unassigned skill is in the prompt:\n%s", prompt)
	}

	// Both tools, and callable.
	for _, name := range []string{skills.LoadName, skills.SearchName} {
		if loadout.Handlers[name] == nil {
			t.Fatalf("%s is not wired", name)
		}
	}

	// search_skill finds it by a word that is ONLY in the description, which is
	// the reason the tool exists.
	body, res := e.callTool(loadout, u, skills.SearchName, map[string]any{"query": "supplier"})
	if res.Failed() {
		t.Fatalf("search failed: %s", body)
	}
	if !strings.Contains(body, "pdf-processing") {
		t.Errorf("search did not find it by its description: %s", body)
	}
	// And cannot reach the one it was not given, whose description says
	// "Invoices" too.
	body, _ = e.callTool(loadout, u, skills.SearchName, map[string]any{"query": "filed"})
	if strings.Contains(body, "payroll-checks") {
		t.Errorf("search reached an unassigned skill: %s", body)
	}

	// load_skill hands over the instructions, without the frontmatter, and
	// indexes the rest of the package.
	body, res = e.callTool(loadout, u, skills.LoadName, map[string]any{"skill": "pdf-processing"})
	if res.Failed() {
		t.Fatalf("load failed: %s", body)
	}
	for _, want := range []string{
		"Read the document first", // the instructions
		"supplier invoices",       // the description, which load DOES carry
		"references/deeper.md",    // the index
		"Deeper",                  // that document's headings
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer is missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "name: pdf-processing") {
		t.Errorf("the frontmatter came with the instructions: %s", body)
	}

	// One part of it, by the path the index gave.
	body, res = e.callTool(loadout, u, skills.LoadName,
		map[string]any{"skill": "pdf-processing", "part": "references/deeper.md"})
	if res.Failed() {
		t.Fatalf("reading a part failed: %s", body)
	}
	if !strings.Contains(body, "The detail") {
		t.Errorf("the part did not arrive: %s", body)
	}

	// And the one it holds no assignment for is refused, by the real store.
	body, res = e.callTool(loadout, u, skills.LoadName, map[string]any{"skill": "payroll-checks"})
	if !res.Failed() {
		t.Errorf("it opened an unassigned skill: %s", body)
	}
	if strings.Contains(body, "Read the document first") {
		t.Errorf("the refusal handed over the instructions anyway: %s", body)
	}
}

// Revoking is not a half state: the tools go with the assignment, so an agent
// cannot be left holding an ability whose only answer is "you hold none".
func TestRevokingASkillTakesTheToolsWithIt(t *testing.T) {
	e := newEnv(t)
	u := e.user("person@acme.test")
	held := e.aSkill("pdf-processing", "PDF Toolkit", "Extract totals.")

	e.assign([]int64{held.ID})
	if _, loadout := e.resolved(u); loadout.Handlers[skills.LoadName] == nil {
		t.Fatal("load_skill is not wired for an agent that holds a skill")
	}

	// An EMPTY list, not a nil one: nil means no opinion and would leave the
	// assignment standing, which is the store's rule and exactly the mistake
	// this test caught when it was written with a variadic.
	e.assign([]int64{})
	prompt, loadout := e.resolved(u)
	if loadout.Handlers[skills.LoadName] != nil || loadout.Handlers[skills.SearchName] != nil {
		t.Error("the tools outlived the assignment")
	}
	if strings.Contains(prompt, "Skills you hold") || strings.Contains(prompt, skills.LoadName) {
		t.Errorf("the prompt still talks about skills:\n%s", prompt)
	}
}

// A skill an administrator switched off is not in the roster. The prompt is
// what the agent MAY do, and a skill it may not use is not a decision to put in
// front of it every turn.
func TestASwitchedOffSkillIsNotInTheRoster(t *testing.T) {
	e := newEnv(t)
	u := e.user("person@acme.test")
	held := e.aSkill("pdf-processing", "PDF Toolkit", "Extract totals.")
	e.assign([]int64{held.ID})

	if prompt, _ := e.resolved(u); !strings.Contains(prompt, "pdf-processing") {
		t.Fatalf("it is not in the roster to begin with:\n%s", prompt)
	}

	if _, err := e.app.Store.Skills().Update(context.Background(), e.ws.ID, held.ID,
		store.SkillUpdate{Title: held.Title, Description: held.Description, Status: model.SkillDisabled},
		model.Nobody()); err != nil {
		t.Fatalf("disable the skill: %v", err)
	}

	prompt, loadout := e.resolved(u)
	if strings.Contains(prompt, "pdf-processing") {
		t.Errorf("a disabled skill is still in the roster:\n%s", prompt)
	}
	// The tools stay, because the assignment stands: opening it says it is
	// switched off, which is the honest answer and not the same as the skill
	// having gone.
	if loadout.Handlers[skills.LoadName] == nil {
		t.Error("the tools went with the status rather than the assignment")
	}
	body, res := e.callTool(loadout, u, skills.LoadName, map[string]any{"skill": "pdf-processing"})
	if !res.Failed() || !strings.Contains(body, "switched off") {
		t.Errorf("opening a disabled skill says: %s", body)
	}
}
