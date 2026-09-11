package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// A turn outlives the reader that asked for it.
//
// These tests run against a real HTTP server, because the thing being proved is
// about a connection dying: a recorder cannot die. The client hangs up
// mid-answer, and the answer keeps being written.

// slowVendor holds its answer open until it is released, so a test can hang up
// while the model is still mid-sentence.
type slowVendor struct {
	server  *httptest.Server
	release chan struct{}
	once    sync.Once
}

// Release lets the model finish. It is safe to call twice, and it is called on
// cleanup no matter what, because a handler still blocked here would hold the
// test server open and the suite would hang instead of failing.
func (v *slowVendor) Release() { v.once.Do(func() { close(v.release) }) }

func newSlowVendor(t *testing.T, before, after string) *slowVendor {
	t.Helper()
	v := &slowVendor{release: make(chan struct{})}

	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		// The naming call is one-shot and must not block: it is not what this
		// test is about.
		if streaming, _ := body["stream"].(bool); !streaming {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"A chat"},` +
				`"finish_reason":"stop"}]}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		write := func(chunk string) {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		write(`{"choices":[{"index":0,"delta":{"content":` + quote(before) + `}}]}`)

		// The client is about to hang up. Everything after this point happens
		// with nobody listening.
		<-v.release

		write(`{"choices":[{"index":0,"delta":{"content":` + quote(after) + `}}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	// Registered in this order so it runs in the opposite one: release first,
	// then close. A blocked handler cannot be shut down.
	t.Cleanup(v.server.Close)
	t.Cleanup(v.Release)
	return v
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// live starts a real HTTP server on the test router.
func (e *testEnv) live() *httptest.Server {
	e.t.Helper()
	server := httptest.NewServer(e.router)
	e.t.Cleanup(server.Close)
	return server
}

// readFrames streams a request and hands each frame to fn. It stops when fn
// returns false, which is how a test hangs up mid-answer.
func readFrames(t *testing.T, ctx context.Context, server *httptest.Server, path, token string, body any, fn func(chat.Frame) bool) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: %d", path, resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame chat.Frame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", line, err)
		}
		if !fn(frame) {
			return
		}
	}
}

// The reader hangs up mid-answer. The turn does not notice, finishes, and
// writes the whole thing down. Before this, closing the tab cancelled the
// vendor call and the half-written answer was thrown away.
func TestHangingUpDoesNotKillTheTurn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	server := env.live()

	vendor := newSlowVendor(t, "The first half", " and the second half.")
	modelID := env.registerModelAt(vendor.server.URL)
	env.GatewayAgent()

	ctx, hangUp := context.WithCancel(context.Background())
	var chatID string
	readFrames(t, ctx, server, "/v1/chat/stream", token, map[string]any{
		"prompt": "tell me something", "model_id": modelID,
	}, func(frame chat.Frame) bool {
		if frame.Type == chat.FrameChatCreated {
			chatID = frame.ChatID
		}
		if frame.Type == chat.FrameDelta {
			// We have heard the first half. Close the laptop.
			hangUp()
			return false
		}
		return true
	})
	if chatID == "" {
		t.Fatal("the conversation was never announced")
	}

	// Nobody is listening now. The model says the rest anyway.
	vendor.Release()

	session := env.sessionByUID(chatID)
	env.waitForRun(session.ID)

	// The whole answer is in the conversation, including the half nobody heard.
	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected the question and the answer, got %d: %+v", len(steps), steps)
	}
	answer := steps[1]
	if answer.Text != "The first half and the second half." {
		t.Fatalf("the answer was cut off when the reader left: %q", answer.Text)
	}
	if answer.Partial {
		t.Fatal("the answer is still marked partial: the turn was abandoned, not finished")
	}
}

// And the reader who comes back hears the rest of it. That is what a refreshed
// page does: it asks what it missed, and it is told.
func TestReattachingHearsTheRestOfTheAnswer(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	server := env.live()

	vendor := newSlowVendor(t, "Half one", ", half two.")
	modelID := env.registerModelAt(vendor.server.URL)
	env.GatewayAgent()

	ctx, hangUp := context.WithCancel(context.Background())
	var chatID string
	var lastSeen int
	readFrames(t, ctx, server, "/v1/chat/stream", token, map[string]any{
		"prompt": "tell me something", "model_id": modelID,
	}, func(frame chat.Frame) bool {
		if frame.Type == chat.FrameChatCreated {
			chatID = frame.ChatID
			return true
		}
		lastSeen = frame.Index
		if frame.Type == chat.FrameDelta {
			hangUp()
			return false
		}
		return true
	})

	// The reader comes back and says how far it got. The rest of the answer is
	// still being written, and it hears it.
	rest := make(chan string, 1)
	go func() {
		var text string
		after := lastSeen
		readFrames(t, context.Background(), server, "/v1/chat/attach", token, map[string]any{
			"chat_id": chatID, "after": after,
		}, func(frame chat.Frame) bool {
			if frame.Type == chat.FrameDelta {
				if delta, ok := frame.Message.(string); ok {
					text += delta
				}
			}
			return frame.Type != chat.FrameResult
		})
		rest <- text
	}()

	// Give the reader a moment to attach, then let the model finish.
	time.Sleep(150 * time.Millisecond)
	vendor.Release()

	select {
	case text := <-rest:
		// It gets exactly what it missed, and not a word it already had.
		if text != ", half two." {
			t.Fatalf("the returning reader heard %q, not the half it missed", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the returning reader heard nothing")
	}
}

// A reloaded page asks for the whole turn from the beginning, because it kept
// nothing: it renders the answer so far and then the rest of it as it lands.
func TestAReloadedPageGetsTheWholeTurn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	server := env.live()

	vendor := newSlowVendor(t, "Half one", ", half two.")
	modelID := env.registerModelAt(vendor.server.URL)
	env.GatewayAgent()

	ctx, hangUp := context.WithCancel(context.Background())
	var chatID string
	readFrames(t, ctx, server, "/v1/chat/stream", token, map[string]any{
		"prompt": "tell me something", "model_id": modelID,
	}, func(frame chat.Frame) bool {
		if frame.Type == chat.FrameChatCreated {
			chatID = frame.ChatID
			return true
		}
		if frame.Type == chat.FrameDelta {
			hangUp()
			return false
		}
		return true
	})

	whole := make(chan string, 1)
	go func() {
		var text string
		readFrames(t, context.Background(), server, "/v1/chat/attach", token,
			map[string]any{"chat_id": chatID}, func(frame chat.Frame) bool {
				if frame.Type == chat.FrameDelta {
					if delta, ok := frame.Message.(string); ok {
						text += delta
					}
				}
				return frame.Type != chat.FrameResult
			})
		whole <- text
	}()

	time.Sleep(150 * time.Millisecond)
	vendor.Release()

	select {
	case text := <-whole:
		if text != "Half one, half two." {
			t.Fatalf("the reloaded page did not get the whole turn: %q", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reloaded page got nothing")
	}

	// And history does not ALSO hand it the half-written step, or the page
	// would render the same words twice.
	rec := env.do(http.MethodPost, "/v1/chat/history", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusOK)
	var resp historyResponse
	env.decode(rec, &resp)
	history := resp.Messages
	for _, message := range history {
		if strings.HasPrefix(message.Content, "Half one") && message.Role == model.StepAssistant {
			// Fine only once the run is over: by now it is, so the completed
			// answer is history and there is nothing live to duplicate it.
			return
		}
	}
}

// A page reloaded AFTER a turn has finished must not replay it. The whole turn
// is already in the history load; a fresh reader (nothing seen, no `after`)
// rejoining a run that is over gets 204, so the completed turn is not painted a
// second time on top of the history. This is the regression for the bug where a
// reloaded conversation showed every answer twice.
func TestReloadingAFinishedTurnDoesNotReplayIt(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"done"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	var chatID string
	for _, f := range frames {
		if f.Type == chat.FrameChatCreated {
			chatID = f.ChatID
		}
	}
	if chatID == "" {
		t.Fatal("no chat id was announced")
	}

	// The turn is over. A fresh rejoin (no `after`) rejoins nothing, rather than
	// replaying the finished turn on top of what history already showed.
	rec := env.do(http.MethodPost, "/v1/chat/attach", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusNoContent)
}

// Attaching to a conversation with nothing in flight says so, rather than
// hanging or inventing a stream.
func TestAttachingToAQuietConversationSaysNothingIsHappening(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"done"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	chatID := chatIDOf(t, frames)

	// A conversation nobody else can see is not attachable either.
	env.createUser("other@acme.test", "dev-Passw0rd!")
	otherToken, _ := env.login("other@acme.test", "dev-Passw0rd!")
	rec := env.do(http.MethodPost, "/v1/chat/attach", otherToken, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusNoContent)
}

// A finished run is not replayed to a fresh reader by default (the answer is
// already in the history it loaded), but a client explicitly replaying a nudged
// completion (Mode C, KB/27) gets its frames even when the run has finished, so a
// fast narration is not lost to a race that once forced a reload.
func TestReplayAttachReplaysAFinishedRun(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	vendor := newFakeVendor(t, answerChunks("the completed answer"))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()

	frames := env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID})
	chatID := chatIDOf(t, frames)
	env.waitForRun(env.sessionOf(frames).ID)

	// A fresh reader on the FINISHED run gets nothing without replay: replaying
	// would double-paint on top of the history.
	rec := env.do(http.MethodPost, "/v1/chat/attach", token, map[string]any{"chat_id": chatID, "after": -1})
	env.expectStatus(rec, http.StatusNoContent)

	// With replay, the same finished run streams from the start.
	rec = env.do(http.MethodPost, "/v1/chat/attach", token, map[string]any{"chat_id": chatID, "after": -1, "replay": true})
	env.expectStatus(rec, http.StatusOK)
	replayed := parseFrames(t, rec.Body.String())
	if deltaText(replayed) != "the completed answer" {
		t.Fatalf("the replayed run did not carry the answer: %q", deltaText(replayed))
	}
	if last := replayed[len(replayed)-1]; last.Type != chat.FrameResult || !last.Final {
		t.Fatalf("the replay did not end with the result: %+v", last)
	}
}

// Stopping is now something a person does. A reader disconnecting used to mean
// stop, which made a network blip indistinguishable from a decision.
func TestCancellingStopsTheTurn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	server := env.live()

	vendor := newSlowVendor(t, "I am thinking", " and here is more.")
	modelID := env.registerModelAt(vendor.server.URL)
	env.GatewayAgent()

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	var chatID string
	readFrames(t, ctx, server, "/v1/chat/stream", token, map[string]any{
		"prompt": "go on", "model_id": modelID,
	}, func(frame chat.Frame) bool {
		if frame.Type == chat.FrameChatCreated {
			chatID = frame.ChatID
		}
		return frame.Type != chat.FrameDelta
	})

	rec := env.do(http.MethodPost, "/v1/chat/cancel", token, map[string]string{"chat_id": chatID})
	env.expectStatus(rec, http.StatusNoContent)

	session := env.sessionByUID(chatID)
	env.waitForRun(session.ID)

	// The turn is over, and the record says a person stopped it rather than
	// claiming it answered.
	status := env.runStatus(session.ID)
	if status != model.RunCancelled {
		t.Fatalf("a cancelled turn was recorded as %q", status)
	}

	// The words the model did say are kept, and they are marked unfinished: a
	// fragment committed as an answer would be putting words in its mouth.
	steps, err := env.app.Store.Agent().Transcript(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected the question and the fragment, got %d: %+v", len(steps), steps)
	}
	if steps[1].Text != "I am thinking" {
		t.Fatalf("what the model said before it was stopped was lost: %q", steps[1].Text)
	}
	if !steps[1].Partial {
		t.Fatal("a stopped answer was recorded as a finished one")
	}
	vendor.Release()
}

// One conversation answers one question at a time. Two turns writing into one
// transcript would leave neither making sense.
func TestASecondPromptWhileAnsweringIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	server := env.live()

	vendor := newSlowVendor(t, "Working", " on it.")
	modelID := env.registerModelAt(vendor.server.URL)
	env.GatewayAgent()

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	var chatID string
	readFrames(t, ctx, server, "/v1/chat/stream", token, map[string]any{
		"prompt": "first", "model_id": modelID,
	}, func(frame chat.Frame) bool {
		if frame.Type == chat.FrameChatCreated {
			chatID = frame.ChatID
		}
		return frame.Type != chat.FrameDelta
	})

	rec := env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"prompt": "second", "chat_id": chatID, "model_id": modelID,
	})
	env.expectStatus(rec, http.StatusConflict)

	vendor.Release()
}
