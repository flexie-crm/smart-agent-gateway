package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"flexie.io/sag/internal/model"
)

// The dashboard has to be true. A number that is merely plausible is worse than
// no number, because somebody will act on it.

func TestStatsCountsWhatActuallyHappened(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	// A fresh workspace: nothing has run, and the dashboard says so rather than
	// inventing a zero it did not count.
	stats := env.stats(token)
	if stats.Total.Runs != 0 || stats.Today.ToolCalls != 0 {
		t.Fatalf("a workspace that has done nothing reports work: %+v", stats)
	}
	if stats.Configured.Vendors != 0 {
		t.Fatalf("a workspace with no vendor claims one: %+v", stats.Configured)
	}
	// It also has the tools this build ships, synced at boot.
	if stats.Configured.Tools == 0 {
		t.Fatal("the workspace was offered none of the build's tools")
	}

	// One turn, one tool call.
	vendor := newFakeVendor(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"It is midnight."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":7,"total_tokens":37}}`,
		},
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "time?", "model_id": modelID})
	session := env.sessionOf(frames)
	env.waitForRun(session.ID)
	// The conversation is named by a SECOND model call, made after the answer is
	// sent so the user never waits on it. Its tokens are real money, so the
	// dashboard counts them, and a test that read the numbers before it landed
	// would be asserting on a turn that was not finished being paid for.
	env.waitForTitle(session.ID)

	stats = env.stats(token)

	if stats.Total.Runs != 1 {
		t.Fatalf("the turn was not counted: %+v", stats.Total)
	}
	if stats.Total.ToolCalls != 1 {
		t.Fatalf("the tool call was not counted: %+v", stats.Total)
	}
	// The tokens are the vendor's own numbers, not a guess: 30/7 for the turn,
	// plus 10/5 for the call that named the conversation. The naming call is a
	// real model call and it costs real money, so a dashboard that hid it would
	// be under-reporting the bill.
	if stats.Total.InputTokens != 40 || stats.Total.OutputTokens != 12 {
		t.Fatalf("the tokens are not what the vendor reported: %+v", stats.Total)
	}
	// Today is a subset of ever, and this all happened today.
	if stats.Today.Runs != stats.Total.Runs {
		t.Fatalf("today and ever disagree about a turn that just ran: %+v", stats)
	}

	if stats.Configured.Vendors != 1 || stats.Configured.Models != 1 {
		t.Fatalf("the vendor and model are not reported as configured: %+v", stats.Configured)
	}
	if len(stats.Models) != 1 || stats.Models[0].Calls == 0 {
		t.Fatalf("the model's usage is missing: %+v", stats.Models)
	}
	if len(stats.Tools) != 1 || stats.Tools[0].Name != "current_time" || stats.Tools[0].Calls != 1 {
		t.Fatalf("the tool's usage is missing: %+v", stats.Tools)
	}
	// Usage carries the friendly name for the dashboard, not just the alias.
	if stats.Tools[0].FriendlyName != "Current time" {
		t.Fatalf("tool usage should carry the friendly name, got %q", stats.Tools[0].FriendlyName)
	}
}

// A turn stopped waiting on a person is NOT running: it holds nothing and can
// sit for hours. Counting it as running would make an idle gateway look busy.
func TestStatsSeparatesWaitingFromRunning(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.GatewayAgent()
	vendor := parkingVendor(t, modelID, "Done.")
	env.pointModelAt(modelID, vendor)

	frames := env.streamTurn(token, map[string]any{
		"prompt": "disable that model", "model_id": modelID,
	})
	env.waitForRun(env.sessionOf(frames).ID)

	stats := env.stats(token)
	if stats.Live.WaitingApproval != 1 {
		t.Fatalf("the parked turn is not reported as waiting: %+v", stats.Live)
	}
	if stats.Live.Running != 0 {
		t.Fatalf("a turn waiting on a person is reported as running: %+v", stats.Live)
	}

	// A refusal is not a failure, and the dashboard keeps them apart.
	card := confirmRequest(t, frames[len(frames)-1])
	env.streamTurn(token, map[string]any{
		"resume_token": card.Token, "resume_action": "rejected",
	})

	stats = env.stats(token)
	if stats.Live.WaitingApproval != 0 {
		t.Fatalf("an answered approval is still reported as waiting: %+v", stats.Live)
	}
	var refused int64
	for _, tool := range stats.Tools {
		refused += tool.Refused
	}
	if refused != 1 {
		t.Fatalf("the refusal was not recorded as one: %+v", stats.Tools)
	}
	// The same refusal shows up in the period aggregate the dashboard reads for
	// its "refused today" tile, counted apart from failures.
	if stats.Today.Refused != 1 {
		t.Fatalf("the refusal was not counted in today's total: %+v", stats.Today)
	}
	if stats.Today.Failed != 0 {
		t.Fatalf("a refusal was counted as a failure: %+v", stats.Today)
	}
}

// The numbers belong to the workspace that made them.
func TestStatsDoNotLeakAcrossWorkspaces(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	other := &model.Workspace{Slug: "globex", Name: "Globex"}
	if err := env.app.Store.Workspaces().Create(context.Background(), other); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := env.app.Store.Vendors().Create(context.Background(), &model.AIVendor{
		WorkspaceID: other.ID, VendorKey: model.VendorAnthropic, Name: "Theirs",
	}); err != nil {
		t.Fatalf("create vendor: %v", err)
	}

	stats := env.stats(token)
	if stats.Configured.Vendors != 0 {
		t.Fatalf("another workspace's vendor was counted here: %+v", stats.Configured)
	}
}

func (e *testEnv) stats(token string) statsResponse {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/v1/stats", token, nil)
	e.expectStatus(rec, http.StatusOK)

	var stats statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		e.t.Fatalf("decode stats: %v", err)
	}
	return stats
}
