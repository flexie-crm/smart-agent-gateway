package app

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The defect, pinned.
//
// An agent pinned inline was offered in delegate_fleet's list of legal agents.
// The prompt's line for inline forbade the background and said nothing about
// fleets, so the model asked for the one thing nobody had told it not to, and
// was refused by a rule stated in neither place it reads. The refusal then
// stayed in the transcript: the pin was changed hours later and the model went
// on believing it, because a tool result is permanent and configuration is not.
//
// So the rule is: an agent that would be turned away is never offered.

func fleetAgentsOffered(t *testing.T, subs []agentInfo) []string {
	t.Helper()
	var schema struct {
		Properties struct {
			Tasks struct {
				Items struct {
					Properties struct {
						Agent struct {
							Enum []string `json:"enum"`
						} `json:"agent"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(fleetToolSchema(subs, 0).InputSchema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	return schema.Properties.Tasks.Items.Properties.Agent.Enum
}

func TestAnInlineAgentIsNeverOfferedAsAFleetMember(t *testing.T) {
	offered := fleetAgentsOffered(t, []agentInfo{
		{Key: "waits", DelegationMode: model.DelegationModeInline},
		{Key: "batched", DelegationMode: model.DelegationModeFleet},
		{Key: "detached", DelegationMode: model.DelegationModeBackground},
		{Key: "undecided", DelegationMode: model.DelegationModeAuto},
	})

	for _, key := range offered {
		if key == "waits" {
			t.Fatal("an inline agent was offered as a fleet member: the model can ask for it, " +
				"be refused, and carry that refusal for the rest of the conversation")
		}
	}
	// Everything that CAN be in a fleet still is: this must not become a filter
	// that quietly empties the tool.
	for _, want := range []string{"batched", "detached", "undecided"} {
		if !contains(offered, want) {
			t.Fatalf("%q can run in a fleet but was not offered; the filter is too tight", want)
		}
	}
}

// The prompt and the schema have to agree. The schema stopping the request is
// no use if the prose still leaves the model believing it may make it.
func TestTheInlineInstructionSaysFleetsAreOutToo(t *testing.T) {
	line := pinnedModeLine(model.DelegationModeInline)
	if !strings.Contains(line, "delegate_fleet") {
		t.Fatalf("the inline instruction never mentions fleets, which is how this was missed:\n%s", line)
	}
	// And it must not overstate the restriction the other way. Terminal IS
	// honoured for an inline agent, so telling the model it has no mode choice
	// would take away something that works.
	if !strings.Contains(line, "terminal") {
		t.Fatalf("the inline instruction does not say terminal is still available:\n%s", line)
	}
}

// A mode the runtime ignores must not be presented as a choice. Background and
// fleet agents have their mode overruled outright, so their line has to say so.
func TestAPinnedAgentIsToldItsModeIsNotItsToChoose(t *testing.T) {
	for _, mode := range []string{model.DelegationModeBackground, model.DelegationModeFleet} {
		line := pinnedModeLine(mode)
		if !strings.Contains(line, "You do not choose its mode") {
			t.Fatalf("%s does not tell the model its mode argument is ignored:\n%s", mode, line)
		}
	}
}

// The mode argument exists only when somebody can actually use it. Every agent
// pinned means nothing to decide, and asking anyway is 230 tokens per call spent
// on an answer that is thrown away.
func TestTheModeArgumentIsAbsentWhenNoAgentLeavesTheChoice(t *testing.T) {
	pinned := []agentInfo{
		{Key: "waits", DelegationMode: model.DelegationModeInline},
		{Key: "batched", DelegationMode: model.DelegationModeFleet},
	}
	if strings.Contains(string(delegateToolSchema(pinned).InputSchema), "\"mode\"") {
		t.Fatal("the mode argument was offered when every agent pins its own")
	}
	withAuto := append(pinned, agentInfo{Key: "undecided", DelegationMode: model.DelegationModeAuto})
	if !strings.Contains(string(delegateToolSchema(withAuto).InputSchema), "\"mode\"") {
		t.Fatal("the mode argument vanished even though an agent leaves the choice to the Gateway")
	}
}

// The fleet instruction has to say which tool starts one and that a call is a
// batch. Without that the model reads "runs in a batch" and still issues one
// call per task, which is fifteen batches of one.
func TestTheFleetInstructionNamesTheToolAndTheBatch(t *testing.T) {
	line := pinnedModeLine(model.DelegationModeFleet)
	for _, want := range []string{"delegate_fleet", "ONE"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the fleet instruction does not say %q:\n%s", want, line)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
