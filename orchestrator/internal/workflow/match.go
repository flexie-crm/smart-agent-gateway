// Package workflow decides which workflow, if any, shapes a turn.
//
// This is the conditional-override matcher of the layered configuration model
// (KB/05): a workflow is assigned to an identity (a user, a group, a role) and
// to a source channel (the chat UI, an HTTP endpoint, an MCP client), and it
// overrides the default package for the requests that match it.
//
// The matching is pure logic over rows the caller has already loaded, which is
// why it lives apart from the store: the rules below are the product, and they
// deserve to be read and tested without a database in the way.
package workflow

import (
	"sort"
	"strconv"

	"flexie.io/sag/internal/model"
)

// Subject is who is asking, and through what. It is resolved live, per turn:
// a user removed from a group stops matching that group's workflow on their
// very next message, not when a token expires.
type Subject struct {
	UserID   int64
	GroupIDs []int64
	RoleIDs  []int64
	Channel  string
}

// Select returns the workflow that shapes this turn, or false when the
// defaults apply.
//
// # How a workflow matches
//
// Conditions are grouped by dimension. Within a dimension any condition is
// enough; across dimensions all must hold. That is the "identity x channel"
// of the configuration model, read literally:
//
//   - {group: 5, group: 7}            -> members of group 5 or group 7, anywhere
//   - {channel: mcp}                  -> everyone, but only through MCP
//   - {group: 5, channel: chat}       -> group 5, and only in the chat UI
//
// A workflow with no conditions matches nobody. Publishing one must never
// silently capture an entire workspace.
//
// # How a winner is chosen
//
// Exactly one workflow wins: overrides do not stack. Stacking would mean the
// answer to "why did this run that way" is a merge order nobody can hold in
// their head, and the whole point of this layer is that an administrator can
// predict it.
//
// The winner is the one with the highest priority. Ties go to the more
// specific workflow, the one that had to satisfy more dimensions, because an
// administrator who writes a rule for one user expects it to beat a rule for
// a whole channel without discovering that priorities are a number they were
// supposed to have planned. Remaining ties go to the oldest workflow, so the
// outcome is stable rather than merely arbitrary.
func Select(candidates []*model.WorkflowCandidate, subject Subject) (*model.WorkflowCandidate, bool) {
	type scored struct {
		candidate   *model.WorkflowCandidate
		priority    int
		specificity int
	}

	var matches []scored
	for _, c := range candidates {
		priority, specificity, ok := score(c.Assignments, subject)
		if !ok {
			continue
		}
		matches = append(matches, scored{candidate: c, priority: priority, specificity: specificity})
	}
	if len(matches) == 0 {
		return nil, false
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].priority != matches[j].priority {
			return matches[i].priority > matches[j].priority
		}
		if matches[i].specificity != matches[j].specificity {
			return matches[i].specificity > matches[j].specificity
		}
		return matches[i].candidate.WorkflowID < matches[j].candidate.WorkflowID
	})
	return matches[0].candidate, true
}

// score reports whether the subject satisfies every dimension the workflow
// conditions on, along with the priority it earns (the highest of the
// conditions that actually matched) and its specificity (how many dimensions
// it had to satisfy).
func score(assignments []model.WorkflowAssignment, subject Subject) (priority, specificity int, ok bool) {
	if len(assignments) == 0 {
		return 0, 0, false
	}

	// Dimension -> did the subject satisfy it, and at what priority.
	satisfied := make(map[string]bool, 4)
	best := make(map[string]int, 4)

	for _, a := range assignments {
		if _, seen := satisfied[a.MatchType]; !seen {
			satisfied[a.MatchType] = false
		}
		if !matches(a, subject) {
			continue
		}
		if !satisfied[a.MatchType] || a.Priority > best[a.MatchType] {
			satisfied[a.MatchType] = true
			best[a.MatchType] = a.Priority
		}
	}

	for dimension, hit := range satisfied {
		if !hit {
			return 0, 0, false
		}
		if best[dimension] > priority {
			priority = best[dimension]
		}
	}
	return priority, len(satisfied), true
}

func matches(a model.WorkflowAssignment, subject Subject) bool {
	switch a.MatchType {
	case model.MatchUser:
		return matchID(a.MatchValue, []int64{subject.UserID})
	case model.MatchGroup:
		return matchID(a.MatchValue, subject.GroupIDs)
	case model.MatchRole:
		return matchID(a.MatchValue, subject.RoleIDs)
	case model.MatchChannel:
		return a.MatchValue == subject.Channel
	default:
		// An unknown dimension is not a reason to widen the match. A row this
		// code cannot interpret must never be read as "applies to everyone".
		return false
	}
}

// matchID compares an identity condition, whose value is stored as text
// because one column carries four dimensions. A value that is not an id
// matches nothing: a malformed row must fail closed.
func matchID(value string, ids []int64) bool {
	want, err := strconv.ParseInt(value, 10, 64)
	if err != nil || want == 0 {
		return false
	}
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
