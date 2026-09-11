package agent

import (
	"testing"

	"flexie.io/sag/internal/model"
)

// An agent's pinned mode overrides whatever the Gateway requested (KB/27):
// background pins Mode C, inline pins the synchronous modes (a background request
// is forced back to continue), and auto leaves the Gateway's choice.
func TestEffectiveModeEnforcesThePin(t *testing.T) {
	cases := []struct {
		requested, pinned, want string
	}{
		// auto: the Gateway's choice stands, defaulting to continue.
		{"continue", model.DelegationModeAuto, model.HandoffContinue},
		{"terminal", model.DelegationModeAuto, model.HandoffTerminal},
		{"background", model.DelegationModeAuto, model.HandoffBackground},
		{"", model.DelegationModeAuto, model.HandoffContinue},
		{"", "", model.HandoffContinue}, // empty pin is auto

		// background: always background, whatever the Gateway asked.
		{"continue", model.DelegationModeBackground, model.HandoffBackground},
		{"terminal", model.DelegationModeBackground, model.HandoffBackground},
		{"", model.DelegationModeBackground, model.HandoffBackground},

		// inline: synchronous only. continue/terminal honoured, background forced
		// back to continue.
		{"continue", model.DelegationModeInline, model.HandoffContinue},
		{"terminal", model.DelegationModeInline, model.HandoffTerminal},
		{"background", model.DelegationModeInline, model.HandoffContinue},
		{"", model.DelegationModeInline, model.HandoffContinue},
	}
	for _, c := range cases {
		if got := ResolveHandoff(c.requested, c.pinned); got != c.want {
			t.Errorf("ResolveHandoff(%q, %q) = %q, want %q", c.requested, c.pinned, got, c.want)
		}
	}
}
