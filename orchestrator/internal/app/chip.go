package app

import (
	"encoding/json"
	"fmt"
	"strconv"

	"flexie.io/sag/internal/model"
)

// Chip is what the chat draws for work that outlives the turn that asked for
// it: a background agent, or a whole fleet.
//
// It is ONE type because there are three places that produce it (a live push
// when a background agent changes state, a live push when a fleet member
// reports, and the list a reload rebuilds) and one place that renders it. They
// were three hand-written maps with the same keys, which is exactly the shape
// that drifts: the client reads whichever spelling it was written against and
// silently ignores the rest.
type Chip struct {
	ID string `json:"id"`
	// Kind is empty for one agent, which is what every chip was before fleets
	// existed, and ChipKindFleet for a batch.
	Kind   string `json:"kind,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// CreatedAt and CompletedAt bracket the work. A chip that has finished shows
	// how long it took rather than a clock still running.
	CreatedAt   int64           `json:"created_at"`
	CompletedAt int64           `json:"completed_at,omitempty"`
	Progress    json.RawMessage `json:"progress,omitempty"`
	// Agents and Done are a fleet's numbers: how many were started together and
	// how many are back. Zero on a single agent's chip.
	Agents int `json:"agents,omitempty"`
	Done   int `json:"done,omitempty"`
	// ChatUID is set only on a live push, where the client has to know which
	// conversation the chip belongs to. A reload asked about one conversation
	// and does not need telling.
	ChatUID string `json:"chat_uid,omitempty"`
}

// Nothing here carries the agent's RESULT, and that is the rule rather than an
// omission. An agent's output is bare technical fact written for the Gateway,
// which reads it and tells the person what it means (KB/27). Putting it on a
// chip puts "```json" and "the raw response body:" in front of somebody who
// asked what the price of gold was.

// ChipKindFleet marks a chip that stands for a batch rather than one agent.
const ChipKindFleet = "fleet"

// AgentChip is one background agent's chip.
func AgentChip(del *model.AgentDelegation, name string) Chip {
	if name == "" {
		name = del.AgentKey
	}
	// A parked agent's record still says "running"; the chip should read
	// "waiting for approval", so a reload matches what the socket showed live.
	status := del.Status
	if del.Waiting() {
		status = model.SessionWaitingApproval
	}
	chip := Chip{
		ID:        strconv.FormatInt(del.ID, 10),
		Agent:     del.AgentKey,
		Name:      name,
		Status:    status,
		CreatedAt: del.CreatedAt.Unix(),
		Progress:  del.Progress,
	}
	if del.CompletedAt != nil {
		chip.CompletedAt = del.CompletedAt.Unix()
	}
	return chip
}

// FleetChip is a whole batch as one chip: what is working, how many, and how
// many are back.
//
// The status comes from the FLEET row and the numbers from the members, and
// that split is deliberate. Whether the batch is over is a decision somebody
// made once (CloseFleet, and cancelled is not the same as done); how far along
// it is, is a count of rows. Deriving the status from the count instead would
// draw a cancelled fleet as a finished one.
func FleetChip(fleet *model.AgentFleet, members []*model.AgentDelegation, names map[string]string) Chip {
	done, completed := 0, int64(0)
	for _, m := range members {
		if !m.Terminal() {
			continue
		}
		done++
		// The batch finished when its LAST member did.
		if m.CompletedAt != nil && m.CompletedAt.Unix() > completed {
			completed = m.CompletedAt.Unix()
		}
	}
	size := fleet.Size
	if size < len(members) {
		size = len(members)
	}
	chip := Chip{
		ID:        FleetChipID(fleet.ID),
		Kind:      ChipKindFleet,
		Name:      fleetChipName(members, names),
		Status:    fleetChipStatus(fleet),
		Agents:    size,
		Done:      done,
		CreatedAt: fleet.CreatedAt.Unix(),
	}
	if fleet.CompletedAt != nil {
		chip.CompletedAt = fleet.CompletedAt.Unix()
	} else if chip.Status != model.DelegationRunning {
		chip.CompletedAt = completed
	}
	return chip
}

// FleetChipID keeps a fleet's chip id from colliding with a delegation's. They
// share one list on the client, and both are row ids from different tables.
func FleetChipID(id int64) string { return "fleet-" + strconv.FormatInt(id, 10) }

// fleetChipStatus maps a fleet onto the words a chip already knows, so nothing
// on the client needs a new case for a batch.
func fleetChipStatus(fleet *model.AgentFleet) string {
	switch fleet.Status {
	case model.FleetCancelled:
		return model.DelegationCancelled
	case model.FleetDone:
		return model.DelegationDone
	default:
		return model.DelegationRunning
	}
}

// fleetChipName says what is working in one phrase: the agent, when they are all
// the same one, and the count when they are not. Five names in a chip is a list
// nobody needs while the work is still going.
func fleetChipName(members []*model.AgentDelegation, names map[string]string) string {
	if len(members) == 0 {
		return "agents"
	}
	key := members[0].AgentKey
	for _, m := range members[1:] {
		if m.AgentKey != key {
			return fmt.Sprintf("%d agents", len(members))
		}
	}
	name := names[key]
	if name == "" {
		name = key
	}
	if len(members) == 1 {
		return name
	}
	return fmt.Sprintf("%d × %s", len(members), name)
}
