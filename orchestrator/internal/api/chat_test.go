package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// The chat suite drives the real endpoint, the real agent loop, the real tool
// registry and the real database. Only the vendor is faked, by pointing an
// openai-compatible vendor row at a local server that speaks the wire format.
// Everything between the HTTP request and the SQL is the code that ships.

// fakeVendor is a stand-in model server. Each entry in replies is one turn's
// worth of SSE chunks, handed out in order, so a test can script a tool call
// followed by an answer.
type fakeVendor struct {
	t       *testing.T
	server  *httptest.Server
	replies [][]string

	// The background naming call arrives on its own goroutine, at a moment
	// the test does not control, so what it records is guarded.
	mu      sync.Mutex
	turn    int
	prompts []map[string]any
	// status, when >= 400, makes the fake answer with that HTTP error instead
	// of a completion, standing in for a vendor that is down or overloaded.
	status int
	// failFirst makes only the FIRST call fail, which is what a blip looks
	// like: the stream breaks having said nothing, and asking again works.
	// `status` cannot express that, because it applies to every call.
	failFirst bool
}

func newFakeVendor(t *testing.T, replies ...[]string) *fakeVendor {
	t.Helper()
	f := &fakeVendor{t: t, replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode vendor request: %v", err)
			return
		}
		// A conversation is named in the background, from the same model that
		// answered it, so a turn produces one more model call than the test
		// scripted. That call is not what these tests are about, and it
		// answers with a title.
		f.mu.Lock()
		f.prompts = append(f.prompts, body)
		if f.status >= 400 {
			f.mu.Unlock()
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		if f.failFirst {
			f.failFirst = false
			f.mu.Unlock()
			// MID-STREAM, not a status code. An HTTP error before the body is
			// something the vendor SDK retries by itself, so it never reaches
			// our loop and a test built on it proves nothing (it passed with the
			// bug restored). A connection that dies after the stream has started
			// cannot be retried down there: the bytes are already delivered, and
			// the break surfaces as a stream error, which is the case the loop's
			// own retry exists for.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
				flusher.Flush()
			}
			panic(http.ErrAbortHandler) // closes the connection without a reply
		}
		var chunks []string
		if f.turn < len(f.replies) {
			chunks = f.replies[f.turn]
		} else {
			chunks = []string{
				`{"choices":[{"index":0,"delta":{"content":"A conversation"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
		}
		f.turn++
		f.mu.Unlock()

		// A real vendor answers a one-shot request with JSON and a streamed
		// one with SSE. The naming call is one-shot, so the fake has to be
		// both, or it is not standing in for a vendor at all.
		if streaming, _ := body["stream"].(bool); !streaming {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(completionJSON(t, chunks)))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("cannot flush")
			return
		}
		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(f.server.Close)
	return f
}

// setReply replaces a scripted reply before it is served, so a test can inject a
// value it only learns at runtime (like a delegation's id, needed to cancel it by
// id). Safe to call between turns, while the vendor is idle.
func (f *fakeVendor) setReply(i int, reply []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[i] = reply
}

// callCount reports how many times the model was asked anything. Zero proves a
// turn never reached the vendor.
func (f *fakeVendor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// turnAsking finds the request that carried a given user message.
//
// Counting requests would be a lie: a conversation is named in the background,
// from the same model, so a naming call can land between two turns and shift
// every index after it. A test names the turn it means.
func (f *fakeVendor) turnAsking(prompt string) map[string]any {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, body := range f.prompts {
		messages, _ := body["messages"].([]any)
		for _, item := range messages {
			m, _ := item.(map[string]any)
			if m["role"] == "user" && m["content"] == prompt {
				return body
			}
		}
	}
	f.t.Fatalf("the model was never asked %q", prompt)
	return nil
}

// sentToolsAsking reports the tools the model was handed on the turn that
// carried a given user message.
func (f *fakeVendor) sentToolsAsking(prompt string) []string {
	f.t.Helper()
	return toolNames(f.turnAsking(prompt))
}

// sentSystemPromptAsking reports the instructions the model was given on the
// turn that carried a given user message.
func (f *fakeVendor) sentSystemPromptAsking(prompt string) string {
	f.t.Helper()
	messages, _ := f.turnAsking(prompt)["messages"].([]any)
	for _, item := range messages {
		m, _ := item.(map[string]any)
		if m["role"] == "system" {
			content, _ := m["content"].(string)
			return content
		}
	}
	return ""
}

func toolNames(body map[string]any) []string {
	raw, ok := body["tools"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		fn, _ := entry["function"].(map[string]any)
		if name, ok := fn["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// registerModel points a vendor row at the fake server and returns the model
// id the chat endpoint will be told to use.
func (e *testEnv) registerModel(f *fakeVendor) int64 {
	e.t.Helper()
	ctx := context.Background()

	vendor := &model.AIVendor{
		WorkspaceID: e.ws.ID,
		VendorKey:   model.VendorOpenAICompatible,
		Name:        "Fake",
		BaseURL:     f.server.URL,
	}
	if err := e.app.Store.Vendors().Create(ctx, vendor); err != nil {
		e.t.Fatalf("create vendor: %v", err)
	}
	m := &model.AIModel{
		WorkspaceID: e.ws.ID, VendorID: vendor.ID, ModelKey: "fake-1",
		Type: model.ModelTypeChat, ContextWindow: 100_000,
	}
	if err := e.app.Store.AIModels().Create(ctx, m); err != nil {
		e.t.Fatalf("create model: %v", err)
	}
	return m.ID
}

// streamTurn posts a turn and parses the SSE frames that come back.
func (e *testEnv) streamTurn(token string, body map[string]any) []chat.Frame {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/stream", token, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("stream failed: %d %s", rec.Code, rec.Body.String())
	}
	return parseFrames(e.t, rec.Body.String())
}

func parseFrames(t *testing.T, raw string) []chat.Frame {
	t.Helper()
	frames := []chat.Frame{}
	for _, block := range strings.Split(raw, "\n\n") {
		line := strings.TrimPrefix(strings.TrimSpace(block), "data: ")
		if line == "" {
			continue
		}
		var frame chat.Frame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("undecodable frame %q: %v", line, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func framesOfType(frames []chat.Frame, kind chat.FrameType) []chat.Frame {
	out := []chat.Frame{}
	for _, f := range frames {
		if f.Type == kind {
			out = append(out, f)
		}
	}
	return out
}

// deltaText joins the streamed answer, which is what a user actually sees.
func deltaText(frames []chat.Frame) string {
	var b strings.Builder
	for _, f := range framesOfType(frames, chat.FrameDelta) {
		if text, ok := f.Message.(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

func toolMessage(t *testing.T, frame chat.Frame) chat.ToolMessage {
	t.Helper()
	raw, err := json.Marshal(frame.Message)
	if err != nil {
		t.Fatalf("re-encode tool frame: %v", err)
	}
	var msg chat.ToolMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode tool frame: %v", err)
	}
	return msg
}

func confirmRequest(t *testing.T, frame chat.Frame) chat.ConfirmRequest {
	t.Helper()
	raw, err := json.Marshal(frame.Message)
	if err != nil {
		t.Fatalf("re-encode confirm frame: %v", err)
	}
	var msg chat.ConfirmRequest
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode confirm frame: %v", err)
	}
	return msg
}

// --- a plain answer -----------------------------------------------------------

func TestChatStreamsAnAnswer(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":", world"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	})
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{
		"prompt": "hi", "model_id": modelID,
	})

	// A new conversation announces its id first, so a retry continues it
	// rather than starting a second one.
	created := framesOfType(frames, chat.FrameChatCreated)
	if len(created) != 1 || created[0].ChatID == "" {
		t.Fatalf("no chat id was announced: %+v", frames)
	}
	if deltaText(frames) != "Hello, world" {
		t.Fatalf("answer was not streamed: %q", deltaText(frames))
	}

	// The last frame is always terminal, and carries the whole answer so a
	// client that missed deltas can still render the turn.
	last := frames[len(frames)-1]
	if last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the stream did not end with a final result: %+v", last)
	}
	if last.Message != "Hello, world" {
		t.Fatalf("result did not carry the answer: %+v", last.Message)
	}

	// The turn is persisted: the transcript holds the question and the answer.
	ctx := context.Background()
	sessionID := env.sessionOf(frames).ID

	steps, err := env.app.Store.Agent().Transcript(ctx, sessionID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 2 || steps[0].Kind != model.StepUser || steps[1].Kind != model.StepAssistant {
		t.Fatalf("transcript not persisted: %+v", steps)
	}
	if steps[1].Text != "Hello, world" {
		t.Fatalf("the answer was not stored: %q", steps[1].Text)
	}
	if steps[1].Partial {
		t.Fatal("a finished answer is still marked partial")
	}

	session, err := env.app.Store.Agent().GetSession(ctx, env.ws.ID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.Status != model.SessionCompleted {
		t.Fatalf("session was left in %q", session.Status)
	}
}

// The loop: the model asks for a tool, we run it, feed the result back, and
// the model answers using it. This is the whole product in one test.
func TestChatRunsAToolAndFeedsTheResultBack(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t,
		// Turn one: call current_time.
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"current_time","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		},
		// Turn two: answer, having seen the result.
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"It is Sunday."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{
		"prompt": "what day is it?", "model_id": modelID,
	})

	// The UI is told what is happening before the tool runs, and what came of
	// it afterwards: two frames per tool, deliberately.
	preparing := framesOfType(frames, chat.FrameToolPreparing)
	if len(preparing) != 1 {
		t.Fatalf("the tool was not announced before it ran: %+v", frames)
	}
	if msg := toolMessage(t, preparing[0]); msg.Name != "current_time" || msg.FriendlyName == "" {
		t.Fatalf("preparing frame is not usable by a UI: %+v", msg)
	}

	done := framesOfType(frames, chat.FrameTool)
	if len(done) != 1 {
		t.Fatalf("the tool result was not reported: %+v", frames)
	}
	msg := toolMessage(t, done[0])
	if !msg.Done || msg.Status != model.ToolCallCompleted {
		t.Fatalf("tool frame does not report completion: %+v", msg)
	}
	if !bytes.Contains(msg.Output, []byte("utc")) {
		t.Fatalf("the tool output was not passed to the client: %s", msg.Output)
	}

	if deltaText(frames) != "It is Sunday." {
		t.Fatalf("the model did not answer after the tool: %q", deltaText(frames))
	}

	// The second model call must have carried the tool result, or the model
	// was answering blind.
	if len(vendor.prompts) != 2 {
		t.Fatalf("expected two model calls, got %d", len(vendor.prompts))
	}
	second, err := json.Marshal(vendor.prompts[1]["messages"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(second, []byte(`"role":"tool"`)) {
		t.Fatalf("the tool result was not fed back to the model: %s", second)
	}

	// And the call is on the record, with a duration.
	calls := env.toolCalls(env.sessionOf(frames).ID)
	if len(calls) != 1 || calls[0].ToolName != "current_time" ||
		calls[0].Status != model.ToolCallCompleted {
		t.Fatalf("tool call not recorded: %+v", calls)
	}
}

// A model that cannot call tools must not be offered any: the registry says
// so, and the gateway acts on it.
func TestChatRejectsUnknownModelAndUnauthenticatedCallers(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	// A configured main agent, so the turn reaches the model check rather than
	// the "no assistant set up" notice.
	env.GatewayAgent()

	env.expectStatus(env.do(http.MethodPost, "/v1/chat/stream", "", map[string]any{
		"prompt": "hi", "model_id": 1,
	}), http.StatusUnauthorized)

	env.expectStatus(env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"model_id": 1,
	}), http.StatusBadRequest)

	// An unknown model is a stream that ends in an error frame, not a 500:
	// the turn opened, so the failure has to be reported in the protocol.
	rec := env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"prompt": "hi", "model_id": 999999,
	})
	frames := parseFrames(t, rec.Body.String())
	last := frames[len(frames)-1]
	if last.Type != chat.FrameError || !last.Final {
		t.Fatalf("an unusable model must end the stream with an error: %+v", frames)
	}
}

// A workspace answers only through a configured assistant. With a model but no
// main agent and no workflow, the chat does not fall back to a working default:
// it tells the person, in the chat, to set one up, and never calls the model.
func TestChatRequiresAConfiguredMainAgent(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"should never run"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	// Deliberately no GatewayAgent(): the workspace has a model but no assistant.

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	last := frames[len(frames)-1]
	msg, _ := last.Message.(string)
	if last.Type != chat.FrameResult || !last.Final || msg != noMainAgentNotice {
		t.Fatalf("expected the no-main-agent notice, got: %+v", frames)
	}
	if n := vendor.callCount(); n != 0 {
		t.Fatalf("the model was called with no assistant configured: %d calls", n)
	}
}

// A main agent configured with NO tools reaches the model with no tools. An
// empty selection is honoured, never read as "inherit the code defaults" (the
// bug where a zero-tool agent still called list_models, http_request, and the
// rest). Not even tool_guide or the memory tool ride along: there is nothing to
// guide and nothing to reach.
func TestAToollessMainAgentGetsNoTools(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"Hello."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	if err := env.app.Store.Agents().Create(context.Background(), &model.Agent{
		WorkspaceID: env.ws.ID, Key: model.DefaultAgentKey, Name: "Talker", Tools: []string{},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})

	if sent := vendor.sentToolsAsking("hi"); len(sent) != 0 {
		t.Fatalf("a tool-less agent was handed tools: %v", sent)
	}
}

// A conversation's public id is opaque. A row id leaving the server would tell
// whoever holds one how many conversations exist and invite them to walk the
// range: authorization stops them reading someone else's, but they should not
// be able to ask the question at all.
func TestChatIDsAreOpaque(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	chatID := chatIDOf(t, frames)

	if !strings.HasPrefix(chatID, model.ChatUIDPrefix) {
		t.Fatalf("the chat id is not a public id: %q", chatID)
	}
	if _, err := strconv.ParseInt(chatID, 10, 64); err == nil {
		t.Fatalf("the chat id is a row id: %q", chatID)
	}

	// The row id is not addressable from outside, even by its owner.
	session := env.sessionOf(frames)
	rec := env.do(http.MethodPost, "/v1/chat/chats/delete/"+strconv.FormatInt(session.ID, 10), token, map[string]any{})
	env.expectStatus(rec, http.StatusNotFound)

	// The public id is.
	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/delete/"+chatID, token, map[string]any{}),
		http.StatusNoContent,
	)
}

// A new conversation names itself, from the same model that answered it: the
// same routing, the same credentials, the same gateway. It happens after the
// answer is sent, so the user never waits on it.
func TestConversationNamesItself(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t,
		// The turn.
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Rounding is a floating point problem."}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
		// The naming call. Models wrap titles in quotes and prefixes no matter
		// how plainly they are asked not to, so this one does too.
		[]string{
			`{"choices":[{"index":0,"delta":{"content":"Title: \"Invoice rounding errors.\""}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	)
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{
		"prompt": "why do invoice totals disagree?", "model_id": modelID,
	})
	session := env.sessionOf(frames)

	// The naming runs detached, so it is not finished when the answer is.
	deadline := time.Now().Add(5 * time.Second)
	var title string
	for time.Now().Before(deadline) {
		fresh, err := env.app.Store.Agent().GetSession(context.Background(), env.ws.ID, session.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if fresh.Title != "" {
			title = fresh.Title
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if title != "Invoice rounding errors" {
		t.Fatalf("the conversation was not named, or the title was not cleaned: %q", title)
	}
}

// completionJSON turns the scripted deltas into the non-streaming reply the
// same vendor would give: the fake answers both shapes, as a real one does.
func completionJSON(t *testing.T, chunks []string) string {
	t.Helper()

	var content strings.Builder
	for _, chunk := range chunks {
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(chunk), &frame); err != nil {
			t.Fatalf("scripted chunk is not valid JSON: %v", err)
		}
		for _, choice := range frame.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	payload, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": content.String()},
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	if err != nil {
		t.Fatalf("encode completion: %v", err)
	}
	return string(payload)
}

// What the chat is allowed to offer.
//
// The attach button and the microphone are a reading of the Gateway's
// configuration, never a preference: offering to send a file nothing can read
// is the interface making a promise the server will refuse. So the client asks,
// and this is the answer.
func TestTheChatIsToldWhatItMaySend(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	vendor := env.createVendorViaAPI(token, "Anthropic", secretAPIKey)

	newModel := func(key, kind string) int64 {
		t.Helper()
		rec := env.do(http.MethodPost, "/v1/models", token, map[string]any{
			"vendor_id": vendor.ID, "model_key": key, "type": kind, "context_window": 200000,
		})
		env.expectStatus(rec, http.StatusCreated)
		var m modelBody
		env.decode(rec, &m)
		return m.ID
	}
	chat := newModel("claude-sonnet-5", model.ModelTypeChat)
	transcriber := newModel("claude-haiku-4-5", model.ModelTypeSTT)

	// Read off the history answer, which is the one request a conversation makes
	// when it opens. It was an endpoint of its own, asked for beside this one on
	// every load, for a fact needed at exactly that moment and no other.
	accepts := func() chatAcceptsResponse {
		t.Helper()
		rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{})
		env.expectStatus(rec, http.StatusOK)
		var out historyResponse
		env.decode(rec, &out)
		return out.Meta.Accepts
	}

	// With no Gateway at all the chat can only be typed at, and that is an
	// answer rather than an error.
	got := accepts()
	if len(got.FileTypes) != 0 || got.AnyFile || got.Audio {
		t.Fatalf("nothing is configured, so nothing should be offered: %+v", got)
	}
	// The ceiling comes from here so the client can refuse a too-large file
	// before spending somebody's upstream on it, and so one number lives in one
	// place.
	if got.MaxBytes != maxUploadBytes {
		t.Fatalf("the size ceiling was not reported: %d", got.MaxBytes)
	}

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway", "model_id": chat,
		"file_rules": []map[string]any{{"types": []string{"pdf", "docx"}, "model_id": chat}},
	})
	env.expectStatus(rec, http.StatusCreated)
	var gateway agentBody
	env.decode(rec, &gateway)

	got = accepts()
	if len(got.FileTypes) != 2 || got.AnyFile {
		t.Fatalf("only the named types can be attached: %+v", got)
	}
	if got.Audio {
		t.Fatal("no audio model is set, so the microphone must not be offered")
	}

	// A catch-all means the picker should stop filtering.
	//
	// Written through the endpoints that OWN these, which is where the Gateway
	// screen writes them from: a dialog per question (KB/19). The agent form
	// does not send them and no longer gets to blank them, so driving them
	// through it would be testing a path nothing takes.
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/files", token, map[string]any{
		"rules": []map[string]any{
			{"types": []string{"pdf"}, "model_id": chat},
			{"types": []string{}, "model_id": chat},
		},
	}), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodPut, "/v1/gateway/audio", token,
		map[string]any{"model_id": transcriber}), http.StatusNoContent)

	got = accepts()
	if !got.AnyFile {
		t.Fatal("a catch-all rule means any file can be attached")
	}

	// The audio model here is on an Anthropic vendor, which has no
	// transcription endpoint at all. An administrator is not stopped from
	// choosing it, which is right: what they set up is their call. What they
	// could not know is that this vendor cannot be asked, so the chat is told
	// plainly rather than offering a microphone that could never work.
	if got.Audio {
		t.Error("a vendor that cannot transcribe must not turn the microphone on")
	}
	if !strings.Contains(got.AudioUnsupported, "does not transcribe") {
		t.Errorf("the reason should name what the vendor cannot do: %q", got.AudioUnsupported)
	}
	if got.FilesUnsupported != "" {
		t.Errorf("this vendor CAN be sent files: %q", got.FilesUnsupported)
	}
}

// The same question of a vendor that can transcribe.
//
// We do not restrict which model an administrator points at audio. What we
// report is the narrower fact of whether the vendor can be asked at all, and
// this is the side of it that says yes.
func TestAVendorThatTranscribesTurnsTheMicrophoneOn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/vendors", token, map[string]any{
		"vendor_key": model.VendorOpenAICompatible, "name": "Local",
		"credentials": secretAPIKey, "base_url": fakeVendorServer(t, []string{"whisper-1"}),
	})
	env.expectStatus(rec, http.StatusCreated)
	var vendor vendorBody
	env.decode(rec, &vendor)

	rec = env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "whisper-1", "type": model.ModelTypeSTT,
		"context_window": 8000,
	})
	env.expectStatus(rec, http.StatusCreated)
	var m modelBody
	env.decode(rec, &m)

	rec = env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway", "audio_model_id": m.ID,
	})
	env.expectStatus(rec, http.StatusCreated)

	rec = env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{})
	env.expectStatus(rec, http.StatusOK)
	var history historyResponse
	env.decode(rec, &history)
	got := history.Meta.Accepts
	if !got.Audio || got.AudioUnsupported != "" {
		t.Fatalf("a vendor that transcribes should turn the microphone on: %+v", got)
	}
}

// Deleting a conversation is a permission, and the surface says so before it is
// tried.
//
// The chat routes used to require a valid token and nothing else, so anybody who
// could sign in could destroy the record of what they had asked and what the
// assistant had been allowed to do about it, and no role could say otherwise.
func TestDeletingAConversationNeedsThePermission(t *testing.T) {
	env := newTestEnv(t)
	// Everything a person needs to HOLD a conversation, and not the one thing
	// this is about. Signing in is leave to talk; deleting is a decision.
	env.createUser("limited@acme.test", "dev-Passw0rd!", model.PermAgentsView)
	token, _ := env.login("limited@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/chats/create", token, map[string]any{"title": "Payroll"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	env.decode(rec, &created)

	// The list ANSWERS the question, so the sidebar never draws a bin that is
	// going to be refused.
	rec = env.do(http.MethodGet, "/v1/chat/chats", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var list chatListResponse
	env.decode(rec, &list)
	if list.CanDelete {
		t.Fatal("the list offered deleting to somebody who may not")
	}
	if len(list.Chats) != 1 {
		t.Fatalf("the conversation is theirs and should be listed: %+v", list.Chats)
	}

	// And the route refuses it, which is the part that actually protects the
	// record: a client is never what enforces this.
	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/delete/"+created.ID, token, map[string]any{}),
		http.StatusForbidden,
	)

	// Refused, and it is still there. A 403 that deleted anyway would be worse
	// than no permission at all.
	rec = env.do(http.MethodGet, "/v1/chat/chats", token, nil)
	env.decode(rec, &list)
	if len(list.Chats) != 1 {
		t.Fatalf("a refused delete took the conversation anyway: %+v", list.Chats)
	}

	// What they may still do, because none of it destroys anything: naming a
	// conversation and pinning it are that person's own housekeeping.
	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/update/"+created.ID, token, map[string]any{"title": "Payroll Q3"}),
		http.StatusNoContent,
	)
	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/update/"+created.ID, token, map[string]any{"is_pinned": true}),
		http.StatusNoContent,
	)
}

// With the permission, it works, and the list says so first.
func TestTheChatListSaysDeletingIsAllowed(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("allowed@acme.test", "dev-Passw0rd!", model.PermChatsDelete)
	token, _ := env.login("allowed@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/chats/create", token, map[string]any{"title": "Payroll"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	env.decode(rec, &created)

	rec = env.do(http.MethodGet, "/v1/chat/chats", token, nil)
	var list chatListResponse
	env.decode(rec, &list)
	if !list.CanDelete {
		t.Fatal("the list withheld deleting from somebody who holds the permission")
	}

	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/delete/"+created.ID, token, map[string]any{}),
		http.StatusNoContent,
	)
	rec = env.do(http.MethodGet, "/v1/chat/chats", token, nil)
	env.decode(rec, &list)
	if len(list.Chats) != 0 {
		t.Fatalf("the conversation survived a permitted delete: %+v", list.Chats)
	}
}

// The permission is answered LIVE, like every other one (KB/08). A role edited
// while somebody is sitting on the screen bites on their next request, not when
// their token happens to expire.
func TestLosingTheChatsPermissionBitesAtOnce(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermChatsDelete)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/chats/create", token, map[string]any{"title": "Payroll"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	env.decode(rec, &created)

	// The grant is taken away with the token still perfectly valid.
	ctx := context.Background()
	roles, err := env.app.Store.Roles().List(ctx, env.ws.ID)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, role := range roles {
		role.Permissions = []string{}
		if err := env.app.Store.Roles().Update(ctx, role); err != nil {
			t.Fatalf("update role: %v", err)
		}
	}

	rec = env.do(http.MethodGet, "/v1/chat/chats", token, nil)
	var list chatListResponse
	env.decode(rec, &list)
	if list.CanDelete {
		t.Fatal("the list still offers deleting after the grant was taken away")
	}
	env.expectStatus(
		env.do(http.MethodPost, "/v1/chat/chats/delete/"+created.ID, token, map[string]any{}),
		http.StatusForbidden,
	)
}

// The permission is in the catalogue, so a role can actually be given it. A key
// the API validates against but never offers is a permission nobody can grant.
func TestTheChatsPermissionCanBeGranted(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodGet, "/v1/roles", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var screen rolesBody
	env.decode(rec, &screen)
	found := false
	for _, p := range screen.Permissions {
		if p.Key == model.PermChatsDelete {
			found = true
			if p.Area == "" || p.Label == "" {
				t.Fatalf("the permission has no words a person can read: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("chats:delete is enforced but cannot be granted from the roles screen")
	}

	// And a role really takes it, rather than the grant being dropped as unknown.
	rec = env.do(http.MethodPost, "/v1/roles", token, map[string]any{
		"name": "Chat user", "permissions": []string{model.PermChatsDelete},
	})
	env.expectStatus(rec, http.StatusCreated)
	var role roleBody
	env.decode(rec, &role)
	if len(role.Permissions) != 1 || role.Permissions[0] != model.PermChatsDelete {
		t.Fatalf("the grant did not stick: %+v", role.Permissions)
	}
}

// The history says whether anything is still being answered, so the client does
// not have to ask.
//
// A turn outlives the page that asked for it (KB/17), so opening a conversation
// means rejoining whatever is in flight. The chat used to find that out by
// POSTing to /chat/attach after EVERY history load, which in the common case
// answered 204: a second round trip, on every conversation anybody opens, to be
// told that nothing is happening.
func TestTheHistorySaysWhetherAnythingIsStillAnswering(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	chatID := chatIDOf(t, frames)

	// The turn finished before the stream returned, so there is nothing to
	// rejoin, and that is what the answer says. Naming a finished run here would
	// have the client replay a turn that is already in the messages beside it,
	// painting it twice.
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var history historyResponse
	env.decode(rec, &history)
	if history.Meta.LiveRun != "" {
		t.Fatalf("a finished conversation claims a live run: %q", history.Meta.LiveRun)
	}
	if len(history.Messages) == 0 {
		t.Fatal("the turn is not in the history either, so nothing would render at all")
	}

	// And attaching agrees with it: the client asking anyway is told the same
	// thing, which is the round trip the flag exists to avoid.
	rec = env.do(http.MethodPost, "/v1/chat/attach", token, map[string]any{"chat_id": chatID})
	env.expectStatus(rec, http.StatusNoContent)
}

// A conversation that has never run anything says so too, rather than leaving
// the field out and the client guessing.
func TestAFreshConversationHasNothingToRejoin(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/chat/chats/create", token, map[string]any{"title": "Empty"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	env.decode(rec, &created)

	rec = env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": created.ID})
	var history historyResponse
	env.decode(rec, &history)
	if history.Meta.LiveRun != "" {
		t.Fatalf("an empty conversation claims a live run: %q", history.Meta.LiveRun)
	}
	// A draft still has a composer, so it still has to be told what may be sent.
	if history.Meta.Accepts.MaxBytes != maxUploadBytes {
		t.Fatalf("a fresh conversation was not told what it may send: %+v", history.Meta.Accepts)
	}
}

func TestAStreamThatBrokeBeforeSayingAnythingIsRetriedAndTheAnswerKept(t *testing.T) {
	// A retry that WORKS must not be thrown away. settleStreamBreak retries,
	// writes the retried step back through pointer parameters and returns nil,
	// and the caller returned answer{} with that nil: the retried step was never
	// committed, its tool calls never ran, and the turn ended empty with nothing
	// logged as a failure, while the person watched the retried text stream in
	// and then stop.
	//
	// It got much easier to reach when OpenAI's own wire arrived: "spoken"
	// counts content and tool calls but not reasoning, and every reasoning call
	// now asks for maximum effort, so there is a long opening stretch where a
	// break is judged retryable.
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, answerChunks("The vault is secure."))
	vendor.failFirst = true // the first call breaks having said nothing
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "is the vault secure", "model_id": modelID})

	if got := deltaText(frames); !strings.Contains(got, "The vault is secure") {
		t.Fatalf("the retried answer never reached the person: %q", got)
	}
	for _, f := range frames {
		if f.Type == chat.FrameError {
			t.Errorf("a successful retry was reported as an error: %+v", f)
		}
	}

	// And it is in the transcript, not just on the wire: a turn that ends empty
	// leaves a partial row and nothing to reload.
	session := env.sessionOf(frames)
	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	var kept bool
	for _, s := range steps {
		if strings.Contains(s.Text, "The vault is secure") {
			kept = true
		}
	}
	if !kept {
		t.Error("the retried step was never written to the transcript")
	}
}
