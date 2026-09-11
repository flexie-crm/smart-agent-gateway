package machine

import (
	"encoding/json"

	"flexie.io/sag/internal/tool"
)

// The file tools: what the assistant may do with the files on somebody's own
// computer. Five, because they are five things an administrator grants
// separately. Reading a project is not writing to it, and searching it is
// neither; one tool with a `mode` argument would collapse three decisions into
// one grant, and the grant is where this is actually controlled.
//
// Nothing here decides whether a call may proceed. A tool is granted to an
// agent or it is not, and if the administrator put it in that agent's confirm
// set the person answers a card first: the same gate every other tool goes
// through, with no second one invented for files.
//
// The paths are the person's. A relative one is measured from the folder they
// chose in the application; an absolute one reaches what it names, because
// they could open that file themselves and these act as them (KB/39).
const (
	ReadFileName   = "read_file"
	WriteFileName  = "write_file"
	EditFileName   = "edit_file"
	FindFilesName  = "find_files"
	SearchFileName = "search_files"
)

// Each tool's argument version, matched against what the application says it
// speaks when the link opens. A different shape is a new number, and an
// application that speaks the old one is simply not offered the tool.
const (
	readFileVersion = 1
	// TWO for both: they grew a hash, and the editor grew a line range.
	writeFileVersion  = 2
	editFileVersion   = 2
	findFilesVersion  = 1
	searchFileVersion = 1
)

func readFileSchema() tool.Schema {
	return tool.Schema{
		Name:              ReadFileName,
		FriendlyName:      "Read a file",
		FriendlyNarration: "Reading a file",
		Kind:              tool.KindBuiltin,
		Risk:              tool.RiskReadOnly,
		// Short: the paging, the hash and the path rules are in its guide.
		Description: "Reads a text file on the person's own computer. A relative path is measured from " +
			"the folder they chose to work in, an absolute one reaches what it names. Long files come " +
			"back in parts (offset, limit), and every answer carries a hash to pass back as expect_hash " +
			"when you write or edit.",
		About: "Lets the assistant read files on the person's own computer, starting from the working " +
			"folder they chose there. It reads only text, and only what the person themselves can read.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "The file to read, relative to the working folder or absolute."},
				"offset": {"type": "integer", "description": "Optional. The first line to return, counting from 1."},
				"limit": {"type": "integer", "description": "Optional. How many lines to return."}
			},
			"required": ["path"]
		}`),
		Shown: tool.Display{
			Where:    []string{"path"},
			Sent:     []tool.Shown{tool.Value("offset"), tool.Value("limit")},
			Answered: []tool.Shown{tool.Text("content"), tool.Value("more"), tool.Value("hash")},
		},
	}
}

func writeFileSchema() tool.Schema {
	return tool.Schema{
		Name:              WriteFileName,
		FriendlyName:      "Write a file",
		FriendlyNarration: "Writing a file",
		Kind:              tool.KindBuiltin,
		// Destructive because it replaces what is there. Whether that asks the
		// person first is the administrator's setting on the agent, not this
		// tool's opinion of itself.
		Risk: tool.RiskDestructiveAction,
		// Short: the read-before-overwrite rule and its reason are in the guide.
		Description: "Writes a text file on the person's own computer, creating it or replacing what is " +
			"in it entirely. To change PART of one, read it and use edit_file. A file that already exists " +
			"must have been read in this conversation first, so nothing unseen is lost.",
		About: "Lets the assistant create files on the person's own computer and replace what is in " +
			"them. It writes as that person, wherever they can write.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "The file to write, relative to the working folder or absolute."},
				"content": {"type": "string", "description": "What the file should contain, in full."},
				"expect_hash": {"type": "string", "description": "The hash read_file gave for this file. Optional, and worth passing: the write is refused if the file changed since you read it."}
			},
			"required": ["path", "content"]
		}`),
		Guide: writeGuide,
		Shown: tool.Display{
			Where:    []string{"path"},
			Sent:     []tool.Shown{tool.Text("content")},
			Answered: []tool.Shown{tool.Value("created"), tool.Value("created_folder"), tool.Value("bytes"), tool.Value("hash")},
		},
	}
}

func editFileSchema() tool.Schema {
	return tool.Schema{
		Name:              EditFileName,
		FriendlyName:      "Edit a file",
		FriendlyNarration: "Editing a file",
		Kind:              tool.KindBuiltin,
		Risk:              tool.RiskDestructiveAction,
		// Short: the two ways to name the part, and their rules, are in the guide.
		Description: "Changes part of a text file on the person's own computer: replace exact text you " +
			"have read (find/replace), or replace a line range (start_line/end_line). Pass expect_hash " +
			"from read_file and the edit is refused if the file moved on while you were thinking.",
		About: "Lets the assistant change part of a file on the person's own computer, by replacing " +
			"text it has read with new text.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "The file to change, relative to the working folder or absolute."},
				"find": {"type": "string", "description": "The exact text to replace, copied from what you read. One of this or a line range."},
				"replace": {"type": "string", "description": "What to put in its place. Empty with a line range deletes those lines."},
				"all": {"type": "boolean", "description": "Replace every occurrence rather than the single one. Defaults to false."},
				"start_line": {"type": "integer", "description": "The first line to replace, counting from 1. Use this instead of find when the text is repeated or the whitespace is uncertain: read_file gives you the numbers."},
				"end_line": {"type": "integer", "description": "The last line to replace, included. Defaults to start_line."},
				"expect_hash": {"type": "string", "description": "The hash read_file gave for this file. Optional, and worth passing: the edit is refused if the file changed since you read it."}
			},
			"required": ["path"]
		}`),
		Guide: editGuide,
		Shown: tool.Display{
			Where: []string{"path"},
			Sent: []tool.Shown{
				tool.Text("find"), tool.Text("replace"), tool.Value("all"),
				tool.Value("start_line"), tool.Value("end_line"),
			},
			Answered: []tool.Shown{tool.Value("replacements"), tool.Value("hash"), tool.Value("lines")},
		},
	}
}

func findFilesSchema() tool.Schema {
	return tool.Schema{
		Name:              FindFilesName,
		FriendlyName:      "Find files",
		FriendlyNarration: "Looking for files",
		Kind:              tool.KindBuiltin,
		Risk:              tool.RiskReadOnly,
		Description: "Finds files by name on the person's own computer, by pattern (**/*.go, " +
			"src/**/test_*.py, README*). Looks in the folder they chose to work in unless you give " +
			"another, and skips what the project itself ignores.",
		About: "Lets the assistant find files by name on the person's own computer, inside the folder " +
			"they chose to work in.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "The name pattern, e.g. **/*.ts or src/**/*_test.go."},
				"folder": {"type": "string", "description": "Optional. Where to look, relative to the working folder or absolute. Defaults to the working folder."}
			},
			"required": ["pattern"]
		}`),
		Shown: tool.Display{
			Sent:     []tool.Shown{tool.Value("pattern"), tool.Value("folder")},
			Answered: []tool.Shown{tool.Value("files"), tool.Value("count"), tool.Value("more")},
		},
	}
}

func searchFilesSchema() tool.Schema {
	return tool.Schema{
		Name:              SearchFileName,
		FriendlyName:      "Search in files",
		FriendlyNarration: "Searching the files",
		Kind:              tool.KindBuiltin,
		Risk:              tool.RiskReadOnly,
		Description: "Searches inside files on the person's own computer for a regular expression, " +
			"answering each matching line with its file and line number. Narrow it with glob and " +
			"ignore_case. Skips what the project ignores and stops after a couple of hundred matches.",
		About: "Lets the assistant search inside the files on the person's own computer, in the folder " +
			"they chose to work in.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "A regular expression to look for."},
				"folder": {"type": "string", "description": "Optional. Where to search. Defaults to the working folder."},
				"glob": {"type": "string", "description": "Optional. Only search files matching this pattern, e.g. *.go."},
				"ignore_case": {"type": "boolean", "description": "Optional. Match regardless of case."}
			},
			"required": ["pattern"]
		}`),
		Shown: tool.Display{
			Sent:     []tool.Shown{tool.Value("pattern"), tool.Value("glob"), tool.Value("folder"), tool.Value("ignore_case")},
			Answered: []tool.Shown{tool.Value("matches"), tool.Value("count"), tool.Value("more")},
		},
	}
}

// fileTools is the five, in the order somebody meets them: find something,
// read it, change it.
func fileTools(machines Machines) []tool.Tool {
	schemas := []tool.Schema{
		readFileSchema(),
		writeFileSchema(),
		editFileSchema(),
		findFilesSchema(),
		searchFilesSchema(),
	}
	tools := make([]tool.Tool, 0, len(schemas))
	for _, schema := range schemas {
		tools = append(tools, tool.Tool{Schema: schema, Handle: Dispatch(machines, schema, schema.Name)})
	}
	return tools
}

// The two guides worth writing. read, find and search do what their names say
// and their descriptions are enough; these two have a RULE in them, and a rule
// a model meets as a refusal costs a turn to learn.
var writeGuide = json.RawMessage(`{
	"summary": "write_file writes a whole text file on the person's own computer. It answers {path, created, created_folder, bytes}.",
	"the_rule_that_will_refuse_you": "A file that ALREADY exists may only be written after you have read it in THIS conversation. Rewriting a file you have not seen loses whatever was in it. Read it first, or use edit_file to change part of it.",
	"parameters": {
		"path": "Relative to the person's working folder, or absolute. An absolute path reaches what it names.",
		"content": "The WHOLE file. This replaces what is there; it does not append."
	},
	"when_to_use_which": [
		"Creating a file: write_file. Nothing to read first, nothing to lose.",
		"Changing part of a file: edit_file. It needs no whole-file rewrite and cannot lose the rest.",
		"Rewriting a file on purpose: read_file, then write_file."
	],
	"notes": [
		"A folder that is not there yet is made, so a new part of a project can be started in one call. The answer says when one was made.",
		"created is true when the file did not exist before, false when it was replaced.",
		"One call writes at most 4 MB. Write something larger in parts."
	]
}`)

var editGuide = json.RawMessage(`{
	"summary": "edit_file changes part of a file on the person's own computer, by exact text OR by line range. It answers {path, replacements, hash, lines}.",
	"two_ways_to_say_which_part": {
		"by text": "find is the exact text, copied from what you read. Include enough of the surrounding lines to make it unique.",
		"by line": "start_line and end_line replace those lines with replace, whatever is in them. Nothing about whitespace can make it ambiguous, and read_file gives you the numbers. An empty replace deletes the lines.",
		"never both": "Giving find AND start_line is refused: they are two ways of saying the same thing and the tool will not guess which you meant."
	},
	"when_to_use_which": [
		"Changing one distinctive line or block: by text. It survives the file moving around.",
		"The text repeats, or the whitespace is uncertain, or you are replacing a whole block you just read: by line.",
		"Deleting lines: by line, with an empty replace.",
		"Rewriting most of a file: write_file, with expect_hash."
	],
	"the_hash": [
		"read_file answers with a hash: which version of the file you read.",
		"Pass it as expect_hash and the edit is refused if the file changed while you were thinking, instead of landing on somebody else's work.",
		"Every edit and write answers with the file's NEW hash, so a second change can follow without reading the file again.",
		"A refusal names both hashes. Read the file again and write the change against what it says now."
	],
	"line_endings": "Handled. A file written on Windows ends its lines with a character that does not print, and text copied from it still matches; what is written back keeps the file's own endings, so the file never becomes a mixture.",
	"what_it_refuses_and_why": {
		"that text is not there": "Not there, character for character. Read the file again; whitespace is usually the difference, or use a line range instead.",
		"appears N times": "It could mean any of them, so it refuses rather than guessing. Include more of the lines around it, set all, or use a line range.",
		"has changed since": "The file is not the one you read. Read it again.",
		"would leave the file exactly as it is": "The replacement is what is already there."
	},
	"notes": [
		"A refused edit changes nothing.",
		"Reading a file counts as having seen it, and so does editing it: a write after an edit is allowed.",
		"all replaces every occurrence and reports how many; without it, exactly one."
	]
}`)
