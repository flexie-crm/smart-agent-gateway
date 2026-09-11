package link

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A carried TCP connection, with the half that a websocket does not have.
//
// TCP lets one direction finish while the other keeps going: a client says
// "that is my whole request" and then reads the answer, and the far end knows
// the request is complete because it read an EOF. Protocols are built on it.
//
// A websocket has no such thing. Closing it ends both directions at once, so a
// connection carried over one either loses the signal (the far end waits for a
// request it has already been sent in full) or loses the answer (closing to
// signal the end cuts off what was still coming back). Neither is acceptable
// for a route that carries a database session.
//
// So the signal is carried: an EMPTY binary message means "I have finished
// sending". Both ends agree on it, it cannot be confused with data (a websocket
// message carries its own length, so an empty one is never a fragment of
// anything), and it costs two bytes on the wire. Read returns io.EOF when it
// arrives, exactly as a socket does, and the connection stays open the other
// way until the far end says the same or closes.

// eof is the marker. An empty binary message and nothing else.
var eof = []byte{}

// stream is one carried connection as an ordinary net.Conn, so everything above
// it (a database driver, an SSH handshake) is unchanged.
type stream struct {
	sock *websocket.Conn
	// ctx bounds the whole connection; cancel ends it and releases the HTTP
	// handler that is holding the socket open.
	ctx    context.Context
	cancel context.CancelFunc

	// reading holds what is left of the current message, since a caller reads
	// what it asks for rather than what a message happens to contain.
	readMu    sync.Mutex
	remainder []byte
	readEOF   bool

	// writeMu keeps frames whole: two writers on one websocket interleave into
	// a corrupt stream rather than an error.
	writeMu  sync.Mutex
	writeEnd bool

	closing sync.Once
	// onClose lets whoever is counting this connection stop counting it. Set
	// when the connection is handed over, and called exactly once.
	onClose func()
}

func newStream(ctx context.Context, sock *websocket.Conn) *stream {
	ctx, cancel := context.WithCancel(ctx)
	// No size limit on a carried connection.
	//
	// The library's default is 32 KB per message and a message over it does not
	// truncate, it KILLS the connection ("message too big"). What a carried
	// connection contains is a database protocol chopped into whatever sizes the
	// other end chose, so the limit is not a safety rail here, it is a cap on
	// how big a buffer the other half is allowed to use. websocket.NetConn does
	// exactly this for the same reason; this stream replaced it and inherited
	// the default instead.
	//
	// Found by the first test that ran the real client, which reads in 64 KB
	// buffers where the stand-in used less. Nothing in Go could have caught it.
	sock.SetReadLimit(-1)
	return &stream{sock: sock, ctx: ctx, cancel: cancel}
}

// Read fills p from the carried connection, returning io.EOF when the far end
// has finished sending.
func (s *stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for len(s.remainder) == 0 {
		if s.readEOF {
			return 0, io.EOF
		}
		kind, data, err := s.sock.Read(s.ctx)
		if err != nil {
			s.readEOF = true
			if errors.Is(err, context.Canceled) || websocket.CloseStatus(err) != -1 {
				return 0, io.EOF
			}
			return 0, err
		}
		if kind != websocket.MessageBinary {
			continue
		}
		if len(data) == 0 {
			// The far end has finished sending. Not a closed connection: this
			// one may still be answering.
			s.readEOF = true
			return 0, io.EOF
		}
		s.remainder = data
	}
	n := copy(p, s.remainder)
	s.remainder = s.remainder[n:]
	return n, nil
}

func (s *stream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeEnd {
		return 0, net.ErrClosed
	}
	if err := s.sock.Write(s.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite says "that is everything I am sending" and leaves the reading half
// open, which is what makes a request-then-answer protocol work over this.
func (s *stream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeEnd {
		return nil
	}
	s.writeEnd = true
	return s.sock.Write(s.ctx, websocket.MessageBinary, eof)
}

// Close ends the connection both ways.
func (s *stream) Close() error {
	var err error
	s.closing.Do(func() {
		err = s.sock.Close(websocket.StatusNormalClosure, "")
		s.cancel()
		if s.onClose != nil {
			s.onClose()
		}
	})
	return err
}

// Ping asks the far end whether it is still there, and waits for the answer.
//
// NOT used to decide whether a call is alive, and the reason is worth keeping:
// the application does not poll a call's socket while it is running the tool,
// so a ping in the middle of a command is answered by nobody. A ping proves the
// far end is POLLING, which is a different question from whether it is there.
// Presence is read from the registry instead (call.go).
//
// Safe beside a read or a write: a ping is a control frame, and the library
// serialises those separately from the data this stream carries.
func (s *stream) Ping(ctx context.Context) error {
	return s.sock.Ping(ctx)
}

func (s *stream) LocalAddr() net.Addr  { return carried{} }
func (s *stream) RemoteAddr() net.Addr { return carried{} }

// Deadlines are not offered, and saying so is better than accepting one and
// ignoring it: a caller that sets a read deadline to bound a wait would sit
// there forever believing it had. What bounds a carried connection is the
// context it was opened with and the idle limit above it.
func (s *stream) SetDeadline(time.Time) error      { return errNoDeadline }
func (s *stream) SetReadDeadline(time.Time) error  { return errNoDeadline }
func (s *stream) SetWriteDeadline(time.Time) error { return errNoDeadline }

var errNoDeadline = errors.New("a carried connection has no deadlines of its own")

// carried names the far side for anything that prints an address. It is not a
// socket on this machine and pretending otherwise would put a misleading
// address in a log.
type carried struct{}

func (carried) Network() string { return "link" }
func (carried) String() string  { return "carried" }
