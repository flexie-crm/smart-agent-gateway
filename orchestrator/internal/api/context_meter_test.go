package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/ws"
)

// The meter above the message box and the freeze while a conversation is
// compacted, through the real server, the real database, the real socket and
// the real loop, with only the model a stand-in.

// contextPushes is what the socket says about how full conversations are.
type contextPushes struct {
	mu    sync.Mutex
	heard []contextPushed
}

type contextPushed struct {
	ChatUID string `json:"chat_uid"`
	app.ContextMeter
}

// listen opens the chat's socket as the person and keeps every context push.
func (e *testEnv) listen(token string) *contextPushes {
	e.t.Helper()
	server := httptest.NewServer(e.router)
	e.t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		e.t.Fatalf("dial the socket: %v", err)
	}
	e.t.Cleanup(func() { _ = conn.CloseNow() })
	if err := wsjson.Write(ctx, conn, ws.Envelope{Type: ws.TypeAuthenticate, Token: token, Source: ws.SourceChat}); err != nil {
		e.t.Fatalf("authenticate: %v", err)
	}
	var ack ws.Envelope
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != ws.TypeAuthenticated {
		e.t.Fatalf("the socket did not authenticate: %v %q", err, ack.Type)
	}

	pushes := &contextPushes{}
	go func() {
		for {
			var env ws.Envelope
			if err := wsjson.Read(ctx, conn, &env); err != nil {
				return
			}
			var inner struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if env.Type != ws.TypeNotification || json.Unmarshal(env.Payload, &inner) != nil || inner.Type != "context" {
				continue
			}
			var pushed contextPushed
			if json.Unmarshal(inner.Payload, &pushed) == nil {
				pushes.mu.Lock()
				pushes.heard = append(pushes.heard, pushed)
				pushes.mu.Unlock()
			}
		}
	}()
	return pushes
}

// waitFor waits for a push about chat that matches, and answers it.
func (p *contextPushes) waitFor(t *testing.T, chat string, match func(contextPushed) bool) contextPushed {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, pushed := range p.heard {
			if pushed.ChatUID == chat && match(pushed) {
				p.mu.Unlock()
				return pushed
			}
		}
		p.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the socket never said that about %s", chat)
	return contextPushed{}
}

// last is the newest push about chat.
func (p *contextPushes) last(chat string) (contextPushed, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.heard) - 1; i >= 0; i-- {
		if p.heard[i].ChatUID == chat {
			return p.heard[i], true
		}
	}
	return contextPushed{}, false
}

func (e *testEnv) meterOf(token, chat string) app.ContextMeter {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": chat})
	e.expectStatus(rec, http.StatusOK)
	var history historyResponse
	e.decode(rec, &history)
	return history.Meta.Context
}

// percentOf is the percentage worked out here, by hand, from the stored
// measurement and the model as it stands: the room the trim works in is the
// window less 8,000 tokens kept for the reply, at the model's characters per
// token (its measured totals divided, or 2 until it has reported), less the 5
// percent kept free; and the percentage is rounded up.
func (e *testEnv) percentOf(chars int, modelID int64) int {
	e.t.Helper()
	m, err := e.app.Store.AIModels().GetByID(context.Background(), e.ws.ID, modelID)
	if err != nil {
		e.t.Fatalf("read model: %v", err)
	}
	rate := 2.0
	if m.MeasuredTokens > 0 {
		rate = m.MeasuredChars / m.MeasuredTokens
	}
	room := math.Floor(float64(m.ContextWindow-8000) * rate * 0.95)
	return int(math.Ceil(float64(chars) * 100 / room))
}

// How full a conversation is: measured in every turn, kept, shown when it is
// opened and pushed while it is used; against the model's window as it is set
// now; and down again once it is compacted.
func TestTheMeterSaysHowFullTheConversationIs(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	pushes := env.listen(token)

	const summary = "They said hello, then pasted a very long text."
	vendor := newFakeVendor(t, said(t, "Hello."), said(t, "Greeting"), said(t, "That is long."), said(t, summary))
	modelID := env.registerModel(vendor) // a 100,000 token window
	env.GatewayAgent()
	ctx := context.Background()

	session := env.sessionOf(env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)

	first, err := env.app.Store.Agent().ContextUse(ctx, session.ID)
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	if first.ModelID != modelID || first.BaseChars <= 0 || first.Chars <= first.BaseChars {
		t.Fatalf("the turn was not measured: %+v", first)
	}
	meter := env.meterOf(token, session.UID)
	if meter.Percent == nil || *meter.Percent != env.percentOf(first.Chars, modelID) || meter.WarnAt != config.DefaultContextWarnPercent || meter.Compacting {
		t.Fatalf("opening the conversation shows %+v, want %d%% of the window", meter, env.percentOf(first.Chars, modelID))
	}
	// And the socket said the same while the turn ran.
	if pushed, ok := pushes.last(session.UID); !ok || pushed.Percent == nil || *pushed.Percent != *meter.Percent {
		t.Fatalf("the socket said %+v, the conversation says %d%%", pushed, *meter.Percent)
	}

	// A long message fills it by at least its own length: everything said is
	// counted, not only the answers.
	long := strings.Repeat("a long pasted line of text. ", 2000)
	env.streamTurn(token, map[string]any{"prompt": long, "model_id": modelID, "chat_id": session.UID})
	second, err := env.app.Store.Agent().ContextUse(ctx, session.ID)
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	if second.Chars-first.Chars < len(long) {
		t.Fatalf("a %d character message moved the measure by %d", len(long), second.Chars-first.Chars)
	}
	pushes.waitFor(t, session.UID, func(p contextPushed) bool {
		return p.Percent != nil && *p.Percent == env.percentOf(second.Chars, modelID)
	})

	// The window is read when somebody looks, so changing it moves the meter
	// without another turn.
	m, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	m.ContextWindow = 50_000
	if err := env.app.Store.AIModels().Update(ctx, m, model.Nobody()); err != nil {
		t.Fatalf("update model: %v", err)
	}
	halved := env.percentOf(second.Chars, modelID)
	if meter := env.meterOf(token, session.UID); meter.Percent == nil || *meter.Percent != halved {
		t.Fatalf("after halving the window the meter shows %+v, want %d%%", meter, halved)
	}

	// Compacted: what is left is the system prompt, the tools and the summary.
	env.compact(token, session, modelID)
	after, err := env.app.Store.Agent().ContextUse(ctx, session.ID)
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	if after.Chars != second.BaseChars+agent.SummaryChars(summary) {
		t.Fatalf("after compacting the measure is %d, want the base %d plus the summary %d",
			after.Chars, second.BaseChars, agent.SummaryChars(summary))
	}
	done := pushes.waitFor(t, session.UID, func(p contextPushed) bool {
		return !p.Compacting && p.Percent != nil && *p.Percent == env.percentOf(after.Chars, modelID)
	})
	if *done.Percent >= halved {
		t.Fatalf("compacting did not make room: %d%% before, %d%% after", halved, *done.Percent)
	}
}

// While a summary is written the conversation is frozen: a reload says so,
// nothing can be said into it, and a second compaction is refused. When it is
// done every tab hears, and the conversation takes messages again.
func TestTheChatIsFrozenWhileItIsCompacted(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")
	pushes := env.listen(token)

	// A model that answers turns and names conversations at once, and holds a
	// summary until the test lets it go.
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	summarizing := make(chan struct{}, 1)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if streaming, _ := body["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range said(t, "Hello.") {
				_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		messages, _ := body["messages"].([]any)
		first, _ := messages[0].(map[string]any)
		if instruction, _ := first["content"].(string); strings.HasPrefix(instruction, "You are compacting") {
			summarizing <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(completionJSON(t, said(t, "They said hello."))))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(completionJSON(t, said(t, "Greeting"))))
	}))
	// Let go before the server closes (deferred calls run last first): closing
	// waits for the answer held open here.
	defer vendor.Close()
	defer letGo()
	modelID := env.registerModelAt(vendor.URL)
	env.GatewayAgent()

	session := env.sessionOf(env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)

	rec := env.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, token, map[string]any{"model_id": modelID})
	env.expectStatus(rec, http.StatusAccepted)
	select {
	case <-summarizing:
	case <-time.After(10 * time.Second):
		t.Fatal("the summary was never asked for")
	}

	pushes.waitFor(t, session.UID, func(p contextPushed) bool { return p.Compacting })
	if meter := env.meterOf(token, session.UID); !meter.Compacting {
		t.Fatal("a reload during the compaction is not told the conversation is being compacted")
	}
	rec = env.do(http.MethodPost, "/v1/chat/stream", token, map[string]any{
		"prompt": "are you there?", "model_id": modelID, "chat_id": session.UID,
	})
	env.expectFields(rec, http.StatusConflict, "compacting", nil)
	rec = env.do(http.MethodPost, "/v1/chat/chats/compact/"+session.UID, token, map[string]any{"model_id": modelID})
	env.expectFields(rec, http.StatusConflict, "chat_busy", nil)

	letGo()
	env.waitUntilCompacted(session.ID)
	pushes.waitFor(t, session.UID, func(p contextPushed) bool { return !p.Compacting && p.Failed == "" })
	if meter := env.meterOf(token, session.UID); meter.Compacting {
		t.Fatal("the conversation still says it is being compacted")
	}
	if _, err := env.app.Store.Agent().LatestCompaction(context.Background(), session.ID); err != nil {
		t.Fatalf("the summary was not kept: %v", err)
	}
	// And it takes messages again.
	env.streamTurn(token, map[string]any{"prompt": "are you there?", "model_id": modelID, "chat_id": session.UID})
}

// A model learns how big its tokens are from what its vendor reports: the
// characters of the request exactly as it went beside the tokens the vendor
// said it was. And from then on the meter measures the conversation at that
// rate rather than at the guess.
func TestAModelLearnsItsRateFromWhatItsVendorReports(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("u@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("u@acme.test", "dev-Passw0rd!")

	const reported = 4000
	answer := append(said(t, "Hello."), fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":2,"total_tokens":%d}}`, reported, reported+2))
	vendor := newFakeVendor(t, answer, said(t, "Greeting"))
	modelID := env.registerModel(vendor)
	env.GatewayAgent()
	ctx := context.Background()

	before, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil || before.MeasuredTokens != 0 {
		t.Fatalf("the model had a rate before it was ever asked: %+v (%v)", before, err)
	}

	session := env.sessionOf(env.streamTurn(token, map[string]any{"prompt": "hi", "model_id": modelID}))
	env.waitUntilNamed(session.ID)

	// The turn's call, and what it recorded it sent.
	var sent int64
	if err := env.sql.DB().QueryRowContext(ctx,
		`SELECT input_chars FROM model_calls WHERE session_id = ? AND input_tokens = ? ORDER BY id LIMIT 1`,
		session.ID, reported).Scan(&sent); err != nil {
		t.Fatalf("the turn's call was not recorded with what it sent: %v", err)
	}
	learned, err := env.app.Store.AIModels().GetByID(ctx, env.ws.ID, modelID)
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	if sent <= 0 || learned.MeasuredChars != float64(sent) || learned.MeasuredTokens != reported {
		t.Fatalf("the model learned %v characters as %v tokens, want the %d sent as %d",
			learned.MeasuredChars, learned.MeasuredTokens, sent, reported)
	}

	// The meter now measures at that rate, which is not the guess.
	use, err := env.app.Store.Agent().ContextUse(ctx, session.ID)
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	want := env.percentOf(use.Chars, modelID)
	atTheGuess := int(math.Ceil(float64(use.Chars) * 100 / math.Floor(float64(100_000-8000)*2*0.95)))
	if want == atTheGuess {
		t.Fatalf("the learned rate and the guess give the same %d%%, so this proves nothing", want)
	}
	if meter := env.meterOf(token, session.UID); meter.Percent == nil || *meter.Percent != want {
		t.Fatalf("the meter shows %+v, want %d%% at the learned rate (%d%% would be the guess)", meter, want, atTheGuess)
	}
}
