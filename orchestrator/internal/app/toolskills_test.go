package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/skills"
	"flexie.io/sag/internal/tools/template"
)

// A skill attached to a TOOL, which is a different thing from a skill assigned
// to an agent.
//
// An agent's skill is in the prompt every turn, because it could bear on
// anything it is asked. A tool's skill is the documentation FOR that tool: the
// paths an API has, what its codes mean. It is mentioned only where the tool
// is, which is the tool's own guide, and it is reachable because holding the
// tool is what grants it. Nobody assigns it to the agent.
//
// The test says exactly that: the agent is assigned NOTHING, and the skill is
// still named and still openable.

func (e *env) anAPITool(t *testing.T, alias string, skills []int64) *model.Tool {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(api.Close)

	created, err := e.app.CreateCustomTool(context.Background(), e.ws.ID, "api", template.Input{
		Alias:   alias,
		Variant: "none",
		Settings: map[string]any{
			"base_url": api.URL, "timeout_seconds": float64(10),
			"policy.mode": "denylist", "policy.verbs": "DELETE",
		},
	}, skills, model.Nobody())
	if err != nil {
		t.Fatalf("create the api tool: %v", err)
	}
	return created
}

func guideOf(t *testing.T, loadout tool.Loadout, name string) string {
	t.Helper()
	schema, ok := loadout.Schema(name)
	if !ok {
		t.Fatalf("%s did not reach the loadout", name)
	}
	var guide string
	if len(schema.Guide) > 0 {
		if err := json.Unmarshal(schema.Guide, &guide); err != nil {
			t.Fatalf("the guide is not a string: %v", err)
		}
	}
	return guide
}

// The whole of it: attach a skill to a tool, and the tool's guide names it and
// says how to open it.
func TestAToolNamesTheProceduresWrittenForIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops@acme.test")

	written := e.aSkill("invoice-api", "Working the invoice API", "Every path the invoice API has.")
	other := e.aSkill("unrelated", "Something else", "Nothing to do with this tool.")
	// The agent holds NO skills of its own. Whatever it can reach here, it can
	// reach because of the tool.
	e.assign(nil)

	created := e.anAPITool(t, "invoices", []int64{written.ID})

	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{created.Name},
		nil, nil, tool.OwnerOfAgent(), model.Nobody())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}

	guide := guideOf(t, loadout, created.Name)
	if !strings.Contains(guide, "invoice-api") {
		t.Fatalf("the tool's guide does not name the procedure written for it:\n%s", guide)
	}
	if !strings.Contains(guide, "load_skill") {
		t.Fatalf("the guide names a procedure and does not say how to open one:\n%s", guide)
	}
	// Somebody else's skill is not advertised by this tool.
	if strings.Contains(guide, "unrelated") {
		t.Fatalf("a skill this tool was not pointed at reached its guide:\n%s", guide)
	}

	// And it is REACHABLE, which is the half a guide alone cannot deliver: a
	// named procedure the turn could not open would be a signpost to a locked
	// door.
	if !contains(loadout.Skills, written.ID) {
		t.Fatalf("holding the tool did not make its procedure reachable: %v", loadout.Skills)
	}
	if contains(loadout.Skills, other.ID) {
		t.Fatalf("a skill nothing pointed at became reachable: %v", loadout.Skills)
	}
}

// A tool pointed at nothing says nothing. The guide is the template's and
// carries no empty heading about procedures that do not exist.
func TestAToolWithNoProceduresSaysNothingAboutThem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops2@acme.test")
	e.assign(nil)

	created := e.anAPITool(t, "plain", nil)
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{created.Name},
		nil, nil, tool.OwnerOfAgent(), model.Nobody())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	guide := guideOf(t, loadout, created.Name)
	if strings.Contains(guide, "WRITTEN PROCEDURES FOR THIS ABILITY") {
		t.Fatalf("a tool with no procedures advertised the heading anyway:\n%s", guide)
	}
	if len(loadout.Skills) != 0 {
		t.Fatalf("a tool pointed at nothing made something reachable: %v", loadout.Skills)
	}
}

// A skill that is switched off is not named and not reachable, which is the
// same rule an agent's assigned skills follow. The two halves come from one
// resolution, so they cannot disagree.
func TestASwitchedOffProcedureIsNeitherNamedNorReachable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops3@acme.test")
	e.assign(nil)

	written := e.aSkill("retired-api", "A retired procedure", "Was useful once.")
	created := e.anAPITool(t, "retired", []int64{written.ID})

	if _, err := e.app.Store.Skills().Update(ctx, e.ws.ID, written.ID, store.SkillUpdate{
		Title: written.Title, Description: written.Description, Status: model.StatusDisabled,
	}, model.Nobody()); err != nil {
		t.Fatalf("switch the skill off: %v", err)
	}

	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{created.Name},
		nil, nil, tool.OwnerOfAgent(), model.Nobody())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	if guide := guideOf(t, loadout, created.Name); strings.Contains(guide, "retired-api") {
		t.Fatalf("a switched-off procedure was still named:\n%s", guide)
	}
	if len(loadout.Skills) != 0 {
		t.Fatalf("a switched-off procedure was still reachable: %v", loadout.Skills)
	}
}

func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The SEAM, driven rather than reasoned about.
//
// Everything above asserts that the tool's skills reach loadout.Skills. That is
// one layer, and the layer that matters is the next one: AppendSkills scopes
// load_skill to a list of ids, and if the two were assembled in the wrong order
// the list would be empty at the moment it is read, every assertion above would
// still pass, and the agent would be handed a guide naming a procedure it could
// not open.
//
// So this resolves a real turn and CALLS load_skill, with the agent assigned
// nothing at all. What comes back has to be the skill's own content.
func TestHoldingTheToolIsEnoughToOpenItsProcedure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("seam@acme.test")

	written := e.aSkill("payments-api", "Working the payments API", "Every path it has.")
	created := e.anAPITool(t, "payments", []int64{written.ID})

	// The Gateway holds the TOOL and no skills whatsoever.
	gateway, err := e.app.Store.Agents().GetByKey(ctx, e.ws.ID, model.DefaultAgentKey)
	if err != nil {
		gateway = &model.Agent{
			WorkspaceID: e.ws.ID, Key: model.DefaultAgentKey, Name: "Gateway",
			Status: model.StatusActive,
		}
		if err := e.app.Store.Agents().Create(ctx, gateway, model.Nobody()); err != nil {
			t.Fatalf("create the gateway: %v", err)
		}
	}
	gateway.Tools = []string{created.Name}
	gateway.Skills = []int64{}
	if err := e.app.Store.Agents().Update(ctx, gateway, model.Nobody()); err != nil {
		t.Fatalf("set the gateway up: %v", err)
	}

	_, loadout := e.resolved(user)
	if _, ok := loadout.Schema(created.Name); !ok {
		t.Fatalf("the tool is not in the resolved turn, so this proves nothing")
	}
	if _, ok := loadout.Handlers[skills.LoadName]; !ok {
		t.Fatalf("%s is not in the turn at all, though a tool named a procedure", skills.LoadName)
	}

	body, res := e.callTool(loadout, user, skills.LoadName, map[string]any{"skill": "payments-api"})
	if res.Failed() {
		t.Fatalf("a procedure named by a tool this turn HOLDS could not be opened: %s", body)
	}
	if !strings.Contains(body, "Working the payments API") {
		t.Fatalf("what came back is not the skill: %s", body)
	}
}

// One name on the form: what the assistant calls the tool is derived from what
// a person typed, so nobody keeps two names in step.
func TestTheAssistantsNameComesFromTheOneAPersonTyped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(api.Close)

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "api", template.Input{
		// No alias at all, which is what the form now sends.
		Variant:     "none",
		DisplayName: "Production orders (EU)",
		Settings: map[string]any{
			"base_url": api.URL, "timeout_seconds": float64(10),
			"policy.mode": "denylist", "policy.verbs": "DELETE",
		},
	}, nil, model.Nobody())
	if err != nil {
		t.Fatalf("create with no alias: %v", err)
	}
	if created.Name != "api_production_orders_eu" {
		t.Fatalf("the assistant's name is %q, want api_production_orders_eu", created.Name)
	}
	if created.FriendlyName != "Production orders (EU)" {
		t.Fatalf("the person's name was not kept: %q", created.FriendlyName)
	}

	// A name with nothing usable in it is refused, and says what to do rather
	// than reporting a pattern nobody can read.
	_, err = e.app.CreateCustomTool(ctx, e.ws.ID, "api", template.Input{
		Variant: "none", DisplayName: "!!!",
		Settings: map[string]any{
			"base_url": api.URL, "timeout_seconds": float64(10),
			"policy.mode": "denylist", "policy.verbs": "DELETE",
		},
	}, nil, model.Nobody())
	if err == nil {
		t.Fatal("a name with no letters in it was accepted")
	}
	if !strings.Contains(err.Error(), "letters") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
}
