package machine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// A terminal on the person's own computer.
//
// The same thing the SSH tool gives on a server somebody configured, pointed at
// the machine the person is sitting at. It shares that tool's policy, because
// "may this command run" is one question with one answer wherever it is asked,
// and two implementations would mean two sets of rules to learn and two places
// for a way round them to appear.
//
// What it may run is the administrator's, written once in the tool's settings
// and enforced HERE, by parsing: every command in a pipeline, a subshell, a
// substitution. What folder it runs in is the person's, chosen on their own
// computer, because nothing on this side knows what is on their disk.

// TerminalName is the terminal, named once: the loadout finds its row by this
// to read the policy an administrator wrote.
const TerminalName = "terminal"

func terminalSchema() tool.Schema {
	return tool.Schema{
		Name:         TerminalName,
		FriendlyName: "Terminal",
		Kind:         tool.KindBuiltin,
		// What a person sees when they open one of these calls: where it ran,
		// what was run, and what it printed. The exit code and an empty stderr
		// are for the model to read; the row already says whether it worked,
		// and a call that FAILED shows all of it anyway.
		Shown: tool.Display{
			Where: []string{"directory"},
			Sent: []tool.Shown{
				tool.Value("session"), tool.Command("command"), tool.Value("input"),
				tool.Value("wait"), tool.Value("stop"), tool.Value("status"),
			},
			Answered: []tool.Shown{
				tool.Text("output"), tool.Value("running_command"), tool.Value("running_for_seconds"),
				tool.Value("stopped"), tool.Value("terminals"), tool.Value("note"),
			},
		},
		// Every command is a command: this is the person's own machine, and what
		// narrows it is the policy, not a guess about what a given line does.
		Risk:             tool.RiskDestructiveAction,
		RequiresApproval: false,
		// SHORT, and the depth is one tool_guide away.
		//
		// This description was 1,690 characters, about 420 tokens, and it was
		// sent on every turn of every conversation whether or not anybody was
		// going to run a command. All of it is in the guide below, which is
		// fetched when it is wanted: what belongs here is what the tool is and
		// the one thing a caller must know before reaching for the guide.
		Description: "Runs a command in a terminal on the person's own computer. It REMEMBERS between " +
			"calls (folder, exported variables), a conversation may have several by name, and slow or " +
			"interactive commands use wait, input and stop. Read its guide first.",
		Settings: []tool.Section{{
			Title: "Commands",
			Hint: "What the assistant may run on somebody's own computer.\n\n" +
				"- A denylist runs anything except what you list, which is the tool people want: it can work, " +
				"with the damage it cannot undo taken away.\n" +
				"- An allowlist is stronger: only what you list runs, and everything else is refused.\n" +
				"- One command per line. Just the program (git) means it with any arguments; a longer line " +
				"(git log) means only that. A line starting with # is a note to yourself.\n\n" +
				"This tool arrives with a denylist of what cannot be undone, which is a starting point and " +
				"not a rule: take out what you need, add what you know about your own machine.\n\n" +
				"Every command is READ rather than matched: each one in a pipeline, a subshell or a " +
				"substitution is checked, so a rule cannot be walked around by writing the line differently.\n\n" +
				"What it does not stop is anything that runs commands of its own: a shell started on purpose " +
				"(sh -c \"...\"), an interpreter, a script already on the disk. This stops a mistake and the " +
				"obvious ways round a list of names. What really bounds it is that everything runs as the " +
				"person using it, on their own computer, in the one folder they chose.",
			Fields: []tool.Field{
				{
					Key: "policy.mode", Label: "Rule", Type: tool.FieldSelect, Required: true,
					Options: []tool.Option{
						tool.Choice(string(cmdpolicy.PolicyDenylist), "Every command except the ones I list"),
						tool.Choice(string(cmdpolicy.PolicyAllowlist), "Only the commands I list"),
					},
					Default: string(cmdpolicy.PolicyDenylist), Span: 3,
				},
				{
					Key: "policy.sudo", Label: "Running as another user", Type: tool.FieldSelect,
					Options: []tool.Option{
						tool.Choice(cmdpolicy.SudoDeny, "Refused"),
						tool.Choice(cmdpolicy.SudoAllow, "Allowed"),
					},
					Default: cmdpolicy.SudoDeny, Span: 3,
				},
				{
					Key: "policy.allowed", Label: "Commands to allow", Type: tool.FieldTextarea,
					Help: "Used when the rule is an allowlist. Everything not listed here is refused.",
				},
				{
					Key: "policy.denied", Label: "Commands to refuse", Type: tool.FieldTextarea,
					Help: "Used when the rule is a denylist. Listing rm also refuses /bin/rm, sudo rm, and rm " +
						"inside a longer command. A line starting with # is a note to yourself.",
					// What the tool ARRIVES with, so a terminal switched on for
					// the first time already refuses what cannot be undone. The
					// same list the server tool ships, because it is the same
					// question (cmdpolicy.DefaultDenied), and a starting point
					// an administrator owns from then on rather than a rule.
					Default: cmdpolicy.DefaultDenied,
				},
			},
		}},
		// What the form cannot say: an allowlist with nothing in it is a
		// terminal that runs nothing. It saved, said so, and then refused every
		// command with a message about an administrator who had in fact just
		// been one.
		CheckSettings: checkTerminalSettings,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "The command line to run, as it would be typed. Leave it out to read more of something already running, to answer it, or to stop it."
				},
				"directory": {
					"type": "string",
					"description": "Where to run it: a path relative to the working folder, or an absolute one. Optional; the terminal stays wherever it was left otherwise."
				},
				"input": {
					"type": "string",
					"description": "What to type into the command that is still running, as a person would type it and press return. Send it on its own, without the command."
				},
				"wait": {
					"type": "integer",
					"description": "How many seconds to wait for it to finish, up to 600. A ceiling, not a delay: something that finishes sooner answers sooner. Set it to what you are running: a quick check needs a few seconds, an install or a build needs a hundred or more, something you expect to stop and ask needs only a few."
				},
				"stop": {
					"type": "boolean",
					"description": "Ends what is running in this terminal. The folder and the exported variables are kept."
				},
				"session": {
					"type": "string",
					"description": "Which terminal to use. A conversation may have several, each with its own folder, its own variables and its own one-thing-at-a-time, so a build and a test run happen at the same time instead of queueing. Name them for what they do (\"build\", \"tests\"). Left out, it is the one called main."
				},
				"status": {
					"type": "boolean",
					"description": "Report every terminal in this conversation and what it is doing, instead of running anything. Ask when several are going and you need to know which to wait for."
				}
			},
			"additionalProperties": false
		}`),
	}
}

// terminalArgs is what the model asked for.
type terminalArgs struct {
	Command   string `json:"command"`
	Directory string `json:"directory"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
	Status    bool   `json:"status"`
}

// checkTerminal is the gate that runs HERE, before anything is sent.
//
// The policy is the administrator's and belongs where they wrote it. The
// machine stays a dumb executor: it does what it is told, and what it is told
// has already been read.
func checkTerminal(policy cmdpolicy.Policy, args json.RawMessage) (json.RawMessage, string) {
	var asked terminalArgs
	if err := json.Unmarshal(args, &asked); err != nil {
		return nil, "the arguments could not be read"
	}
	// Ending something is always allowed. A person watching a runaway command
	// must never be told the policy will not let them stop it. Asking what is
	// going on runs nothing at all.
	if asked.Stop || asked.Status {
		return args, ""
	}
	// There are two rules, and this tool knows about neither.
	//
	// A special case for an empty ALLOWLIST used to live here, which was wrong
	// twice over. It reached into one of the two modes and hard-coded what that
	// mode means, so a terminal held a rule the policy owns and the denylist had
	// no matching half; and the message it gave named a cause it could not know
	// ("no commands have been permitted for this tool YET. An administrator
	// chooses them in its settings"), which sent people to configure a tool they
	// had already configured. Both are cmdpolicy's business: it is the thing
	// that knows what a mode means, and it answers for both of them.
	ready := policy.WithDefaults()
	// Typing into something already running is checked too, and conservatively.
	//
	// What is running could be a shell, in which case a line typed at it IS a
	// command, or it could be anything else, whose language nothing here can
	// parse. So a line is refused when it names a program the policy denies,
	// wherever in the line it appears. It stops the obvious ways to reach a
	// denied program through a program's own escape (!rm, :!rm) and it stops
	// nothing that has been written to a file and run. The same reading the
	// server tool applies, for the same reason.
	if asked.Input != "" {
		if reason := ready.CheckInput(asked.Input, false); reason != "" {
			return nil, reason
		}
		return args, ""
	}

	// No command and no input: this is somebody asking for more of what is
	// already running. There is nothing to check, and the machine will say if
	// there is nothing to read.
	if strings.TrimSpace(asked.Command) == "" {
		return args, ""
	}
	if reason := ready.Check(asked.Command); reason != "" {
		return nil, reason
	}
	return args, ""
}

// checkTerminalSettings refuses a policy the terminal could not enforce.
//
// The same call the SSH tool makes on its own settings, for the same reason and
// with the same words: these are one policy asked in two places, and a rule that
// is unenforceable on a server is unenforceable on a laptop.
func checkTerminalSettings(config json.RawMessage) error {
	var stored struct {
		Policy cmdpolicy.Policy `json:"policy"`
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &stored); err != nil {
			return errors.New("these settings could not be read")
		}
	}
	return stored.Policy.WithDefaults().Validate()
}

// terminalHandler is the dispatcher with the policy in front of it.
func terminalHandler(machines Machines, policy cmdpolicy.Policy) tool.Handler {
	schema := terminalSchema()
	run := Dispatch(machines, schema, schema.Name)
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		checked, reason := checkTerminal(policy, call.Args)
		if reason != "" {
			// A refused command is not a failure of the machine, and the
			// assistant is told what to do about it: report it, rather than
			// look for another way to the same place.
			return toolkit.Blocked(reason)
		}
		call.Args = checked
		return run(ctx, call)
	}
}

// terminalVersion is which shape of arguments this gateway speaks. THREE:
// TWO added the memory (`input`, `wait`, `stop`), THREE added more than one
// terminal per conversation (`session`, `status`). An application built for an
// older shape is not offered the tool rather than being offered it and failing
// on arguments it has never heard of.
const terminalVersion = 3

// terminalGuide is what tool_guide answers with. It teaches the two things the
// short description cannot carry: the sequence a long or interactive command
// takes, and the mistakes that come from assuming this is a fresh shell.
var terminalGuide = json.RawMessage(`{
	"summary": "terminal runs a command on the person's own computer, in a terminal that REMEMBERS between calls. One terminal per conversation. A call answers with {output, running, running_command, exit_code, directory}.",
	"parameters": {
		"command": "The command line, as it would be typed. Leave it out when reading more of something already running, answering it, or stopping it.",
		"directory": "Where to run this one: relative to the working folder, or absolute. Optional; without it the terminal stays where it was left.",
		"input": "What to type into the command that is still running. Send it ALONE, without a command.",
		"wait": "Seconds to wait for it to finish, up to 600. A ceiling, not a delay: something quicker answers quicker.",
		"stop": "Ends what is running. The folder and the exported variables are kept."
	},
	"several_at_once": [
		"A conversation may have more than one terminal. session names it: \"build\", \"tests\", \"server\".",
		"Each has its own folder, its own exported variables, and its own one-thing-at-a-time. A slow one does not hold up the others.",
		"No session is the one called main, which is what you get if you never think about this.",
		"status reports every terminal in the conversation and what each is doing, which is the question to ask when several are going."
	],
	"what_it_remembers": [
		"cd moves the terminal, and the next command starts where the last one left it.",
		"export NAME=value is there for later commands. A plain NAME=value is NOT: export what later commands need.",
		"An alias or a shell function does not survive: it belongs to a shell that is not held open. Write a script if you need one.",
		"Each conversation has its own terminal. Nothing leaks between conversations."
	],
	"a_command_that_takes_a_while": [
		"Set wait to what you are running: a check needs a few seconds, an install or a build a hundred or more.",
		"If it comes back with running: true, it is STILL GOING and you have what it printed so far.",
		"Call again with NO command and a fresh wait to get the rest. Repeat until running is false.",
		"Do not start it again because you saw no result. It is already running; starting it twice is the mistake this shape exists to prevent."
	],
	"a_command_that_asks_something": [
		"It comes back running: true with the question in output ('Password:', 'Continue? [y/N]').",
		"Answer with input and nothing else: {\"input\": \"y\"}.",
		"When it asks for what only the person has, a password or a code, ASK THEM and send exactly what they say. Never invent one.",
		"Better still, pass the flag that stops it asking (-y, --yes, --non-interactive) when the command has one."
	],
	"one_at_a_time": [
		"A conversation's terminal runs one command at a time. Starting another while one is going is refused, and says so.",
		"Wait for it, answer it, or stop it."
	],
	"reading_the_answer": {
		"output": "Everything it has printed since the last call, both its normal output and its errors, in the order they happened.",
		"running": "true means it has not finished. Ask again for the rest.",
		"running_command": "What is still going, when something is.",
		"exit_code": "How the last finished command ended. 0 is success; anything else is not, and the output says why.",
		"directory": "Where the terminal is now, which is where the next command starts."
	},
	"what_may_run": "An administrator decides, in this tool's settings, and every command is read rather than matched: each one in a pipeline, a subshell or a substitution is checked. A refusal is final. Report it rather than rewording the command to get past it.",
	"limits": [
		"wait is capped at 600 seconds per call, and you may call again as often as you need.",
		"One answer carries at most 96 KB of output, keeping the END, where a command says what went wrong.",
		"Output arriving while nobody is asking is kept up to two megabytes, again keeping the end."
	],
	"mistakes_to_avoid": [
		"Assuming a fresh shell: it is not, and the folder is wherever the last command left it. Check directory in the answer.",
		"Setting a variable without export and expecting the next command to see it.",
		"Running the same command again because the first call came back with running: true.",
		"Sending a command and input together: input goes on its own.",
		"Starting something that never finishes without a plan to stop it."
	]
}`)
