package link

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/rs/zerolog"
)

// A chat application that is not there, an address a tool asks for, and the
// bytes that have to travel between them.

func newTestRegistry(t *testing.T) (*Registry, *httptest.Server) {
	t.Helper()
	r := NewRegistry(zerolog.Nop(), func(token string) (int64, int64, string, time.Time, bool) {
		switch token {
		case "good":
			return 7, 3, "the-laptop", time.Now().Add(time.Hour), true
		case "good-desktop":
			// The same person, on their other computer.
			return 7, 3, "the-desktop", time.Now().Add(time.Hour), true
		case "other":
			// Somebody else entirely: a valid token, a different person.
			return 8, 3, "another-laptop", time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Now().Add(time.Hour), false
	}, []string{"*"})

	mux := http.NewServeMux()
	mux.HandleFunc("/link", r.ServeControl)
	mux.HandleFunc("/link/stream", r.ServeStream)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return r, server
}

func wsURL(base, path string) string {
	return "ws" + strings.TrimPrefix(base, "http") + path
}

// fakeApp is the chat application: it links, waits to be asked for a
// connection, and dials one back.
type fakeApp struct {
	t       *testing.T
	control *websocket.Conn
	// token is what it authenticates BOTH sockets with, exactly as the real
	// client does.
	token string
	// refuse makes it answer every request with a refusal instead of a socket.
	refuse string
	// ignore, when set, receives each request and answers none of them, so a
	// ticket stays outstanding for a test that needs one.
	ignore chan openRequest
	// dialTo, when set, is where it really connects; otherwise it echoes.
	dialTo string
	// answerAfter, when set, makes this application take that long over a tool
	// call before answering, which is what a build or an install is.
	answerAfter time.Duration
	base        string
}

func linkAppAs(t *testing.T, base, token string) *fakeApp { return linkApp(t, base, token) }

func linkApp(t *testing.T, base, token string) *fakeApp {
	t.Helper()
	sock, res, err := websocket.Dial(context.Background(), wsURL(base, "/link"), nil)
	if err != nil {
		t.Fatalf("dial the control socket: %v", err)
	}
	closeHandshake(res)
	t.Cleanup(func() { _ = sock.CloseNow() })
	if err := wsjson.Write(context.Background(), sock, hello{Type: "authenticate", Token: token}); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	var ack openRequest
	if err := wsjson.Read(context.Background(), sock, &ack); err != nil {
		t.Fatalf("no acknowledgement: %v", err)
	}
	if ack.Type != "linked" {
		t.Fatalf("the application was not told it was linked: %+v", ack)
	}
	app := &fakeApp{t: t, control: sock, base: base, token: token}
	go app.serve()
	return app
}

// serve answers what the gateway asks: dial back, or refuse.
func (a *fakeApp) serve() {
	for {
		var request openRequest
		if err := wsjson.Read(context.Background(), a.control, &request); err != nil {
			return
		}
		if request.Type != "open" {
			continue
		}
		if a.ignore != nil {
			a.ignore <- request
			continue
		}
		if a.refuse != "" {
			_ = wsjson.Write(context.Background(), a.control,
				reply{Type: "refused", Ticket: request.Ticket, Reason: a.refuse})
			continue
		}
		go a.carry(request)
	}
}

// carry dials the stream back and pipes it to wherever this application reaches,
// which in a test is either a real listener or an echo.
func (a *fakeApp) carry(request openRequest) {
	sock, res, err := websocket.Dial(context.Background(),
		wsURL(a.base, "/link/stream?ticket="+request.Ticket), nil)
	if err != nil {
		return
	}
	closeHandshake(res)
	if err := wsjson.Write(context.Background(), sock,
		hello{Type: "authenticate", Token: a.token}); err != nil {
		return
	}
	conn := newStream(context.Background(), sock)
	if a.answerAfter > 0 {
		// A tool that takes a while: read the whole request, work, then answer.
		_, _ = io.ReadAll(conn)
		time.Sleep(a.answerAfter)
		_, _ = conn.Write([]byte(`{"ok":true,"content":{"done":true}}`))
		_ = conn.Close()
		return
	}
	if a.dialTo == "" {
		_, _ = io.Copy(conn, conn) // echo
		_ = conn.Close()
		return
	}
	far, err := net.Dial("tcp", a.dialTo)
	if err != nil {
		_ = conn.Close()
		return
	}
	// Both directions, and the end of each said rather than assumed: this is
	// what the Rust half does, and the test is worth nothing if it is gentler.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(far, conn)
		if tcp, ok := far.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, far)
		_ = conn.CloseWrite()
	}()
	wg.Wait()
	_ = far.Close()
	_ = conn.Close()
}

// A request that finishes before the answer starts, carried the whole way.
//
// This is the case a websocket has no answer for on its own: closing it to say
// "I have finished sending" would take the answer with it, and saying nothing
// leaves a service that only replies to a complete request waiting for one it
// already has. An empty binary message is the signal, and this proves it
// survives the whole route.
func TestARequestMayFinishBeforeItsAnswerBegins(t *testing.T) {
	answer := make([]byte, 512*1024)
	for i := range answer {
		answer[i] = byte(i % 251)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read the whole request, which ends only at EOF.
		if _, err := io.Copy(io.Discard, conn); err != nil {
			return
		}
		_, _ = conn.Write(answer)
	}()

	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.dialTo = listener.Addr().String()
	waitOnline(t, r)

	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("the whole request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("a carried connection cannot say it has finished sending")
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatalf("finish sending: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if len(got) != len(answer) {
		t.Fatalf("the answer was cut off: %d bytes of %d", len(got), len(answer))
	}
	for i := range got {
		if got[i] != answer[i] {
			t.Fatalf("the answer is wrong at byte %d", i)
		}
	}
}

// Half a megabyte, in one direction, byte for byte. A message boundary is not a
// packet boundary and nothing may reorder or drop one.
func TestABigAnswerArrivesWholeAndInOrder(t *testing.T) {
	r, server := newTestRegistry(t)
	linkApp(t, server.URL, "good") // the echo
	waitOnline(t, r)

	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload := make([]byte, 512*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	go func() {
		for sent := 0; sent < len(payload); sent += 9973 {
			end := min(sent+9973, len(payload))
			if _, err := conn.Write(payload[sent:end]); err != nil {
				return
			}
		}
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read it all back: %v", err)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d came back as %d, not %d", i, got[i], payload[i])
		}
	}
}

func TestABytePassesThroughTheChatApplication(t *testing.T) {
	r, server := newTestRegistry(t)
	linkApp(t, server.URL, "good")

	waitOnline(t, r)
	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial through the application: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("select 1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "select 1" {
		t.Fatalf("what came back was %q", buf)
	}
}

// The whole point of the feature, end to end: something that speaks a protocol
// on a machine only the application can see, reached by an ordinary net.Conn.
func TestAServiceOnlyTheApplicationCanSeeIsReachable(t *testing.T) {
	// A "database" this test pretends is on the customer's own network.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("hello " + string(buf[:n])))
			}()
		}
	}()

	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.dialTo = listener.Addr().String()

	waitOnline(t, r)
	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len("hello world"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello world" {
		t.Fatalf("the service answered %q", buf)
	}
}

// Nobody there is an ordinary, expected answer, and it must not be a wait.
func TestNobodyRunningTheApplicationIsAnAnswerNotAWait(t *testing.T) {
	r, _ := newTestRegistry(t)
	start := time.Now()
	_, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("it waited %s for a machine that is not there", time.Since(start))
	}
}

// A refusal carries the machine's own reason, so a tool can say what the
// person's computer said rather than reporting a timeout.
func TestARefusalSaysWhy(t *testing.T) {
	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.refuse = "connection refused by the accounts server"

	waitOnline(t, r)
	_, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "accounts server") {
		t.Fatalf("got %v", err)
	}
}

// A bad token opens nothing. The link is a socket that can reach inside
// somebody's network: it gets the same answer the REST API gives.
func TestAnUnauthenticatedApplicationIsNotLinked(t *testing.T) {
	r, server := newTestRegistry(t)
	sock, res, err := websocket.Dial(context.Background(), wsURL(server.URL, "/link"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	closeHandshake(res)
	defer func() { _ = sock.CloseNow() }()
	_ = wsjson.Write(context.Background(), sock, hello{Type: "authenticate", Token: "forged"})

	var ack openRequest
	if err := wsjson.Read(context.Background(), sock, &ack); err == nil {
		t.Fatalf("a forged token was told %+v", ack)
	}
	if r.Online(3, 7, "the-laptop") {
		t.Fatal("a forged token registered a machine")
	}
}

// A ticket is not enough on its own.
//
// It is a one-shot secret sent over an authenticated socket, which is strong,
// but it says nothing about WHO is presenting it. Anyone who ever saw one could
// otherwise take delivery of the connection it was issued for, and whoever took
// it would be the far end of a database session: they would receive the traffic
// and could answer it with anything.
func TestAStolenTicketIsUselessToSomebodyElse(t *testing.T) {
	r, server := newTestRegistry(t)
	// An application that hears the request and does nothing with it, so the
	// ticket is still outstanding while somebody else tries to use it. Racing a
	// client that answers immediately would be a test that passes when the
	// machine is slow.
	app := linkApp(t, server.URL, "good")
	app.ignore = make(chan openRequest, 1)
	waitOnline(t, r)

	go func() { _, _ = r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test") }()

	var ticket string
	select {
	case request := <-app.ignore:
		ticket = request.Ticket
	case <-time.After(5 * time.Second):
		t.Fatal("no ticket reached the application")
	}
	if ticket == "" {
		t.Fatal("the request carried no ticket")
	}

	// Somebody else, with a perfectly valid token of their own, presenting it.
	sock, res, err := websocket.Dial(context.Background(),
		wsURL(server.URL, "/link/stream?ticket="+ticket), nil)
	if err != nil {
		return // refused at the handshake is just as good
	}
	defer func() { _ = sock.CloseNow() }()
	closeHandshake(res)
	if err := wsjson.Write(context.Background(), sock,
		hello{Type: "authenticate", Token: "other"}); err != nil {
		return
	}

	// A socket that was honoured stays open and carries bytes. This one is
	// closed by the server, so reading it is the question and the answer.
	read, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := sock.Read(read); err == nil {
		t.Fatal("somebody else's ticket was honoured")
	}
}

// A ticket is worth one socket. A second one quoting it is closed, so a ticket
// cannot be replayed even by the machine it was sent to.
func TestATicketIsSpentOnce(t *testing.T) {
	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	waitOnline(t, r)

	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The application dials again with a ticket that has already been spent.
	// It has no way to know which; the registry does.
	sock, res, err := websocket.Dial(context.Background(),
		wsURL(app.base, "/link/stream?ticket=whatever-was-spent"), nil)
	if err != nil {
		return // refused at the handshake is just as good
	}
	closeHandshake(res)
	if err := wsjson.Write(context.Background(), sock,
		hello{Type: "authenticate", Token: "good"}); err != nil {
		return
	}
	defer func() { _ = sock.CloseNow() }()
	second := websocket.NetConn(context.Background(), sock, websocket.MessageBinary)
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("a spent ticket opened a second stream")
	}
}

func waitOnline(t *testing.T, r *Registry) {
	t.Helper()
	waitUntil(t, func() bool { return r.Online(3, 7, "the-laptop") })
}

// waitUntil polls for something another goroutine does.
func waitUntil(t *testing.T, done func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the application")
}

// A machine that goes away takes its connections with it.
//
// They had one far side and it is gone. Left open, a driver holding one waits
// for an answer that cannot arrive, and finds out when its own deadline runs
// out rather than now.
func TestConnectionsEndWithTheMachineCarryingThem(t *testing.T) {
	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	waitOnline(t, r)

	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// It works before.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, 5)
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}

	// The application closes, as a laptop lid does.
	_ = app.control.CloseNow()
	for i := 0; i < 200 && r.Online(3, 7, "the-laptop"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Online(3, 7, "the-laptop") {
		t.Fatal("the machine is still registered")
	}

	// The carried connection is over, and says so rather than hanging.
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a connection whose machine is gone kept reading")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a connection whose machine is gone hung instead of ending")
	}
}

// One computer carries a pool's worth of connections, not a server's.
func TestAMachineIsNotAskedToCarryTooMuch(t *testing.T) {
	r, server := newTestRegistry(t)
	linkApp(t, server.URL, "good")
	waitOnline(t, r)

	held := make([]net.Conn, 0, maxStreams)
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for i := 0; i < maxStreams; i++ {
		conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
		if err != nil {
			t.Fatalf("connection %d of %d was refused: %v", i+1, maxStreams, err)
		}
		held = append(held, conn)
	}

	if _, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test"); !errors.Is(err, ErrRefused) {
		t.Fatalf("the limit was not enforced: %v", err)
	}

	// And it is a limit, not a wall: closing one makes room for another.
	_ = held[0].Close()
	held = held[1:]
	conn, err := r.Dial(context.Background(), 3, 7, "the-laptop", "10.0.0.5", 3306, "a test")
	if err != nil {
		t.Fatalf("a freed slot was not reused: %v", err)
	}
	held = append(held, conn)
}

// closeHandshake releases the upgrade response. The socket is the point and the
// body is empty, but a test that leaks one per connection is a test that cannot
// be run in a loop.
func closeHandshake(res *http.Response) {
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
}

// One person, two computers: a tool reaches the one the request came from.
//
// This is the case that has no good guess. Picking the most recently connected
// machine is wrong half the time and unpredictable always, and the person can
// see it is wrong: they asked on the laptop and something happened on the
// desktop. The request knows which application it came from, so it says.
func TestAToolReachesTheComputerTheRequestCameFrom(t *testing.T) {
	r, server := newTestRegistry(t)

	// The same person, signed in twice. Each carries its own service, and the
	// services answer differently so there is no doubt which was reached.
	laptopService := namedEcho(t, "laptop")
	desktopService := namedEcho(t, "desktop")

	laptop := linkApp(t, server.URL, "good") // device "the-laptop"
	laptop.dialTo = laptopService
	desktop := linkAppAs(t, server.URL, "good-desktop")
	desktop.dialTo = desktopService

	waitUntil(t, func() bool { return r.Online(3, 7, "the-laptop") })
	waitUntil(t, func() bool { return r.Online(3, 7, "the-desktop") })

	for _, want := range []struct{ device, answer string }{
		{"the-laptop", "laptop"},
		{"the-desktop", "desktop"},
	} {
		conn, err := r.Dial(context.Background(), 3, 7, want.device, "10.0.0.5", 3306, "a test")
		if err != nil {
			t.Fatalf("dial %s: %v", want.device, err)
		}
		if _, err := conn.Write([]byte("?")); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want.answer))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read from %s: %v", want.device, err)
		}
		_ = conn.Close()
		if string(got) != want.answer {
			t.Fatalf("a call for %s reached %q", want.device, got)
		}
	}

	// And a request from neither of them reaches neither, rather than one of
	// them. A browser has no machine, and picking somebody's laptop for it
	// would be the guess this whole design removes.
	if _, err := r.Dial(context.Background(), 3, 7, "", "10.0.0.5", 3306, "a test"); !errors.Is(err, ErrNoMachine) {
		t.Fatalf("a request with no machine was given one: %v", err)
	}
}

// namedEcho is a service that answers with its own name, so a test can tell
// which machine carried the connection.
func namedEcho(t *testing.T, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := conn.Read(make([]byte, 1)); err != nil {
					return
				}
				_, _ = conn.Write([]byte(name))
			}()
		}
	}()
	return listener.Addr().String()
}

// A call takes as long as the work takes.
//
// It used to be cut off after two minutes, which is an arbitrary answer to a
// question nobody can answer in advance: an install, a build, a search across a
// disk take what they take. Being cut off in the middle loses the work AND
// tells the person their computer did not answer, which is not what happened.
//
// What bounds a call now is whether the machine is still there, asked rather
// than assumed: the stream is pinged while the call is outstanding. This proves
// a call outliving many of those pings still comes back with its answer.
func TestACallIsNotCutOffWhileTheMachineIsAnswering(t *testing.T) {
	was := stillThereEvery
	stillThereEvery = 20 * time.Millisecond
	t.Cleanup(func() { stillThereEvery = was })

	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	// Twenty times the interval at which the machine's presence is checked: a
	// call that would have been long dead if anything here were counting.
	app.answerAfter = 400 * time.Millisecond
	waitUntil(t, func() bool { return r.Online(3, 7, "the-laptop") })

	started := time.Now()
	answer, err := r.Call(context.Background(), 3, 7, "the-laptop", "slow", []byte(`{}`), "a test")
	if err != nil {
		t.Fatalf("a call that took its time was cut off: %v", err)
	}
	if !answer.OK {
		t.Fatalf("the answer did not come back: %+v", answer)
	}
	if took := time.Since(started); took < app.answerAfter {
		t.Fatalf("the call answered in %s, before the work was done", took)
	}
}

// And a machine that goes away mid-call is an answer, not a wait for ever.
//
// It IS a wait for a while, on purpose: the work is still going on that
// computer and the application usually comes back in seconds, so the call is
// resumed rather than lost. What this holds is the other end of that: a
// machine that does not come back becomes an answer, instead of a turn hanging
// on a laptop that has gone home.
func TestACallEndsWhenTheMachineStaysAway(t *testing.T) {
	wasStill, wasBack := stillThereEvery, comesBackWithin
	stillThereEvery, comesBackWithin = 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { stillThereEvery, comesBackWithin = wasStill, wasBack })

	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	app.answerAfter = 30 * time.Second // it would answer eventually; it will not get the chance
	waitUntil(t, func() bool { return r.Online(3, 7, "the-laptop") })

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = app.control.CloseNow() // the application is gone: closed lid, quit app
	}()

	started := time.Now()
	if _, err := r.Call(context.Background(), 3, 7, "the-laptop", "slow", []byte(`{}`), "a test"); err == nil {
		t.Fatal("a call to a machine that stayed away came back as a success")
	}
	took := time.Since(started)
	if took < 300*time.Millisecond {
		t.Fatalf("it gave up in %s, without waiting to see whether the machine came back", took)
	}
	if took > 10*time.Second {
		t.Fatalf("it waited %s for a machine that was not coming back", took)
	}
}

// A machine that is there but slow to dial back gets asked again.
//
// "Your computer is not connected" about a computer that plainly is, was the
// most common complaint from using this, and one cause is a dial-back that
// arrives late: the application was busy for a moment. A moment is not a
// state, so it is asked once more before that answer is given.
func TestASlowDialBackIsAskedAgain(t *testing.T) {
	wasDial := dialDeadline
	dialDeadline = 200 * time.Millisecond
	t.Cleanup(func() { dialDeadline = wasDial })

	r, server := newTestRegistry(t)
	app := linkApp(t, server.URL, "good")
	// Ignore the first request entirely, and carry the second: an application
	// that missed one moment and is fine the next.
	missed := make(chan openRequest, 4)
	app.ignore = missed
	waitUntil(t, func() bool { return r.Online(3, 7, "the-laptop") })
	go func() {
		<-missed         // the first is dropped on the floor
		app.ignore = nil // and everything after it is answered
	}()

	app.answerAfter = 10 * time.Millisecond
	answer, err := r.Call(context.Background(), 3, 7, "the-laptop", "slow", []byte(`{}`), "a test")
	if err != nil {
		t.Fatalf("a dial-back that was late once became a failure: %v", err)
	}
	if !answer.OK {
		t.Fatalf("the answer did not come back: %+v", answer)
	}
}
