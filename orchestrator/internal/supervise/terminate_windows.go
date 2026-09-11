//go:build windows

package supervise

import "os"

// Terminate is a hard stop here, because Windows has no SIGTERM: os.Process.Signal
// refuses anything but Kill.
//
// What that costs depends entirely on the child. The database used to be stopped
// through this and it was the wrong answer for it: cut off mid-write, every start
// paid for the last one with InnoDB recovery. It no longer is. A database is asked
// over a connection instead (localdb: SHUTDOWN is a statement, and a statement
// always reaches a server that is answering), so the polite path now exists on
// this platform too and the shutdown-complete assertion in localdb's suite holds
// here as it does on macOS.
//
// What is left using this is the inference node, which holds nothing durable: it
// keeps weights on disk that it only ever reads, so ending it costs the request in
// flight and nothing else. For that child a kill IS the honest shutdown, not a
// stand-in for one.
func Terminate(p *os.Process) error { return p.Kill() }
