package app

import (
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// Computer is what a turn knows about the machine it may act on.
//
// Three facts that travel together and have one lifetime: which installation
// this is, the folder on it the person opened up, and what kind of machine it
// is. None of them can be worked out here. The gateway may be on another
// continent, and the same person may be signed in from a laptop and a desktop
// at once, so the request is the only thing that knows and it says.
//
// One value rather than three arguments, because they are only ever passed
// together and a run with nobody in front of it has to say so about all three
// at once.
type Computer struct {
	// DeviceID is which of the person's computers this turn came from. Empty
	// from a browser, and empty for a run nobody is watching.
	DeviceID string
	// Folder is the folder on that computer the person gave the assistant to
	// work in, or empty when they have given none.
	Folder string
	// Env is what kind of computer it is, as the application described itself.
	// Nil from a browser, from an application too old to say, and from any run
	// with no computer at all.
	Env *MachineEnv
}

// MachineEnv is the application's own account of the computer it runs on.
//
// Sent with every message rather than asked for, because it can change while a
// conversation is open (somebody installs a runtime and immediately asks for
// it) and because there is no second channel to ask on: the control socket's
// first message is its only unprompted one.
//
// Nothing here is trusted with a decision. It is shown to the model so it stops
// guessing, and every guard that matters, what may run, what may be read, is
// applied on this side against an administrator's settings.
type MachineEnv struct {
	OS                 string   `json:"os"`
	Arch               string   `json:"arch"`
	Name               string   `json:"name"`
	Shell              string   `json:"shell"`
	PathSeparator      string   `json:"path_separator"`
	CaseSensitivePaths bool     `json:"case_sensitive_paths"`
	LineEnding         string   `json:"line_ending"`
	Home               string   `json:"home"`
	Has                []string `json:"has"`
	Missing            []string `json:"missing"`
}

// hasMachineTool reports whether any of these is a tool that runs on the
// person's own computer.
//
// The condition for telling the model about their machine at all. Not "is a
// device linked": a laptop can be linked, declaring every tool, while an
// administrator has switched all of them off. Describing a computer nothing can
// touch invites the assistant to plan work it has no way to do, which is worse
// than saying nothing.
func hasMachineTool(schemas []tool.Schema) bool {
	for _, s := range schemas {
		if machine.Runs(s.Name) {
			return true
		}
	}
	return false
}
