//go:build !windows

package supervise

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// HideConsole does nothing here. It is Windows that gives a console program
// with no console to inherit a terminal window of its own; on this platform a
// child started by a windowed application simply has no terminal, which is
// what we want and what already happens.
func HideConsole(*exec.Cmd) {}

// Running reports whether a process id still belongs to a running process.
//
// Signal 0 asks the question without sending anything, which is what it is for.
func Running(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// EndByID ends a process this program did not start.
//
// It exists for one caller: a server left behind by a previous run, found by
// the pid file it wrote and holding the data directory the next start needs.
// Anything we started ourselves is a Process and goes through Stop and Kill.
//
// How to ask that process politely is deliberately NOT here, because only its
// owner knows: a signal reaches a database on this platform and nothing reaches
// one on Windows, where the request is a statement over a connection instead.
func EndByID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("supervise: find process %d: %w", pid, err)
	}
	return p.Kill()
}
