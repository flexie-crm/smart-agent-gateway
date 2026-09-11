//go:build windows

package api

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// addressInUse reports the one error worth waiting on: the port is taken.
//
// Windows says so with a WINSOCK error, WSAEADDRINUSE (10048), and not with the
// POSIX EADDRINUSE the rest of the code was written against. syscall.EADDRINUSE
// exists here too, carrying its POSIX number, so a comparison against it
// COMPILES AND NEVER MATCHES: the failure is not a build error and not a runtime
// error, it is a server that treats "the previous one is still draining" as
// fatal and gives up instantly on a port that was about to free, leaving nothing
// listening at all.
//
// Both are accepted rather than one per platform, because the cost of the extra
// comparison is nothing and the cost of guessing wrong is that silence.
func addressInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
