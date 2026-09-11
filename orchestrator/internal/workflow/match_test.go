package workflow_test

import (
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/workflow"
)

// The matcher decides which assistant a person gets. Every rule below is a
// promise to an administrator, so each one is tested for what it does AND for
// what it must refuse to do.

func assign(matchType, value string, priority int) model.WorkflowAssignment {
	return model.WorkflowAssignment{MatchType: matchType, MatchValue: value, Priority: priority}
}

func candidate(id int64, assignments ...model.WorkflowAssignment) *model.WorkflowCandidate {
	return &model.WorkflowCandidate{
		WorkflowID:  id,
		VersionID:   id * 10,
		Assignments: assignments,
	}
}

// alice is in group 5, holds role 2, and is talking through the chat UI.
var alice = workflow.Subject{
	UserID:   7,
	GroupIDs: []int64{5},
	RoleIDs:  []int64{2},
	Channel:  model.ChannelChat,
}

func TestMatching(t *testing.T) {
	cases := []struct {
		name        string
		assignments []model.WorkflowAssignment
		subject     workflow.Subject
		want        bool
	}{
		{
			name:        "a workflow with no conditions applies to nobody",
			assignments: nil,
			subject:     alice,
			want:        false,
		},
		{
			name:        "her user id",
			assignments: []model.WorkflowAssignment{assign(model.MatchUser, "7", 0)},
			subject:     alice,
			want:        true,
		},
		{
			name:        "somebody else's user id",
			assignments: []model.WorkflowAssignment{assign(model.MatchUser, "8", 0)},
			subject:     alice,
			want:        false,
		},
		{
			name:        "her group",
			assignments: []model.WorkflowAssignment{assign(model.MatchGroup, "5", 0)},
			subject:     alice,
			want:        true,
		},
		{
			name:        "her role",
			assignments: []model.WorkflowAssignment{assign(model.MatchRole, "2", 0)},
			subject:     alice,
			want:        true,
		},
		{
			name:        "her channel",
			assignments: []model.WorkflowAssignment{assign(model.MatchChannel, model.ChannelChat, 0)},
			subject:     alice,
			want:        true,
		},
		{
			// Within a dimension, any condition is enough.
			name: "one of several groups",
			assignments: []model.WorkflowAssignment{
				assign(model.MatchGroup, "4", 0),
				assign(model.MatchGroup, "5", 0),
			},
			subject: alice,
			want:    true,
		},
		{
			// Across dimensions, all must hold. This is the identity x channel
			// of the configuration model, and it is the rule that lets one
			// workspace give the same people a different assistant per surface.
			name: "her group, on her channel",
			assignments: []model.WorkflowAssignment{
				assign(model.MatchGroup, "5", 0),
				assign(model.MatchChannel, model.ChannelChat, 0),
			},
			subject: alice,
			want:    true,
		},
		{
			name: "her group, but on a channel she is not using",
			assignments: []model.WorkflowAssignment{
				assign(model.MatchGroup, "5", 0),
				assign(model.MatchChannel, model.ChannelMCP, 0),
			},
			subject: alice,
			want:    false,
		},
		{
			name: "her channel, but a group she is not in",
			assignments: []model.WorkflowAssignment{
				assign(model.MatchGroup, "99", 0),
				assign(model.MatchChannel, model.ChannelChat, 0),
			},
			subject: alice,
			want:    false,
		},
		{
			// A row this build cannot interpret must never widen the match. It
			// fails closed, because the alternative is a workflow silently
			// applying to everyone.
			name:        "an unknown dimension matches nothing",
			assignments: []model.WorkflowAssignment{assign("department", "sales", 0)},
			subject:     alice,
			want:        false,
		},
		{
			name:        "a malformed identity matches nothing",
			assignments: []model.WorkflowAssignment{assign(model.MatchUser, "seven", 0)},
			subject:     alice,
			want:        false,
		},
		{
			name: "an unknown dimension cannot be smuggled in beside a real one",
			assignments: []model.WorkflowAssignment{
				assign(model.MatchGroup, "5", 0),
				assign("department", "sales", 0),
			},
			subject: alice,
			want:    false,
		},
		{
			name:        "a user with no groups still matches on channel",
			assignments: []model.WorkflowAssignment{assign(model.MatchChannel, model.ChannelMCP, 0)},
			subject:     workflow.Subject{UserID: 9, Channel: model.ChannelMCP},
			want:        true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := workflow.Select([]*model.WorkflowCandidate{candidate(1, tc.assignments...)}, tc.subject)
			if ok != tc.want {
				t.Fatalf("matched = %v, want %v", ok, tc.want)
			}
		})
	}
}

// Exactly one workflow wins. Overrides do not stack, so the winner has to be
// the one an administrator would predict.
func TestTheWinner(t *testing.T) {
	cases := []struct {
		name       string
		candidates []*model.WorkflowCandidate
		want       int64
	}{
		{
			name: "the higher priority wins",
			candidates: []*model.WorkflowCandidate{
				candidate(1, assign(model.MatchGroup, "5", 10)),
				candidate(2, assign(model.MatchGroup, "5", 20)),
			},
			want: 2,
		},
		{
			// Nobody should have to plan a priority scheme to express "this
			// rule is about one person, that one is about a whole channel".
			name: "at equal priority, the more specific wins",
			candidates: []*model.WorkflowCandidate{
				candidate(1, assign(model.MatchChannel, model.ChannelChat, 0)),
				candidate(2,
					assign(model.MatchUser, "7", 0),
					assign(model.MatchChannel, model.ChannelChat, 0)),
			},
			want: 2,
		},
		{
			// Priority is the explicit knob, so it outranks the implicit one.
			// An administrator who sets a number means it.
			name: "priority beats specificity",
			candidates: []*model.WorkflowCandidate{
				candidate(1, assign(model.MatchChannel, model.ChannelChat, 50)),
				candidate(2,
					assign(model.MatchUser, "7", 0),
					assign(model.MatchChannel, model.ChannelChat, 0)),
			},
			want: 1,
		},
		{
			name: "an otherwise perfect tie goes to the older workflow, and stays there",
			candidates: []*model.WorkflowCandidate{
				candidate(3, assign(model.MatchGroup, "5", 0)),
				candidate(1, assign(model.MatchRole, "2", 0)),
			},
			want: 1,
		},
		{
			name: "a workflow that does not match cannot win, however high its priority",
			candidates: []*model.WorkflowCandidate{
				candidate(1, assign(model.MatchGroup, "99", 100)),
				candidate(2, assign(model.MatchGroup, "5", 1)),
			},
			want: 2,
		},
		{
			// The priority of a workflow is the priority of the conditions that
			// actually matched. A high number on a condition she does not
			// satisfy must not lift the workflow she does.
			name: "priority comes from the conditions that matched",
			candidates: []*model.WorkflowCandidate{
				candidate(1,
					assign(model.MatchGroup, "5", 1),
					assign(model.MatchGroup, "99", 100)),
				candidate(2, assign(model.MatchRole, "2", 50)),
			},
			want: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			winner, ok := workflow.Select(tc.candidates, alice)
			if !ok {
				t.Fatal("expected a winner")
			}
			if winner.WorkflowID != tc.want {
				t.Fatalf("workflow %d won, want %d", winner.WorkflowID, tc.want)
			}
		})
	}
}

func TestNoCandidatesMeansTheDefaults(t *testing.T) {
	if _, ok := workflow.Select(nil, alice); ok {
		t.Fatal("nothing configured must resolve to the defaults, not to a workflow")
	}
}

// The order the store happened to return rows in must not change who gets
// which assistant.
func TestSelectionIsStable(t *testing.T) {
	forwards := []*model.WorkflowCandidate{
		candidate(1, assign(model.MatchGroup, "5", 0)),
		candidate(2, assign(model.MatchRole, "2", 0)),
		candidate(3, assign(model.MatchUser, "7", 0)),
	}
	backwards := []*model.WorkflowCandidate{forwards[2], forwards[1], forwards[0]}

	first, _ := workflow.Select(forwards, alice)
	second, _ := workflow.Select(backwards, alice)
	if first.WorkflowID != second.WorkflowID {
		t.Fatalf("the winner depends on row order: %d then %d", first.WorkflowID, second.WorkflowID)
	}
}
