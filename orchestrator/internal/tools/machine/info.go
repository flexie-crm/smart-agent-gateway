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
	return speaks
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
	default:
		return 0
	}
}
