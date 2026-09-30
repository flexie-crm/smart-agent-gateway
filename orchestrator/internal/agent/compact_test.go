package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
)

// compactingAgent is the agent store as far as compacting reaches into it.
type compactingAgent struct {
	store.AgentStore
	steps  []*model.AgentStep
	latest *model.Compaction
	saved  []*model.Compaction
	calls  []*model.ModelCall
}

func (a *compactingAgent) Transcript(context.Context, int64) ([]*model.AgentStep, error) {
	return a.steps, nil
}

func (a *compactingAgent) LatestCompaction(context.Context, int64) (*model.Compaction, error) {
	if a.latest == nil {
		return nil, store.ErrNotFound
	}
	return a.latest, nil
}

func (a *compactingAgent) SaveCompaction(_ context.Context, c *model.Compaction) error {
	a.saved = append(a.saved, c)
	return nil
}

func (a *compactingAgent) RecordModelCall(_ context.Context, c *model.ModelCall) error {
	a.calls = append(a.calls, c)
	return nil
}

type compactingStore struct {
	store.Store
	agent  *compactingAgent
	models *measuringModels
}

func (s *compactingStore) Agent() store.AgentStore      { return s.agent }
func (s *compactingStore) AIModels() store.AIModelStore { return s.models }

// measuringModels keeps what the model store was taught about a model's tokens.
type measuringModels struct {
	store.AIModelStore
	taught []measured
}

type measured struct {
	id, chars, tokens int64
	keep              float64
}

func (m *measuringModels) MeasureTokens(_ context.Context, id int64, chars, tokens int64, keep float64) error {
	m.taught = append(m.taught, measured{id, chars, tokens, keep})
	return nil
}

// summarizer is a model that answers with a fixed summary and keeps what it
// was asked.
type summarizer struct {
	provider.Provider
	answer string
	asked  []provider.GenerateRequest
}

func (s *summarizer) Generate(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	s.asked = append(s.asked, req)
	return &provider.GenerateResponse{
		Message: provider.Message{Role: provider.RoleAssistant, Content: s.answer},
		Usage:   provider.Usage{InputTokens: 1200, OutputTokens: 80},
	}, nil
}

func compactingRunner(steps []*model.AgentStep, latest *model.Compaction) (*Runner, *compactingAgent) {
	ag := &compactingAgent{steps: steps, latest: latest}
	return &Runner{store: &compactingStore{agent: ag, models: &measuringModels{}}, log: zerolog.Nop()}, ag
}

func toolStep(seq int, text, name, result string) *model.AgentStep {
	return &model.AgentStep{
		Seq: seq, Kind: model.StepAssistant, Text: text,
		ToolCalls: []*model.ToolCall{{
			ToolCallID: "call_" + name, ToolName: name, Status: model.ToolCallCompleted,
			Args: json.RawMessage(`{"q":"x"}`), Result: json.RawMessage(result),
		}},
	}
}

// A second summary is written from the first plus what came after it, and an
// agent's inner steps, which the Gateway never reads, are not in it either.
func TestASummaryIsWrittenFromTheLastOneOnward(t *testing.T) {
	steps := []*model.AgentStep{
		userStep(0, "the old question"),
		{Seq: 1, Kind: model.StepAssistant, Text: "the old answer"},
		userStep(2, "the new question"),
		toolStep(3, "", "lookup", `{"found":"the thing it found"}`),
		{Seq: 4, Kind: model.StepAssistant, Text: "an agent working", AgentKey: "research", ParentToolCallID: "call_d"},
		{Seq: 5, Kind: model.StepAssistant, Text: "the new answer"},
	}
	r, store := compactingRunner(steps, &model.Compaction{SessionID: 9, ThroughSeq: 1, Summary: "THE EARLIER SUMMARY"})
	vendor := &summarizer{answer: "  the whole story  "}
	resolved := &provider.Resolved{
		Provider: vendor,
		Model:    &model.AIModel{ID: 7, ModelKey: "writer"},
		Vendor:   &model.AIVendor{VendorKey: "somebody"},
	}

	got, err := r.compact(context.Background(), CompactRequest{
		WorkspaceID: 1, SessionID: 9, ModelID: 7, By: model.Actor{UserID: 3, Name: "A Person"},
	}, resolved)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}

	if len(vendor.asked) != 1 {
		t.Fatalf("the model was asked %d times", len(vendor.asked))
	}
	sent := vendor.asked[0].Messages
	if len(sent) != 2 || sent[0].Role != provider.RoleSystem || sent[1].Role != provider.RoleUser {
		t.Fatalf("the model was not sent an instruction and the conversation: %+v", sent)
	}
	text := sent[1].Content
	for _, want := range []string{"THE EARLIER SUMMARY", "the new question", "lookup", "the thing it found", "the new answer"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the conversation sent to be summarized has no %q:\n%s", want, text)
		}
	}
	for _, gone := range []string{"the old question", "the old answer", "an agent working"} {
		if strings.Contains(text, gone) {
			t.Fatalf("the conversation sent to be summarized still has %q:\n%s", gone, text)
		}
	}

	// Kept covering the last step the Gateway reads, trimmed, with who asked and
	// what wrote it; and the call is on the cost record.
	if len(store.saved) != 1 || store.saved[0] != got {
		t.Fatalf("saved %d summaries", len(store.saved))
	}
	if got.ThroughSeq != 5 || got.Summary != "the whole story" || got.SessionID != 9 {
		t.Fatalf("kept %+v", got)
	}
	if got.Vendor != "somebody" || got.Model != "writer" || got.CreatedBy != 3 || got.CreatedByName != "A Person" {
		t.Fatalf("the summary does not say who asked and what wrote it: %+v", got)
	}
	if len(store.calls) != 1 || store.calls[0].InputTokens != 1200 || store.calls[0].SessionID != 9 {
		t.Fatalf("the model call was not recorded against the conversation: %+v", store.calls)
	}
	// And what it taught about the model: the characters of the request exactly
	// as it was sent, beside the tokens the vendor said it was.
	sentChars := int64(provider.RequestChars(vendor.asked[0].Messages, vendor.asked[0].Tools))
	if store.calls[0].InputChars != sentChars {
		t.Fatalf("the call recorded %d characters sent, the request was %d", store.calls[0].InputChars, sentChars)
	}
	taught := r.store.(*compactingStore).models.taught
	if len(taught) != 1 || taught[0] != (measured{7, sentChars, 1200, provider.MeasureKeep}) {
		t.Fatalf("the model was taught %+v, want model 7, %d characters as 1,200 tokens", taught, sentChars)
	}
}

// With nothing said since the last summary there is nothing to write, and the
// model is not asked.
func TestNothingSinceTheLastSummaryIsNotSummarized(t *testing.T) {
	steps := []*model.AgentStep{userStep(0, "hi"), {Seq: 1, Kind: model.StepAssistant, Text: "hello"}}
	r, store := compactingRunner(steps, &model.Compaction{ThroughSeq: 1, Summary: "they said hello"})
	vendor := &summarizer{answer: "anything"}

	_, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: vendor, Model: &model.AIModel{}})
	if !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("got %v, want ErrNothingToCompact", err)
	}
	if len(vendor.asked) != 0 || len(store.saved) != 0 {
		t.Fatalf("asked %d times and kept %d summaries of nothing", len(vendor.asked), len(store.saved))
	}
}

// An empty answer is not kept: it would stand in for the whole conversation.
func TestAnEmptySummaryIsNotKept(t *testing.T) {
	r, store := compactingRunner([]*model.AgentStep{userStep(0, "hi")}, nil)

	_, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: &summarizer{answer: " \n "}, Model: &model.AIModel{}})
	if err == nil {
		t.Fatal("an empty summary was accepted")
	}
	if len(store.saved) != 0 {
		t.Fatal("an empty summary was kept")
	}
}

// The next turn reads the summary where the steps it covers were, and those
// steps are not sent. The control is the same conversation with no summary,
// which must read exactly as it always has.
func TestATurnReadsTheSummaryInPlaceOfWhatItCovers(t *testing.T) {
	steps := func() []*model.AgentStep {
		return []*model.AgentStep{
			userStep(0, "the old question"),
			toolStep(1, "", "lookup", `{"found":"old"}`),
			{Seq: 2, Kind: model.StepAssistant, Text: "the old answer"},
			userStep(3, "the new question"),
		}
	}

	plain, _ := compactingRunner(steps(), nil)
	whole, err := plain.buildTranscript(context.Background(), Turn{SessionID: 9, SystemPrompt: "instructions"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(whole) != 6 { // system, user, assistant, tool, assistant, user
		t.Fatalf("a conversation with no summary is not sent whole: %+v", whole)
	}

	compacted, _ := compactingRunner(steps(), &model.Compaction{ThroughSeq: 2, Summary: "THE SUMMARY"})
	sent, err := compacted.buildTranscript(context.Background(), Turn{SessionID: 9, SystemPrompt: "instructions"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(sent) != 3 {
		t.Fatalf("sent %d messages, want the instructions, the summary and the new question: %+v", len(sent), sent)
	}
	if sent[0].Content != "instructions" || sent[1].Role != provider.RoleSystem ||
		!strings.HasSuffix(sent[1].Content, "THE SUMMARY") || sent[2].Content != "the new question" {
		t.Fatalf("the summary is not where the history was: %+v", sent)
	}
}

// The summary is a system message so the trim can never drop it: history goes
// oldest first, and a summary anywhere in it would be the oldest there is.
func TestTheSummarySurvivesTheTrim(t *testing.T) {
	long := strings.Repeat("words that take up room ", 200)
	messages := withSummary([]provider.Message{
		{Role: provider.RoleSystem, Content: "instructions"},
		{Role: provider.RoleUser, Content: "an old question " + long},
		{Role: provider.RoleAssistant, Content: "an old answer " + long},
		{Role: provider.RoleUser, Content: "the new question"},
	}, "THE SUMMARY")

	trimmed := provider.TrimToBudget(messages, 2000)

	kept := false
	for _, m := range trimmed {
		if strings.Contains(m.Content, "an old question") {
			t.Fatal("the trim dropped nothing, so this proves nothing about what it keeps")
		}
		if m.Role == provider.RoleSystem && strings.HasSuffix(m.Content, "THE SUMMARY") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the trim dropped the summary: %+v", trimmed)
	}
}

// chainingSummarizer answers every part with a numbered summary, and keeps
// every request, so a test can read what each part carried.
type chainingSummarizer struct {
	provider.Provider
	asked []provider.GenerateRequest
}

func (c *chainingSummarizer) Generate(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	c.asked = append(c.asked, req)
	return &provider.GenerateResponse{Message: provider.Message{
		Role: provider.RoleAssistant, Content: fmt.Sprintf("SUMMARY-%d", len(c.asked)),
	}}, nil
}

func (c *chainingSummarizer) parts() []string {
	out := make([]string, 0, len(c.asked))
	for _, req := range c.asked {
		out = append(out, req.Messages[1].Content)
	}
	return out
}

// Everything a tool said reaches the model writing the summary, whole: a
// 30,000 character answer and a 5,000 character argument, not their first
// 2,000 and 500. What matters in them is the model's to decide.
func TestTheSummaryIsWrittenFromEverythingAToolSaid(t *testing.T) {
	schema := `{"tables":"` + strings.Repeat("customers(id, name, vat_number, created_at) ", 700) + `"}`
	query := `{"sql":"SELECT ` + strings.Repeat("a_long_column_name, ", 250) + `x FROM t"}`
	steps := []*model.AgentStep{
		userStep(0, "what is in the database?"),
		{Seq: 1, Kind: model.StepAssistant, ToolCalls: []*model.ToolCall{{
			ToolCallID: "c1", ToolName: "db_schema", Status: model.ToolCallCompleted,
			Args: json.RawMessage(query), Result: json.RawMessage(schema),
		}}},
	}
	if len(schema) < 30_000 || len(query) < 5_000 {
		t.Fatal("the tool call is not big enough to have been cut, so this proves nothing")
	}
	r, _ := compactingRunner(steps, nil)
	vendor := &chainingSummarizer{}
	if _, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: vendor, Model: &model.AIModel{ContextWindow: 200_000}}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(vendor.asked) != 1 {
		t.Fatalf("a conversation that fits was read in %d parts", len(vendor.asked))
	}
	sent := vendor.parts()[0]
	if !strings.Contains(sent, schema) || !strings.Contains(sent, query) {
		t.Fatal("the summary was not written from the whole of what the tool was sent and answered")
	}
	if strings.Contains(sent, "cut short") {
		t.Fatal("something was cut")
	}
}

// Too long to read in one go, a conversation is read in parts, each carrying
// the summary so far, and every part of it is read: nothing is left out to fit.
// Each request fits the room the model's window gives.
func TestALongConversationIsReadInPartsAndNothingIsLeftOut(t *testing.T) {
	var steps []*model.AgentStep
	var said []string
	for i := 0; i < 24; i++ {
		text := fmt.Sprintf("MARK-%02d %s", i, strings.TrimSpace(strings.Repeat("words that take up room ", 60)))
		said = append(said, text)
		steps = append(steps, userStep(i, text))
	}
	window := &model.AIModel{ContextWindow: 12_000}
	r, store := compactingRunner(steps, &model.Compaction{ThroughSeq: -1, Summary: "THE EARLIER SUMMARY"})
	vendor := &chainingSummarizer{}
	got, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: vendor, Model: window})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}

	parts := vendor.parts()
	if len(parts) < 3 {
		t.Fatalf("read in %d parts; the conversation does not outgrow the window, so this proves nothing", len(parts))
	}
	all := strings.Join(parts, "")
	for _, text := range said {
		if !strings.Contains(all, text) {
			t.Fatalf("%q was never read", text[:8])
		}
	}
	// Each part carries the summary before it: the first the earlier one, every
	// other the one just written.
	if !strings.Contains(parts[0], "THE EARLIER SUMMARY") {
		t.Fatal("the first part does not carry the earlier summary")
	}
	for i := 1; i < len(parts); i++ {
		if !strings.Contains(parts[i], fmt.Sprintf("SUMMARY-%d", i)) {
			t.Fatalf("part %d does not carry the summary written from part %d", i+1, i)
		}
	}
	// And every request fits what the model can read.
	room := provider.BudgetFor(window, compactMaxTokens)
	for i, req := range vendor.asked {
		if size := provider.RequestChars(req.Messages, nil); size > room {
			t.Fatalf("part %d is %d characters, over the %d the window gives", i+1, size, room)
		}
	}
	// The summary kept is the last one, covering the last step.
	if got.Summary != fmt.Sprintf("SUMMARY-%d", len(parts)) || got.ThroughSeq != 23 || len(store.saved) != 1 {
		t.Fatalf("kept %q through %d", got.Summary, got.ThroughSeq)
	}
}

// One step bigger than a part on its own (a tool answer larger than the
// window) is split across parts, never cut: put back together, it is exactly
// what the tool answered.
func TestAStepTooBigForAPartIsSplitNotCut(t *testing.T) {
	answer := `"` + strings.Repeat("row ", 5000) + `"`
	steps := []*model.AgentStep{toolStep(0, "", "query", answer)}
	r, _ := compactingRunner(steps, nil)
	vendor := &chainingSummarizer{}
	if _, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: vendor, Model: &model.AIModel{ContextWindow: 12_000}}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	parts := vendor.parts()
	if len(parts) < 2 {
		t.Fatal("the step fitted in one part, so this proves nothing")
	}
	var whole strings.Builder
	for i, part := range parts {
		if i > 0 {
			_, part, _ = strings.Cut(part, "The conversation since:\n\n")
			part = strings.TrimPrefix(part, continuedNote)
		}
		whole.WriteString(strings.TrimSuffix(part, continuesNote))
	}
	if !strings.Contains(whole.String(), answer) {
		t.Fatal("the answer, put back together from its parts, is not what the tool answered")
	}
}

// A summary that leaves the window no room to read on says so, rather than
// reading the rest a few lines at a time or pretending it read it.
func TestASummaryThatLeavesNoRoomSaysSo(t *testing.T) {
	r, store := compactingRunner([]*model.AgentStep{userStep(5, "and one more thing")},
		&model.Compaction{ThroughSeq: 4, Summary: strings.Repeat("a very long summary ", 1000)})
	_, err := r.compact(context.Background(), CompactRequest{SessionID: 9},
		&provider.Resolved{Provider: &chainingSummarizer{}, Model: &model.AIModel{ContextWindow: 12_000}})
	if !errors.Is(err, ErrNoRoomToRead) || len(store.saved) != 0 {
		t.Fatalf("got %v and kept %d summaries, want ErrNoRoomToRead and nothing kept", err, len(store.saved))
	}
}

// The parts are measured the way a request is: every character's length once
// it is escaped, which has to be what the encoder actually writes.
func TestEscapedLengthIsWhatTheEncoderWrites(t *testing.T) {
	for _, s := range []string{
		"plain", `a "quote" and a \ slash`, "lines\nand\ttabs\r", "\b\f\x01\x1f",
		"<tag> & amp", "\u2028\u2029", "é日😀", strings.Repeat("mixed <\n\"é", 50),
	} {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := escapedLen(s), len(raw)-2; got != want {
			t.Fatalf("%q is %d escaped, the encoder writes %d", s, got, want)
		}
	}
}
