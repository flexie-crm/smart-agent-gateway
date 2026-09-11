package app

import (
	"math"
	"testing"

	"flexie.io/sag/internal/chat"
)

// A background agent's chip accumulates its exact token spend and cost
// across model calls while showing what it is doing now, and drops the
// agent's prose and reasoning so nothing it says leaks to the chip (KB/27).
func TestProgressAccumulatesSpend(t *testing.T) {
	p := &delegationProgress{}

	// A tool starts: the activity and step advance, no tokens yet.
	if !p.fold(chat.Frame{Type: chat.FrameToolPreparing, Message: chat.ToolMessage{Narration: "Searching the web"}}) {
		t.Fatal("a tool starting should change the chip")
	}

	// Two model calls report exact usage; both accumulate.
	p.fold(chat.Frame{Type: chat.FrameUsage, Message: chat.UsageMessage{InputTokens: 1200, OutputTokens: 300, Cost: 0.0021}})
	p.fold(chat.Frame{Type: chat.FrameUsage, Message: chat.UsageMessage{InputTokens: 800, OutputTokens: 200, Cost: 0.0014}})

	// The agent's reasoning and prose must never touch the chip.
	if p.fold(chat.Frame{Type: chat.FrameReasoningDelta, Message: "thinking out loud"}) {
		t.Fatal("reasoning must not change the chip")
	}
	if p.fold(chat.Frame{Type: chat.FrameDelta, Message: "raw answer text"}) {
		t.Fatal("prose must not change the chip")
	}

	// A second tool advances the step and moves the activity on.
	if !p.fold(chat.Frame{Type: chat.FrameToolPreparing, Message: chat.ToolMessage{FriendlyName: "Reading the report"}}) {
		t.Fatal("a second tool should change the chip")
	}

	m := p.snapshot()
	if m["step"] != 2 {
		t.Fatalf("step = %v, want 2", m["step"])
	}
	if m["activity"] != "Reading the report" {
		t.Fatalf("activity = %v, want the latest tool", m["activity"])
	}
	if m["tokens"] != int64(2500) {
		t.Fatalf("tokens total = %v, want 2500", m["tokens"])
	}
	if m["tokens_in"] != int64(2000) {
		t.Fatalf("tokens_in = %v, want 2000", m["tokens_in"])
	}
	if m["tokens_out"] != int64(500) {
		t.Fatalf("tokens_out = %v, want 500", m["tokens_out"])
	}
	if cost, ok := m["cost"].(float64); !ok || math.Abs(cost-0.0035) > 1e-9 {
		t.Fatalf("cost = %v, want ~0.0035", m["cost"])
	}
}

// The running token total survives a park and resume: an agent that stops
// for approval (waiting flag on, spend kept) and starts again keeps counting up
// from where it left off, rather than resetting to zero.
func TestProgressSurvivesParkAndResume(t *testing.T) {
	p := &delegationProgress{}
	p.fold(chat.Frame{Type: chat.FrameUsage, Message: chat.UsageMessage{InputTokens: 1000, OutputTokens: 250, Cost: 0.002}})

	// Park for approval: the chip flips to waiting but the spend is untouched.
	p.setState("Waiting for your approval", true)
	m := p.snapshot()
	if m["waiting"] != true {
		t.Fatal("a parked delegation should be marked waiting")
	}
	if m["tokens"] != int64(1250) {
		t.Fatalf("tokens after park = %v, want 1250 kept", m["tokens"])
	}

	// Resume: waiting clears, and a further model call adds to the SAME total.
	p.setState("Working…", false)
	p.fold(chat.Frame{Type: chat.FrameUsage, Message: chat.UsageMessage{InputTokens: 500, OutputTokens: 100, Cost: 0.001}})
	m = p.snapshot()
	if _, waiting := m["waiting"]; waiting {
		t.Fatal("a resumed delegation should not be marked waiting")
	}
	if m["tokens"] != int64(1850) {
		t.Fatalf("tokens after resume = %v, want 1850 (1250 + 600)", m["tokens"])
	}
	if cost, ok := m["cost"].(float64); !ok || math.Abs(cost-0.003) > 1e-9 {
		t.Fatalf("cost after resume = %v, want ~0.003", m["cost"])
	}
}

// A chip shows a field only once there is something to say: before any usage
// arrives it carries the activity, not a zero token count or a zero cost.
func TestProgressOmitsEmptySpend(t *testing.T) {
	p := &delegationProgress{}
	if !p.fold(chat.Frame{Type: chat.FrameToolPreparing, Message: chat.ToolMessage{Narration: "Starting"}}) {
		t.Fatal("a tool starting should change the chip")
	}
	m := p.snapshot()
	if _, ok := m["tokens"]; ok {
		t.Fatal("tokens should be absent until a model call reports usage")
	}
	if _, ok := m["cost"]; ok {
		t.Fatal("cost should be absent until there is spend")
	}
	if m["activity"] != "Starting" {
		t.Fatalf("activity = %v, want Starting", m["activity"])
	}
}
