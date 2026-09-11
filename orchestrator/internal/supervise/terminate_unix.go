//go:build !windows

package supervise

import (
	"os"
	"syscall"
)

// Terminate asks the server to close cleanly.
//
// SIGTERM is what mariadbd reads as "shut down": it finishes what it is doing,
// flushes, and logs "Shutdown complete". The difference that makes is the next
// start, which either opens a consistent directory or runs InnoDB recovery over
// one that was cut off mid-write.
func Terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
