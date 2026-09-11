package datasource

import (
	"context"
	"fmt"
	"net"
)

// Reaching a database through the chat application.
//
// The gateway runs on a server. A customer's older systems often do not: an
// accounts package on a machine in an office, a database on a network only the
// people who work there can see. There is no address the gateway can be given
// for those, and there does not need to be, because the chat application is
// already installed on a computer that can see them and is already connected to
// us.
//
// So the connection is carried through it. What that costs here is a dial
// function: the forwarder does the rest, and MySQL, PostgreSQL and every driver
// added later work unchanged, because all any of them ever sees is a local port.

// Reach carries a connection to somewhere this server cannot go. It is supplied
// by the caller rather than configured, because it is not a setting: it is a
// live capability that belongs to whoever is asking (internal/link).
//
// Excluded from JSON deliberately: a Config is stored, and this is a function.
type Reach struct {
	// Dial opens one connection to host:port from the far side.
	Dial func(ctx context.Context, host string, port int) (net.Conn, error)
	// Describe names the far side for an error a person will read ("the chat
	// application"), so a failure says where it happened.
	Describe string
}

// openReach starts a local forwarder that carries every connection through the
// caller's Reach, and returns the local address a driver should dial instead.
func openReach(r Reach, host string, port int) (*forwarder, string, int, error) {
	dial := func(ctx context.Context) (net.Conn, error) {
		conn, err := r.Dial(ctx, host, port)
		if err != nil {
			return nil, fmt.Errorf("%s could not open a connection to %s: %w",
				r.Describe, net.JoinHostPort(host, fmt.Sprint(port)), err)
		}
		return conn, nil
	}
	// Probed, for the same reason the bastion is: the driver's own failure would
	// be a reset local socket, which names neither the machine nor the reason.
	// Here it also answers the commonest case of all, which is that nobody is
	// running the chat application at the other end.
	return forward(dial, true, nil)
}
