package datasource

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// A local address that stands in for a remote one.
//
// Some databases cannot be dialled from here: they sit behind an SSH bastion,
// or on a network only somebody's own computer can see. Both are answered the
// same way, and it is the older half of this file that found the shape: open a
// listener on 127.0.0.1, carry every connection accepted on it to the real
// database by whatever means, and hand back the local address. The driver then
// connects to a perfectly ordinary local port and knows nothing.
//
// That is what makes one implementation serve every driver, present and future,
// and what lets a second way of reaching a database (internal/link, through the
// chat application) cost a dial function rather than a change to every driver.

// probeTimeout bounds the one throwaway connection made to prove the far side
// will carry traffic. It is a person waiting on a Test button, not a batch job.
const probeTimeout = 20 * time.Second

// dialer opens one connection to the far side. It is called once per connection
// the driver makes, so it must be safe to call concurrently.
type dialer func(ctx context.Context) (net.Conn, error)

// forwarder is a local listener whose every connection is piped to the far side.
type forwarder struct {
	listener net.Listener
	dial     dialer
	// after runs when the forwarder closes, for whatever the dialler is built
	// on (an SSH client, and nothing at all for a link).
	after func() error
}

// forward opens the listener and starts serving. probe, when true, opens one
// connection first and throws it away: the caller then learns that the far side
// will not carry a connection HERE, where there is still somebody to tell,
// rather than through a driver reporting a reset local socket.
func forward(dial dialer, probe bool, after func() error) (*forwarder, string, int, error) {
	if probe {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		trial, err := dial(ctx)
		cancel()
		if err != nil {
			return nil, "", 0, err
		}
		_ = trial.Close()
	}

	// The listener lives for the forwarder's lifetime, not a request's, so it is
	// bound to a background context; Close stops it.
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		if after != nil {
			_ = after()
		}
		return nil, "", 0, fmt.Errorf("open the local end: %w", err)
	}

	f := &forwarder{listener: listener, dial: dial, after: after}
	go f.serve()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return f, host, port, nil
}

// serve accepts local connections and pipes each one to the far side, for the
// life of the listener.
func (f *forwarder) serve() {
	for {
		local, err := f.listener.Accept()
		if err != nil {
			return // listener closed: we are shutting down
		}
		go f.pipe(local)
	}
}

// pipe carries one connection in both directions until BOTH are finished.
//
// It used to return on the first of the two, and the deferred closes then cut
// the other one off wherever it had got to. That is a truncated answer, not an
// error: the driver sees a connection that ended, and the rows it had not read
// yet are simply gone. A database holds both directions open for the life of a
// session, so the first to finish is normally the end of the whole thing and it
// looked correct; what it was, was a race nobody had lost yet.
//
// When the far side finishes, the local end is half-closed rather than closed,
// so the driver reads its EOF, finishes what it was doing, and hangs up in its
// own time. That is what "the answer is complete" means to a client.
func (f *forwarder) pipe(local net.Conn) {
	defer func() { _ = local.Close() }()
	through, err := f.dial(context.Background())
	if err != nil {
		return
	}
	defer func() { _ = through.Close() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(through, local)
		// The local end sent everything it is going to, so the far side is told
		// that and nothing more. Closing here would take the answer with it;
		// saying nothing leaves a far side that answers only once the request is
		// complete waiting for a request it already has in full. Both were
		// caught by the same test.
		halfClose(through)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(local, through)
		// The far side is done. Say so to the local end the way a socket does:
		// EOF on its read side, its write side still its own.
		halfClose(local)
	}()
	wg.Wait()
}

// halfClose ends the writing half of a connection and leaves the reading half
// alone, when the connection has such a thing. A TCP connection does; one
// carried over a websocket does not, and closing that entirely would be the
// truncation this exists to avoid, so it is left to the deferred close.
func halfClose(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
}

// Close stops accepting and drops whatever the dialler was built on, which ends
// every connection carried through it.
func (f *forwarder) Close() error {
	_ = f.listener.Close()
	if f.after != nil {
		return f.after()
	}
	return nil
}
