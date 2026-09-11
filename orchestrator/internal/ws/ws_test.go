package ws

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/rs/zerolog"
)

// The hub, exercised over real sockets: a connection authenticates on its first
// message, presence counts distinct people and coalesces its updates, a
// notification reaches every socket a person holds, and a disconnect is seen.

// testValidate maps a token to an identity: "u<user>w<workspace>", or invalid.
func testValidate(token string) (int64, int64, bool) {
	var user, workspace int64
	if n, err := fmt.Sscanf(token, "u%dw%d", &user, &workspace); err != nil || n != 2 {
		return 0, 0, false
	}
	return user, workspace, true
}

func startHub(t *testing.T) (*Hub, string) {
	return startHubChange(t, nil)
}

func startHubChange(t *testing.T, onChange func(int64)) (*Hub, string) {
	t.Helper()
	h := NewHub(zerolog.Nop(), testValidate, nil)
	if onChange != nil {
		h.OnChange(onChange)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	srv := httptest.NewServer(http.HandlerFunc(h.Serve))
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return h, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// dial opens a chat socket and authenticates it, returning the live connection.
func dial(t *testing.T, url, token string) *websocket.Conn {
	return dialSource(t, url, token, SourceChat)
}

func dialSource(t *testing.T, url, token, source string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.Dial(context.Background(), url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := wsjson.Write(context.Background(), c, Envelope{Type: TypeAuthenticate, Token: token, Source: source}); err != nil {
		t.Fatalf("send authenticate: %v", err)
	}
	if got := readEnvelope(t, c).Type; got != TypeAuthenticated {
		t.Fatalf("expected an authenticated ack, got %q", got)
	}
	return c
}

func readEnvelope(t *testing.T, c *websocket.Conn) Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var env Envelope
	if err := wsjson.Read(ctx, c, &env); err != nil {
		t.Fatalf("read: %v", err)
	}
	return env
}

// awaitConnected polls the hub's synchronous count until it reaches the wanted
// numbers (a register lands on the hub goroutine a moment after dial returns).
func awaitConnected(t *testing.T, h *Hub, ws int64, users, conns int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := h.Connected(ws); p.Users == users && p.Connections == conns {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := h.Connected(ws)
	t.Fatalf("never saw connected users=%d connections=%d (last: %d/%d)", users, conns, got.Users, got.Connections)
}

func TestAuthenticationIsRequiredFirst(t *testing.T) {
	_, url := startHub(t)

	c, resp, err := websocket.Dial(context.Background(), url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// A bad token is refused, and the socket is closed.
	_ = wsjson.Write(context.Background(), c, Envelope{Type: TypeAuthenticate, Token: "nope"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var env Envelope
	if err := wsjson.Read(ctx, c, &env); err == nil {
		t.Fatalf("a bad token should have closed the socket, got %q", env.Type)
	}
}

func TestConnectedCountsPeople(t *testing.T) {
	h, url := startHub(t)

	one := dial(t, url, "u1w1")
	awaitConnected(t, h, 1, 1, 1)

	// A second tab from the same person: one more connection, still one person.
	second := dial(t, url, "u1w1")
	awaitConnected(t, h, 1, 1, 2)

	// A different person: now two people.
	other := dial(t, url, "u2w1")
	awaitConnected(t, h, 1, 2, 3)

	// A console (a dashboard) connects, but is NOT counted: only the chat is.
	console := dialSource(t, url, "u3w1", SourceConsole)
	awaitConnected(t, h, 1, 2, 3)

	// The other person leaves: back to one chat person, two connections.
	_ = other.Close(websocket.StatusNormalClosure, "")
	awaitConnected(t, h, 1, 1, 2)

	_ = console.Close(websocket.StatusNormalClosure, "")
	_ = second.Close(websocket.StatusNormalClosure, "")
	_ = one.Close(websocket.StatusNormalClosure, "")
}

func TestBroadcastReachesTopicSubscribers(t *testing.T) {
	h, url := startHub(t)

	watcher := dial(t, url, "u1w1")
	if err := wsjson.Write(context.Background(), watcher, Envelope{Type: TypeSubscribe, Topic: TopicDashboard}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// A socket in the same workspace that did NOT subscribe must not receive it.
	bystander := dial(t, url, "u2w1")
	time.Sleep(100 * time.Millisecond) // let the subscribe land

	h.Broadcast(1, TopicDashboard, map[string]int{"running": 3})

	env := readEnvelope(t, watcher)
	if env.Type != TypeTopic || env.Topic != TopicDashboard {
		t.Fatalf("expected a dashboard topic frame, got type=%q topic=%q", env.Type, env.Topic)
	}
	if !strings.Contains(string(env.Payload), "\"running\":3") {
		t.Fatalf("payload lost: %s", env.Payload)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var got Envelope
	if err := wsjson.Read(ctx, bystander, &got); err == nil {
		t.Fatalf("a non-subscriber received a topic push: %q", got.Type)
	}

	_ = watcher.Close(websocket.StatusNormalClosure, "")
	_ = bystander.Close(websocket.StatusNormalClosure, "")
}

func TestOnChangeFiresOnConnectAndSubscribe(t *testing.T) {
	changes := make(chan int64, 16)
	_, url := startHubChange(t, func(ws int64) { changes <- ws })

	// A connect fires a change for its workspace.
	c := dial(t, url, "u1w5")
	awaitChange(t, changes, 5)

	// A dashboard subscribe fires one too (the new watcher needs a snapshot).
	if err := wsjson.Write(context.Background(), c, Envelope{Type: TypeSubscribe, Topic: TopicDashboard}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	awaitChange(t, changes, 5)

	// A disconnect fires one as well.
	_ = c.Close(websocket.StatusNormalClosure, "")
	awaitChange(t, changes, 5)
}

func awaitChange(t *testing.T, changes chan int64, ws int64) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-changes:
			if got == ws {
				return
			}
		case <-deadline:
			t.Fatalf("never saw an onChange for workspace %d", ws)
		}
	}
}

func TestNotifyReachesEveryTab(t *testing.T) {
	h, url := startHub(t)

	tabA := dial(t, url, "u7w3")
	tabB := dial(t, url, "u7w3")

	// Give the registrations time to land in the hub.
	time.Sleep(100 * time.Millisecond)
	h.Notify(3, 7, map[string]string{"kind": "run_finished"})

	for _, tab := range []*websocket.Conn{tabA, tabB} {
		env := readEnvelope(t, tab)
		if env.Type != TypeNotification {
			t.Fatalf("expected a notification, got %q", env.Type)
		}
		if !strings.Contains(string(env.Payload), "run_finished") {
			t.Fatalf("notification payload lost: %s", env.Payload)
		}
	}

	// A notification for someone else does not arrive here.
	h.Notify(3, 99, map[string]string{"kind": "not_yours"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var env Envelope
	if err := wsjson.Read(ctx, tabA, &env); err == nil {
		t.Fatalf("received a notification meant for another user: %q", env.Type)
	}

	_ = tabA.Close(websocket.StatusNormalClosure, "")
	_ = tabB.Close(websocket.StatusNormalClosure, "")
}
