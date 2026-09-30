package machine

import (
	"encoding/json"

	"flexie.io/sag/internal/tool"
)

// What computer the person is using.
//
// The least interesting tool this package will ever have, and the first one
// deliberately: it reads nothing, changes nothing, and asks nobody's
// permission. What it is for is proving the whole path with nothing at stake.

func infoSchema() tool.Schema {
	return tool.Schema{
		Name:         "machine_info",
		FriendlyName: "About this computer",
		Kind:         tool.KindBuiltin,
		Risk:         tool.RiskReadOnly,
		Description: "Reports what computer the person is using: its operating system, " +
			"its processor architecture, and what it calls itself. Use it when somebody " +
			"asks about their own machine, or to check that this conversation can reach it " +
			"before offering to do something there. Takes no arguments.",
		About: "Answers what kind of computer the person is working on. It reads nothing " +
			"else and changes nothing, and it only works in the chat application, where " +
			"there is a computer to ask.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

// Versions is what this gateway speaks, tool by tool.
//
// The link negotiates per tool (KB/39), so anything standing in for a computer,
// a test or a fixture, has to declare these numbers or the tools are filtered
// out of the loadout before anything runs, and the assertion fails for a reason
// that has nothing to do with what it was asking about. Exported so a fixture
// ASKS instead of repeating: a hardcoded 1 went on meaning "the terminal" for
// exactly as long as the terminal's arguments did not change, and then meant a
// computer too old to be offered it.
func Versions() map[string]int {
	speaks := map[string]int{}
	for _, t := range Tools(nil) {
		speaks[t.Schema.Name] = versionOf(t.Schema.Name)
	}
	// And the clock, which is not a machine-ONLY tool (it answers from this
	// server's own when there is no computer to ask) but is one an application
	// is asked to run, so it belongs in the one table rather than carrying a
	// second number of its own somewhere else.
	speaks[CurrentTimeName] = versionOf(CurrentTimeName)
	// And the two a skill is run with. They are link calls the GATEWAY makes,
	// never abilities a model is offered: the model asks to run a script, and
	// whether the package had to be sent down first is not its business. They
	// are in this table for the reason the clock is: one place where a number
	// lives, so two of them cannot drift into disagreeing.
	speaks[SkillInstallName] = versionOf(SkillInstallName)
	speaks[SkillRunName] = versionOf(SkillRunName)
	return speaks
}

// The two calls that put a skill on the person's computer and run one of its
// scripts. Declared here beside every other thing an application is asked to
// do; what they MEAN is in internal/tools/skills.
const (
	SkillInstallName = "skill_install"
	SkillRunName     = "skill_run"
)

// Speaks reports whether this person's computer can run a link call of this
// name, at the version this gateway speaks.
//
// Offers answers the same question for the tools a model is offered; this
// answers it for a call the gateway makes on its own. Same rule either way: an
// application a release behind is simply not asked, so nothing fails in the
// middle of a conversation.
func Speaks(machines Machines, workspaceID, userID int64, deviceID, name string) bool {
	if machines == nil || deviceID == "" {
		return false
	}
	runs := machines.Runs(workspaceID, userID, deviceID)
	version, known := runs[name]
	return known && version == versionOf(name)
}

// VersionOf is which shape of a tool's arguments this gateway speaks, for the
// one tool that lives outside this package and is still run on the person's
// computer. One table, so two numbers cannot drift into disagreeing.
func VersionOf(name string) int { return versionOf(name) }

// CurrentTimeName is the clock, whose handler lives in its own package and
// whose version lives here with every other tool an application runs.
const CurrentTimeName = "current_time"

// versionOf is which version of a tool's arguments this gateway speaks.
//
// A tool's arguments never change shape under the same version: a different
// shape is a new version, and an application that speaks only the old one is
// not offered the tool rather than being offered it and failing. That rule is
// what makes two halves that ship separately safe to change.
func versionOf(name string) int {
	switch name {
	case "machine_info":
		return 1
	case CurrentTimeName:
		// Named here rather than imported, because the clock's package imports
		// this one: it asks the computer through Machines.
		return 1
	case SkillInstallName, SkillRunName:
		// Named here for the same reason, and the same number for both: they
		// are one feature and an application that speaks half of it can do
		// nothing useful with the half it has.
		return 1
	case TerminalName:
		return terminalVersion
	case ReadFileName:
		return readFileVersion
	case WriteFileName:
		return writeFileVersion
	case EditFileName:
		return editFileVersion
	case FindFilesName:
		return findFilesVersion
	case SearchFileName:
		return searchFileVersion
	case BrowserName:
		return browserVersion
	default:
		return 0
	}
}
