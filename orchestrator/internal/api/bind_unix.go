//go:build !windows

package api

import (
	"errors"
	"syscall"
)

// addressInUse reports the one error worth waiting on: the port is taken.
//
// Split per platform because the number is not the same on both, and getting it
// wrong is silent: an unrecognised error is treated as fatal and the server
// gives up on a port that was about to free.
func addressInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
