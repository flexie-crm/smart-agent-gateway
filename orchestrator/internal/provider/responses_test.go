package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The Responses wire, tested against a server that speaks it.
//
// What these are for, beyond the usual: this is a SECOND way of talking to a
// vendor we already talk to, and the classic failure when porting one wire to
// another is that the old one's vocabulary comes along. So several of these
// assert ABSENCE, which is the only way to catch a field that is harmlessly
// ignored today and rejected tomorrow.

// responsesServer replies with the given stream events and records the request.
//
// It writes `event:` as well as `data:`, because that is what the vendor sends
// and a client that only works without it would be one this test had taught to
// be wrong.
func responsesServer(t *testing.T, captured *capturedRequest, events ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		body := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode request body %q: %v", raw, err)
				return
			}
		}
		*captured = capturedRequest{
			path: r.URL.Path, header: r.Header.Clone(),
			query: r.URL.RawQuery, body: body,
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for _, event := range events {
			var typed struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &typed)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustResponses(t *testing.T, baseURL string) *OpenAIResponses {
	t.Helper()
	dialect, ok := DialectFor(model.VendorOpenAI)
	if !ok {
		t.Fatal("no dialect for openai")
	}
	if dialect.Wire != WireResponses {
		t.Fatal("the openai vendor is not on this wire, so this whole suite is testing nothing")
	}
	adapter, err := NewOpenAIResponses(dialect, "test-key", baseURL, nil)
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}
	return adapter
}

func completed(input, output int) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_1","object":"response",`+
		`"created_at":1,"model":"m","status":"completed","output":[],`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`,
		input, output, input+output)
}

func TestResponsesStreamParsesContentAndUsage(t *testing.T) {
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.output_text.delta","delta":"Hello","item_id":"msg_1","output_index":0,"content_index":0,"sequence_number":1}`,
		`{"type":"response.output_text.delta","delta":", world","item_id":"msg_1","output_index":0,"content_index":0,"sequence_number":2}`,
		completed(11, 4),
	)

	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{
		Model:    "gpt-5.6",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	content, _, calls, usage, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if content != "Hello, world" {
		t.Fatalf("content not assembled: %q", content)
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 4 {
		t.Fatalf("usage not captured: %+v", usage)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("wrong auth header: %q", auth)
	}
}

func TestTheResponsesAdapterNeverCallsTheChatWire(t *testing.T) {
	// The hazard of embedding the chat adapter for the endpoints both wires
	// share: a method that should have been overridden silently goes to the old
	// endpoint. Nothing else would notice, because the chat wire still works.
	var got capturedRequest
	srv := responsesServer(t, &got, completed(1, 1))

	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{
		Model: "gpt-5.6", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)

	if strings.Contains(got.path, "chat/completions") {
		t.Fatalf("this adapter sent a request to the chat wire: %s", got.path)
	}
	if !strings.Contains(got.path, "responses") {
		t.Fatalf("expected the responses endpoint, got %s", got.path)
	}
}

func TestBothSpellingsOfThinkingBecomeTheOneEvent(t *testing.T) {
	// This vendor has two, and which one arrives depends on the model. A caller
	// must never learn that, which is the entire job of this layer.
	for _, spelling := range []string{"response.reasoning_text.delta", "response.reasoning_summary_text.delta"} {
		t.Run(spelling, func(t *testing.T) {
			var got capturedRequest
			srv := responsesServer(t, &got,
				fmt.Sprintf(`{"type":%q,"delta":"thinking","item_id":"rs_1","output_index":0,"content_index":0,"summary_index":0,"sequence_number":1}`, spelling),
				`{"type":"response.output_text.delta","delta":"answer","item_id":"msg_1","output_index":1,"content_index":0,"sequence_number":2}`,
				completed(1, 1),
			)
			events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{
				Model: "gpt-5.6", Reasoning: true,
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			content, reasoning, _, _, errs := collect(t, events)
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if reasoning != "thinking" {
				t.Errorf("thinking did not arrive as reasoning: %q", reasoning)
			}
			// And it must NOT have leaked into the answer, which is the
			// invariant every adapter here is held to.
			if content != "answer" {
				t.Errorf("thinking leaked into the content: %q", content)
			}
		})
	}
}

func TestAToolCallArrivesWholeAndCarriesTheCorrelationID(t *testing.T) {
	// The id that matters is `call_id`, not the item's own `id`. The next turn's
	// result is keyed by it, and sending the other one back is a 400 saying the
	// call does not exist. Both are in the fixture so a mix-up is visible.
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.output_item.done","output_index":0,"sequence_number":1,"item":{"type":"function_call","id":"fc_ITEM","call_id":"call_CORRELATION","name":"get_weather","arguments":"{\"city\":\"Berlin\"}","status":"completed"}}`,
		completed(5, 2),
	)

	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{
		Model: "gpt-5.6",
		Tools: []ToolDef{{
			Name:        "get_weather",
			Description: "Look up the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, calls, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %+v", calls)
	}
	if calls[0].ID != "call_CORRELATION" {
		t.Fatalf("the wrong id was kept (%q): a result keyed by it will be refused", calls[0].ID)
	}
	if calls[0].Name != "get_weather" {
		t.Fatalf("tool identity lost: %+v", calls[0])
	}
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("arguments are not valid JSON (%q): %v", calls[0].Args, err)
	}
	if args.City != "Berlin" {
		t.Fatalf("arguments not carried: %q", calls[0].Args)
	}
}

func TestTwoToolCallsKeepTheirOrderAndTheirIdentities(t *testing.T) {
	// The loop dedupes on id and returns results in call order, so a swap or a
	// collision here is a tool answering with another tool's result.
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.output_item.done","output_index":0,"sequence_number":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"first","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"sequence_number":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"second","arguments":"{}","status":"completed"}}`,
		completed(5, 2),
	)
	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{Model: "gpt-5.6"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, calls, _, _ := collect(t, events)
	if len(calls) != 2 {
		t.Fatalf("expected two calls, got %+v", calls)
	}
	if calls[0].ID != "call_a" || calls[1].ID != "call_b" {
		t.Fatalf("calls arrived out of order or with wrong ids: %+v", calls)
	}
	if calls[0].Name != "first" || calls[1].Name != "second" {
		t.Fatalf("names do not match their calls: %+v", calls)
	}
}

func TestRunningOutOfBudgetIsAFailureAndNotASilentEnd(t *testing.T) {
	// On this wire the cap covers THINKING as well as writing, so a reasoning
	// model can spend all of it before the first character. Reported as done,
	// that is a successful, silent, empty turn, and the person concludes the
	// model had nothing to say. That is the class the clients were fixed for.
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":2000,"total_tokens":2009}}}`,
	)
	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{
		Model: "gpt-5.6", MaxTokens: 2000, Reasoning: true,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	content, _, _, _, errs := collect(t, events)
	if content != "" {
		t.Fatalf("nothing was written, yet content arrived: %q", content)
	}
	if len(errs) == 0 {
		t.Fatal("a turn that produced nothing ended as a success")
	}
	if !strings.Contains(errs[0].Error(), "max_output_tokens") {
		t.Errorf("the reason it stopped was not reported: %v", errs[0])
	}
}

func TestAFailedResponseAndABareErrorBothSurface(t *testing.T) {
	// The failure event on this wire is typed bare `error`, NOT
	// `response.error`. A dispatcher filtering on the prefix drops exactly the
	// event that must never be dropped, and the turn hangs instead of failing.
	cases := []struct {
		name, event, want string
	}{
		{
			name:  "the response failed",
			event: `{"type":"response.failed","sequence_number":1,"response":{"id":"r","object":"response","created_at":1,"model":"m","status":"failed","output":[],"error":{"code":"server_error","message":"the model gave up"}}}`,
			want:  "the model gave up",
		},
		{
			name:  "a bare error",
			event: `{"type":"error","sequence_number":1,"code":"rate_limit","message":"slow down","param":null}`,
			want:  "slow down",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got capturedRequest
			srv := responsesServer(t, &got, c.event)
			events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{Model: "gpt-5.6"})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			_, _, _, _, errs := collect(t, events)
			if len(errs) != 1 {
				t.Fatalf("expected exactly one error, got %v", errs)
			}
			if !strings.Contains(errs[0].Error(), c.want) {
				t.Errorf("the vendor's own words were lost: %v", errs[0])
			}
		})
	}
}

func TestARefusalIsShownRatherThanSwallowed(t *testing.T) {
	// A model declining is something the person must see. Dropped, the turn
	// reads as one that produced nothing at all.
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.refusal.done","item_id":"msg_1","output_index":0,"content_index":0,"sequence_number":1,"refusal":"I cannot help with that."}`,
		completed(3, 1),
	)
	events, err := mustResponses(t, srv.URL).Stream(context.Background(), GenerateRequest{Model: "gpt-5.6"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	content, _, _, _, errs := collect(t, events)
	if len(errs) > 0 {
		t.Fatalf("a refusal is not a transport failure: %v", errs)
	}
	if !strings.Contains(content, "cannot help") {
		t.Errorf("the refusal never reached the person: %q", content)
	}
}

func TestCancellationEndsTheStreamWithoutAnError(t *testing.T) {
	var got capturedRequest
	srv := responsesServer(t, &got,
		`{"type":"response.output_text.delta","delta":"partial","item_id":"m","output_index":0,"content_index":0,"sequence_number":1}`,
		completed(1, 1),
	)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := mustResponses(t, srv.URL).Stream(ctx, GenerateRequest{Model: "gpt-5.6"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cancel()
	for range events { //nolint:revive // draining is the point
	}
	// The channel closed, which is all that is asserted: a caller that went
	// away must not be told it made a mistake.
}

// --- what goes onto the wire -------------------------------------------------

// sent runs one request against a server that answers immediately, and hands
// back the body it received.
func sent(t *testing.T, req GenerateRequest) map[string]any {
	t.Helper()
	var got capturedRequest
	srv := responsesServer(t, &got, completed(1, 1))
	events, err := mustResponses(t, srv.URL).Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	collect(t, events)
	return got.body
}

func TestNoChatCompletionsVocabularyReachesTheWire(t *testing.T) {
	// The classic porting bug, so this asserts ABSENCE. Every one of these is
	// a field the old wire uses for the same job, and a body carrying one is a
	// body that was built by the wrong half of somebody's memory.
	body := sent(t, GenerateRequest{
		Model:     "gpt-5.6",
		MaxTokens: 100,
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "t", Args: json.RawMessage(`{}`)}}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "done"},
		},
		Tools: []ToolDef{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})

	for _, banned := range []string{"messages", "max_tokens", "max_completion_tokens", "stream_options"} {
		if _, present := body[banned]; present {
			t.Errorf("%q belongs to the chat wire and was sent here", banned)
		}
	}
	if _, ok := body["input"]; !ok {
		t.Fatal("the conversation was not sent as `input`")
	}
	if _, ok := body["max_output_tokens"]; !ok {
		t.Error("the token cap was not sent under its name on this wire")
	}

	// A tool is declared flat here, with no nested "function" object.
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not sent: %+v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if _, nested := tool["function"]; nested {
		t.Error("the tool was declared in the chat wire's nested shape")
	}
	if tool["name"] != "t" {
		t.Errorf("the tool's name is not where this wire puts it: %+v", tool)
	}

	// And nothing anywhere may carry a tool_call_id or a "tool" role.
	raw, _ := json.Marshal(body)
	for _, banned := range []string{`"tool_call_id"`, `"role":"tool"`} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("%s is chat wire vocabulary and reached this one: %s", banned, raw)
		}
	}
}

func TestAToolResultGoesBackAsItsOwnItemKeyedByTheCall(t *testing.T) {
	body := sent(t, GenerateRequest{
		Model: "gpt-5.6",
		Messages: []Message{
			{Role: RoleUser, Content: "weather?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_abc", Name: "get_weather", Args: json.RawMessage(`{"city":"Berlin"}`)},
			}},
			{Role: RoleTool, ToolCallID: "call_abc", Content: "17 degrees"},
		},
	})

	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input is not a list: %+v", body["input"])
	}
	var call, output map[string]any
	for _, item := range input {
		m, _ := item.(map[string]any)
		switch m["type"] {
		case "function_call":
			call = m
		case "function_call_output":
			output = m
		}
	}
	if call == nil {
		t.Fatal("the assistant's tool call did not become an item")
	}
	if output == nil {
		t.Fatal("the tool result did not become an item of its own")
	}
	if call["call_id"] != "call_abc" || output["call_id"] != "call_abc" {
		t.Fatalf("the call and its result are not paired: %+v / %+v", call, output)
	}
	if output["output"] != "17 degrees" {
		t.Errorf("the result's content was lost: %+v", output)
	}
}

func TestThinkingIsAskedForOnlyWhenItWasAskedFor(t *testing.T) {
	// Omission is the one behaviour every model on this wire understands. The
	// vendors that need "off" said out loud are on the other wire and default
	// thinking ON; these models reject the object outright unless they reason,
	// and the ones that accept it disagree about which values are legal. So the
	// field is present or absent, and never a value guessed on a model's behalf.
	off := sent(t, GenerateRequest{Model: "gpt-5.6", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if _, present := off["reasoning"]; present {
		t.Error("thinking was requested for an agent that did not ask for it")
	}

	on := sent(t, GenerateRequest{
		Model: "gpt-5.6", Reasoning: true,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	reasoning, ok := on["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("thinking was asked for and not requested: %+v", on["reasoning"])
	}
	// The DECLARED default, not a constant: nothing was configured here, so the
	// dialect's answer is what should go on the wire.
	if want := reasoningEffortSetting.Default; reasoning["effort"] != want {
		t.Errorf("effort is %v, want the declared default %v", reasoning["effort"], want)
	}
}

func TestTheEffortIsWhatWasChosen(t *testing.T) {
	// The point of declaring it: a model or an agent can say otherwise, and it
	// reaches the wire. It was a constant applied to every call this vendor
	// took, so a one-line classification thought as hard as a research
	// question.
	on := sent(t, GenerateRequest{
		Model:     "gpt-5.6",
		Reasoning: true,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		Settings:  model.Settings{"reasoning_effort": "low"},
	})
	reasoning, ok := on["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("thinking was asked for and not requested: %+v", on["reasoning"])
	}
	if reasoning["effort"] != "low" {
		t.Errorf("the chosen effort did not reach the wire: %v", reasoning["effort"])
	}

	// And a value the dialect does not offer is not sent: a vendor refuses the
	// whole request over one bad field, so the default answers instead.
	off := sent(t, GenerateRequest{
		Model:     "gpt-5.6",
		Reasoning: true,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		Settings:  model.Settings{"reasoning_effort": "ludicrous"},
	})
	withdrawn, _ := off["reasoning"].(map[string]any)
	if withdrawn["effort"] != reasoningEffortSetting.Default {
		t.Errorf("an undeclared value reached the wire: %v", withdrawn["effort"])
	}
}

func TestNothingIsKeptOnTheVendorsSide(t *testing.T) {
	// Our transcript is the truth (KB/16): a turn is rebuilt from rows every
	// time and park-and-resume depends on it. Storing server side would put a
	// customer's conversation in somebody else's retention window for no gain.
	body := sent(t, GenerateRequest{Model: "gpt-5.6", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	store, present := body["store"]
	if !present {
		t.Fatal("store was not sent, so the vendor's default decides")
	}
	if store != false {
		t.Errorf("store is %v, want false", store)
	}
	for _, banned := range []string{"previous_response_id", "conversation"} {
		if _, present := body[banned]; present {
			t.Errorf("%q chains the conversation on the vendor's side", banned)
		}
	}
}

func TestASystemMessageKeepsItsPlaceInTheConversation(t *testing.T) {
	// Rather than being hoisted into `instructions`. The trimmer's contract is
	// that a system message stays where it sits, and a second way of expressing
	// one is a second thing to keep in step with it.
	body := sent(t, GenerateRequest{
		Model: "gpt-5.6",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "hi"},
		},
	})
	if _, present := body["instructions"]; present {
		t.Error("the system message was hoisted out of the conversation")
	}
	input, _ := body["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("expected the system message and the question, got %+v", input)
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("the system message did not come first: %+v", first)
	}
}

func TestAnEmptyAssistantTurnIsLeftOutEntirely(t *testing.T) {
	// It said nothing and called nothing. Some vendors reject an empty item and
	// none needs one.
	body := sent(t, GenerateRequest{
		Model: "gpt-5.6",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant},
			{Role: RoleUser, Content: "still there?"},
		},
	})
	input, _ := body["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("an empty assistant turn reached the wire: %+v", input)
	}
}

func TestFilesGoDownTheChannelThatMatchesWhatTheyAre(t *testing.T) {
	body := sent(t, GenerateRequest{
		Model: "gpt-5.6",
		Messages: []Message{{
			Role: RoleUser, Content: "what are these?",
			Files: []FilePart{
				{FileName: "shot.png", MediaType: "image/png", Data: []byte{1, 2, 3}},
				{FileName: "deal.pdf", MediaType: "application/pdf", Data: []byte{4, 5}},
				{FileName: "notes.txt", MediaType: "text/plain", Data: []byte("the file's own words")},
			},
		}},
	})
	raw, _ := json.Marshal(body)
	for _, want := range []string{`"input_image"`, `"input_file"`, `"input_text"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("no %s part was sent: %s", want, raw)
		}
	}
	// A text file's bytes ARE its content: it must arrive readable, not base64.
	if !strings.Contains(string(raw), "the file's own words") {
		t.Errorf("a text file was not sent as text: %s", raw)
	}
}

func TestAFileThisWireCannotCarryIsRefusedBeforeItIsSent(t *testing.T) {
	_, err := mustResponses(t, "http://127.0.0.1:1").Stream(context.Background(), GenerateRequest{
		Model: "gpt-5.6",
		Messages: []Message{{
			Role: RoleUser,
			Files: []FilePart{{
				FileName: "notes.docx", Data: []byte{1},
				MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			}},
		}},
	})
	if err == nil {
		t.Fatal("a file this model cannot read was sent anyway")
	}
	if !errors.Is(err, ErrUnsupportedFile) {
		t.Errorf("a refusal callers can branch on was expected, got %v", err)
	}
}

// --- the non-streaming path --------------------------------------------------

func TestGenerateReadsEveryKindOfOutputItem(t *testing.T) {
	// Generate is not decoration: titles, memory distillation and file reading
	// all run on it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"completed",
			"output":[
			  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thought about it"}]},
			  {"type":"message","id":"msg_1","role":"assistant","status":"completed",
			   "content":[{"type":"output_text","text":"the answer","annotations":[]}]},
			  {"type":"function_call","id":"fc_1","call_id":"call_x","name":"t","arguments":"{\"a\":1}","status":"completed"}
			],
			"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}))
	t.Cleanup(srv.Close)

	resp, err := mustResponses(t, srv.URL).Generate(context.Background(), GenerateRequest{Model: "gpt-5.6"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.Message.Content != "the answer" {
		t.Errorf("content: %q", resp.Message.Content)
	}
	if resp.Message.Reasoning != "thought about it" {
		t.Errorf("reasoning: %q", resp.Message.Reasoning)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call_x" {
		t.Errorf("tool calls: %+v", resp.Message.ToolCalls)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestGenerateRefusesAnAnswerThatIsNotThere(t *testing.T) {
	// The floor the chat adapter has. Without it an empty answer becomes a
	// blank title and a silently skipped memory rewrite, and nothing says why.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"response","created_at":1,"model":"m",
			"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	if _, err := mustResponses(t, srv.URL).Generate(context.Background(), GenerateRequest{Model: "gpt-5.6"}); err == nil {
		t.Fatal("a response with no output was accepted as an answer")
	}
}
