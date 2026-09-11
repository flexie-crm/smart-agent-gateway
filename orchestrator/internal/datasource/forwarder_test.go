package datasource

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// What a forwarder must not do is end a connection early.
//
// It returned on the FIRST direction to finish and closed both, so a client
// that said everything it had to say and then waited for the answer could have
// the answer cut off wherever it had reached. Nothing reports that: the driver
// sees a connection that ended, and the rows it had not read are gone.

// answerAfterEOF is a service that reads until the client half-closes, and only
// then replies. It is how a protocol that says "that is my whole request" works,
// and the shape that catches this exactly.
func answerAfterEOF(t *testing.T, answer []byte) (host string, port int) {
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
				if _, err := io.Copy(io.Discard, conn); err != nil {
					return
				}
				// The request is complete. Now the long answer, in pieces, so a
				// close in the middle of it truncates rather than racing.
				for sent := 0; sent < len(answer); sent += 4096 {
					end := min(sent+4096, len(answer))
					if _, err := conn.Write(answer[sent:end]); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(p)
	return h, number
}

func TestAnAnswerIsNotCutOffWhenTheRequestFinishesFirst(t *testing.T) {
	answer := make([]byte, 256*1024)
	for i := range answer {
		answer[i] = byte(i % 251)
	}
	host, port := answerAfterEOF(t, answer)

	f, localHost, localPort, err := forward(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}, false, nil)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer func() { _ = f.Close() }()

	conn, err := net.Dial("tcp", net.JoinHostPort(localHost, strconv.Itoa(localPort)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("the whole request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// "That is everything I am sending." The other direction is still open, and
	// everything on it must still arrive.
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatalf("half close: %v", err)
		}
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

// And the reverse: when the far side finishes, the local end must read an EOF
// rather than nothing, or a client waits for an answer that has already been
// given in full.
func TestTheLocalEndIsToldWhenTheFarSideIsDone(t *testing.T) {
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
		_, _ = conn.Write([]byte("done"))
		_ = conn.Close()
	}()

	f, host, port, err := forward(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}, false, nil)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer func() { _ = f.Close() }()

	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("the local end never read an EOF: %v", err)
	}
	if string(got) != "done" {
		t.Fatalf("got %q", got)
	}
}

// Many connections at once through one forwarder, each carrying its own bytes.
// A pipe that mixed two of them would be unusable and quiet about it.
func TestConnectionsDoNotMixWithEachOther(t *testing.T) {
	host, port := newEchoServer(t)
	f, localHost, localPort, err := forward(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}, false, nil)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer func() { _ = f.Close() }()

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", net.JoinHostPort(localHost, strconv.Itoa(localPort)))
			if err != nil {
				t.Errorf("dial %d: %v", n, err)
				return
			}
			defer func() { _ = conn.Close() }()

			mine := []byte(strconv.Itoa(n) + "-" + strconv.Itoa(n*7919))
			if _, err := conn.Write(mine); err != nil {
				t.Errorf("write %d: %v", n, err)
				return
			}
			back := make([]byte, len(mine))
			if _, err := io.ReadFull(conn, back); err != nil {
				t.Errorf("read %d: %v", n, err)
				return
			}
			if string(back) != string(mine) {
				t.Errorf("connection %d was given %q, not its own %q", n, back, mine)
			}
		}(i)
	}
	wg.Wait()
}
