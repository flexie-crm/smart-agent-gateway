//go:build windows

package supervise

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsole starts a child without giving it a console window.
//
// Every process here is a console program, and Windows gives a console program
// with no console to inherit A NEW ONE: a black terminal window beside the
// application, titled with the path to the binary, staying for as long as the
// process runs. The desktop application is windowed and has no console, so
// without this the person gets one window per child.
//
// It is not only ugly. A console window is in quick-edit mode by default, so a
// click anywhere inside it SUSPENDS the process that owns it, freezing the
// database or the node until somebody presses a key, with nothing anywhere
// saying that is what happened.
//
// Nothing is lost: every child's output is already redirected to a file,
// because an inherited stdout is not a pipe anything is reading when an
// application is launched from an icon.
func HideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NO_WINDOW. Not HideWindow, which allocates the console anyway and
	// only asks for it not to be shown, and which flashes on the way past.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// stillActive is what GetExitCodeProcess reports for a process that has not
// exited. It is Win32's STILL_ACTIVE, which neither syscall nor x/sys/windows
// exports under that name: it is an alias for the NTSTATUS STATUS_PENDING, and
// reaching for it through that is a conversion that says less than the number
// with this sentence beside it.
const stillActive = 259

// Running reports whether a process id still belongs to a running process.
//
// The obvious port of the unix version is os.FindProcess followed by a signal 0,
// and it is worse than having no check at all: os.Process.Signal on Windows
// refuses every signal but Kill, so it returns an error for a process that is
// running perfectly well, and a caller reading that as "gone" walks straight
// past the process it was looking for. That is not theoretical. It is the
// database from a previous run still holding the data directory, which then
// fails the next start with "Can't lock aria control file" and stays broken
// until somebody finds it in Task Manager.
//
// So the question is put to the kernel instead: open the process and read its
// exit code, which reads STILL_ACTIVE for as long as it runs.
func Running(pid int) bool {
	// The limited right, because this is a question and not an intention. It is
	// granted where the full query right is not, and everything read here is
	// available through it.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// No such process, or one this account may not look at. Either way it is
		// not a process of ours that we are about to have to stop.
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// EndByID ends a process this program did not start.
//
// It exists for one caller: a server left behind by a previous run, found by
// the pid file it wrote and holding the data directory the next start needs.
// Anything we started ourselves is a Process and goes through Stop and Kill.
//
// The handle is opened for termination here rather than through os.FindProcess,
// which asks for read and query rights only: killing through one of those fails
// with access denied whoever owns the process, because the right is checked
// against the handle and not against the account.
//
// How to ask that process politely is deliberately NOT here, because only its
// owner knows: nothing reaches a database on this platform except a statement
// over a connection, which is what localdb sends before it comes to this.
func EndByID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("supervise: open process %d to end it: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("supervise: end process %d: %w", pid, err)
	}
	return nil
}
