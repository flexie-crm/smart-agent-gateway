package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
)

// measuredAgent is the agent store as far as one plain answer reaches into it.
type measuredAgent struct {
	store.AgentStore
	mu    sync.Mutex
	next  int
	calls []*model.ModelCall
}

func (a *measuredAgent) NextSeq(context.Context, int64) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	return a.next, nil
}
func (a *measuredAgent) SaveStep(context.Context, *model.AgentStep) error { return nil }
func (a *measuredAgent) TouchChat(context.Context, int64) error           { return nil }
func (a *measuredAgent) RecordModelCall(_ context.Context, c *model.ModelCall) error {
	a.calls = append(a.calls, c)
	return nil
}

type measuredStore struct {
	store.Store
	agent  *measuredAgent
	models *measuringModels
}

func (s *measuredStore) Agent() store.AgentStore      { return s.agent }
func (s *measuredStore) AIModels() store.AIModelStore { return s.models }

// answersOnce is a model that answers every step with the same words and
// keeps what it was sent.
type answersOnce struct {
	provider.Provider
	asked []provider.GenerateRequest
}

func (a *answersOnce) Stream(_ context.Context, req provider.GenerateRequest) (<-chan provider.StreamEvent, error) {
	a.asked = append(a.asked, req)
	events := make(chan provider.StreamEvent, 2)
	events <- provider.StreamEvent{Kind: provider.EventContentDelta, ContentDelta: "the answer"}
	events <- provider.StreamEvent{Kind: provider.EventDone}
	close(events)
	return events, nil
}

// How full the conversation is, as a turn reports it, is exactly what the step
// sent (the trim's own measure of it), and once more with the answer in. The
// base is the system prompt and the tools and nothing of the conversation.
func TestATurnMeasuresWhatItSendsAndThenTheAnswer(t *testing.T) {
	r := &Runner{store: &measuredStore{agent: &measuredAgent{}, models: &measuringModels{}}, log: zerolog.Nop()}
	vendor := &answersOnce{}
	resolved := &provider.Resolved{
		Provider: vendor,
		Model:    &model.AIModel{ID: 5, ModelKey: "m"},
		Vendor:   &model.AIVendor{VendorKey: "v"},
	}
	type measured struct {
		model       int64
		chars, base int
	}
	var heard []measured
	turn := Turn{
		SessionID: 1, WorkspaceID: 1, SystemPrompt: "the instructions",
		ContextUsed: func(m *model.AIModel, chars, base int) { heard = append(heard, measured{m.ID, chars, base}) },
	}
	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: "the instructions"},
		{Role: provider.RoleUser, Content: "a question"},
	}
	tools := []provider.ToolDef{{Name: "a_tool", Description: "does a thing", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	if _, err := r.loop(context.Background(), turn, resolved, messages, tools, nil, map[string]tool.Schema{}, chat.NewStream(nowhere{}), nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(vendor.asked) != 1 || len(heard) != 2 {
		t.Fatalf("one step, %d asked, %d measured; want one asked and two measured", len(vendor.asked), len(heard))
	}

	sent := vendor.asked[0]
	if heard[0].chars != provider.RequestChars(sent.Messages, sent.Tools) || heard[0].model != 5 {
		t.Fatalf("measured %+v before the step, but the step sent %d", heard[0], provider.RequestChars(sent.Messages, sent.Tools))
	}
	withAnswer := append(append([]provider.Message{}, messages...), provider.Message{Role: provider.RoleAssistant, Content: "the answer"})
	if heard[1].chars != provider.RequestChars(withAnswer, tools) {
		t.Fatalf("measured %d with the answer in, want %d", heard[1].chars, provider.RequestChars(withAnswer, tools))
	}
	base := provider.RequestChars([]provider.Message{{Role: provider.RoleSystem, Content: "the instructions"}}, tools)
	if heard[0].base != base || heard[1].base != base {
		t.Fatalf("the base was %d and %d, want the system prompt and the tools, %d", heard[0].base, heard[1].base, base)
	}
}

// countsItsTokens is a model that answers and then says how many tokens the
// request was, the way a vendor does at the end of a stream.
type countsItsTokens struct {
	provider.Provider
	tokens int64
	asked  []provider.GenerateRequest
}

func (c *countsItsTokens) Stream(_ context.Context, req provider.GenerateRequest) (<-chan provider.StreamEvent, error) {
	c.asked = append(c.asked, req)
	events := make(chan provider.StreamEvent, 3)
	events <- provider.StreamEvent{Kind: provider.EventContentDelta, ContentDelta: "the answer"}
	events <- provider.StreamEvent{Kind: provider.EventUsage, Usage: &provider.Usage{InputTokens: c.tokens, OutputTokens: 3}}
	events <- provider.StreamEvent{Kind: provider.EventDone}
	close(events)
	return events, nil
}

// One step of a turn: what it sent and what the vendor said that was.
func stepWith(t *testing.T, tokens int64, messages []provider.Message) (*countsItsTokens, *measuredAgent, *measuringModels) {
	t.Helper()
	agent, models := &measuredAgent{}, &measuringModels{}
	r := &Runner{store: &measuredStore{agent: agent, models: models}, log: zerolog.Nop()}
	vendor := &countsItsTokens{tokens: tokens}
	resolved := &provider.Resolved{Provider: vendor, Model: &model.AIModel{ID: 5, ModelKey: "m"}, Vendor: &model.AIVendor{VendorKey: "v"}}
	tools := []provider.ToolDef{{Name: "a_tool", Description: "does a thing", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	if _, err := r.loop(context.Background(), Turn{SessionID: 1, WorkspaceID: 1}, resolved, messages, tools, nil, map[string]tool.Schema{}, chat.NewStream(nowhere{}), nil); err != nil {
		t.Fatalf("loop: %v", err)
	}
	return vendor, agent, models
}

// A step teaches its model what its tokens are: the characters of the request
// exactly as it went, beside the tokens the vendor said it was.
func TestAStepTeachesItsModelWhatItsTokensAre(t *testing.T) {
	question := strings.Repeat("a question with some length to it. ", 40)
	vendor, agent, models := stepWith(t, 400, []provider.Message{
		{Role: provider.RoleSystem, Content: "the instructions"},
		{Role: provider.RoleUser, Content: question},
	})
	sent := int64(provider.RequestChars(vendor.asked[0].Messages, vendor.asked[0].Tools))
	if len(models.taught) != 1 || models.taught[0] != (measured{5, sent, 400, provider.MeasureKeep}) {
		t.Fatalf("the model was taught %+v, want model 5, %d characters as 400 tokens", models.taught, sent)
	}
	if len(agent.calls) != 1 || agent.calls[0].InputChars != sent || agent.calls[0].InputTokens != 400 {
		t.Fatalf("the call was recorded as %+v, want %d characters beside 400 tokens", agent.calls, sent)
	}
}

// Nothing is learned from a step that carried a file, whose bytes the vendor
// counts in its own way, or from a vendor that did not say.
func TestAStepThatCannotTeachTeachesNothing(t *testing.T) {
	withFile := []provider.Message{{Role: provider.RoleUser, Content: "what is in this?",
		Files: []provider.FilePart{{FileName: "a.png", MediaType: "image/png", Data: []byte(strings.Repeat("x", 5000))}}}}
	if _, agent, models := stepWith(t, 900, withFile); len(models.taught) != 0 || agent.calls[0].InputChars != 0 {
		t.Fatalf("a step with a file taught %+v and recorded %d characters", models.taught, agent.calls[0].InputChars)
	}
	plain := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("words ", 200)}}
	if _, _, models := stepWith(t, 0, plain); len(models.taught) != 0 {
		t.Fatalf("a step whose vendor said nothing taught %+v", models.taught)
	}
}
