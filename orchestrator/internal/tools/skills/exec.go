package skills

// Running a skill's script on the person's own computer.
//
// The model asks to run one script of one skill. Whether that package had to be
// sent down the link first is not its business, and it never appears in the
// conversation: the handler asks the computer, and if the computer has never
// seen this VERSION it sends the whole package and asks again. One tool call,
// one answer, whichever of the two happened.
//
// WHY THE WHOLE PACKAGE. A script imports its siblings, opens a template out of
// assets/ and reads a reference beside it, and nothing here can know which. So
// the version goes down whole, which is kilobytes in practice and makes "it
// failed because a file next to it was not there" impossible rather than
// unlikely.
//
// WHY THE VERSION IS THE KEY. A version is immutable, so a new upload is a new
// directory on that computer and there is nothing to invalidate: the question
// "is what you have current" has no way to be answered wrongly. What the far
// end keeps is `.sag/skill/<handle>/<version_id>/`, and the id is what it
// answers about.
//
// WHAT IS NOT HERE. There is no policy and no approval, and that is a decision
// rather than an omission: a skill is a package an administrator imported and
// switched on, having been able to read every file of it on the skills screen.
// The authorisation IS the assignment. What guards the run instead is the far
// end supervising it: a deadline, a memory ceiling and a processor ceiling,
// applied to a child process that cannot outlive them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	mlink "flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/toolkit"
)

// ExecName is what the model calls.
const ExecName = "skill_exec"

// missing is what the far end answers when it has never seen a version.
//
// A STRUCTURED answer rather than a failure, because it is not one: nothing ran
// and nothing is wrong. It carries the version it is talking about so the reply
// cannot be mistaken for one about a different install.
const missingKind = "skill_missing"

// NewExec builds skill_exec over the skills this turn holds and the link to the
// person's computer.
func NewExec(lib Library, machines machine.Machines, allowed []int64) tool.Tool {
	return tool.Tool{Schema: ExecSchema(), Handle: execHandler(lib, machines, allowed)}
}

// ExecSchema is what the model is told about running a script.
func ExecSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill": map[string]any{
				"type":        "string",
				"description": "The skill whose script to run, by the handle your instructions list it under.",
			},
			"script": map[string]any{
				"type": "string",
				"description": "The script to run, by the exact path " + LoadName +
					" listed it under `more` (for example \"scripts/extract.py\").",
			},
			"args": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The arguments to pass, one per item, exactly as the script's own instructions describe them. Each is passed through untouched: there is no shell, so nothing is expanded and a pattern like *.pdf arrives as those four characters.",
			},
		},
		"required": []string{"skill", "script"},
	})
	return tool.Schema{
		Name:         ExecName,
		FriendlyName: "Run a skill's script",
		About: "Lets the assistant run one of the scripts a skill ships, on the person's own computer, " +
			"under a deadline and a memory ceiling.",
		Description: "Run one script of one of your skills. Read the skill first with " + LoadName +
			": its instructions say which script does what and what arguments it takes, and the index " +
			"names the scripts it ships. The script runs on the person's own computer, where their files " +
			"are, in the folder this conversation is working in. Arguments are passed through untouched " +
			"and there is no shell, so nothing is expanded for you. It comes back with what the script " +
			"printed and whether it succeeded.",
		InputSchema: input,
		// Internal, like the other two: reading and running what an
		// administrator imported and assigned is not a second thing to grant.
		Kind: tool.KindInternal,
		// The terminal's own classification, because it is the same act: code
		// somebody wrote, running on their computer, as them. Not
		// approval-gated, deliberately, since a skill is read and switched on
		// by an administrator before any agent can reach it, but classified
		// honestly so nothing downstream mistakes it for a read.
		Risk: tool.RiskDestructiveAction,
	}
}

func execHandler(lib Library, machines machine.Machines, allowed []int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			Skill  string   `json:"skill"`
			Script string   `json:"script"`
			Args   []string `json:"args"`
		}
		_ = json.Unmarshal(call.Args, &args)
		wanted := strings.TrimSpace(args.Skill)
		script := strings.TrimSpace(args.Script)
		switch {
		case len(allowed) == 0:
			return toolkit.Failed(noSkills)
		case wanted == "":
			return toolkit.BadArguments("name the skill in `skill`")
		case script == "":
			return toolkit.BadArguments("name the script to run in `script`")
		case call.DeviceID == "":
			// Said plainly. A skill's instructions are readable from anywhere,
			// but its scripts run where the person is sitting.
			return toolkit.Failed("a skill's scripts run in the chat application, on your own " +
				"computer, and this conversation is not in one")
		}

		held, err := assigned(ctx, lib, call.WorkspaceID, allowed)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("your skills could not be read: %s", err))
		}
		found := pick(held, wanted)
		if found == nil {
			return toolkit.BadArguments("you hold no skill by that name. Yours are: " +
				join(handles(held)))
		}
		if found.Status != model.SkillActive {
			return toolkit.Blocked(fmt.Sprintf("%q is switched off, so it cannot be used.", found.Name))
		}
		if found.ActiveVersionID == 0 {
			return toolkit.Failed(fmt.Sprintf("%q has no version in use, so it has no scripts.", found.Name))
		}

		files, err := lib.Files(ctx, call.WorkspaceID, found.ActiveVersionID)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("the skill's files could not be read: %s", err))
		}
		// The script must be one this version actually ships, checked HERE
		// rather than left to the far end. A path the package does not carry is
		// the model misreading an index, which is a correction it can act on,
		// and it must never reach a filesystem as a path to try.
		wantedFile := find(files, script)
		if wantedFile == nil {
			return toolkit.BadArguments(fmt.Sprintf("%q ships no script called %q. It ships: %s",
				found.Name, script, join(scriptPaths(files))))
		}
		if wantedFile.FileType != model.SkillFileScript {
			return toolkit.BadArguments(fmt.Sprintf(
				"%q is not a script, it is %s. The scripts are: %s",
				wantedFile.Path, wantedFile.FileType, join(scriptPaths(files))))
		}

		// A list, never null. `args` is optional in the schema, so a model
		// running a script that takes none simply leaves it out, and an absent
		// field unmarshals to a nil slice which marshals back as `null`. The
		// far end reads a sequence and refuses that outright, so every script
		// with no arguments failed with "that is not a script to run: invalid
		// type: null, expected a sequence", which reads as the model getting
		// the call wrong when the call was right.
		passed := args.Args
		if passed == nil {
			passed = []string{}
		}
		run := func() (link, error) {
			return askComputer(ctx, machines, call, machine.SkillRunName, map[string]any{
				"skill":   found.Name,
				"version": found.ActiveVersionID,
				"script":  wantedFile.Path,
				"args":    passed,
			})
		}

		answer, err := run()
		if err != nil {
			return reached(err)
		}
		if answer.missing() {
			// The computer has never seen this version. Put it there, then ask
			// again. Both invisible: the model asked to run a script and is
			// going to be told how the script went.
			//
			// ONCE, however many agents are asking. The first one through
			// installs and the rest wait for it (install.go).
			//
			// What that saves, measured on twenty agents and a package of 303
			// files: one push down the link instead of twenty, and 303 file
			// bodies read out of the database instead of 6060. The LISTING
			// above is read per agent either way, which is right: each one
			// needs it to check the script it was asked for is one this
			// version ships, and it carries no file contents.
			key := installKey{
				workspaceID: call.WorkspaceID,
				userID:      call.UserID,
				deviceID:    call.DeviceID,
				versionID:   found.ActiveVersionID,
			}
			err := gate.once(ctx, key, func() error {
				return install(ctx, lib, machines, call, found, files)
			})
			switch {
			case errors.Is(err, errStillInstalling):
				// The wait ran out. Ask anyway before giving up: the likeliest
				// reason to be here is an install that finished while something
				// else went slowly, and one extra question is cheaper than a
				// turn that failed for nothing.
				if answer, err = run(); err != nil {
					return reached(err)
				}
				if answer.missing() {
					return toolkit.Failed(fmt.Sprintf("%q is still being put on your computer. "+
						"Nothing is wrong with the skill: try again in a moment.", found.Name))
				}
			case err != nil:
				return toolkit.Failed(fmt.Sprintf("%q could not be put on your computer: %s",
					found.Name, err))
			default:
				if answer, err = run(); err != nil {
					return reached(err)
				}
				if answer.missing() {
					// It said it had the version, and then said it did not.
					// Not retried again: a loop between two ends that disagree
					// is worse than an answer somebody can read.
					return toolkit.Failed(fmt.Sprintf(
						"%q was sent to your computer and it still reports it as missing.",
						found.Name))
				}
			}
		}
		return answer.result()
	}
}

// install sends the whole version down the link.
func install(ctx context.Context, lib Library, machines machine.Machines, call tool.Call, s *model.Skill, files []*model.SkillFile) error {
	// The size is decided BEFORE anything is read, from the listing, which
	// carries it. Checking as the files were loaded meant a package over the
	// ceiling was read into memory in full and then refused, which is the cost
	// the ceiling exists to avoid.
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	if total > maxInstall {
		return fmt.Errorf("it is %d MB, over the %d MB a skill may carry to a computer",
			total>>20, maxInstall>>20)
	}

	carried := make([]map[string]any, 0, len(files))
	for _, f := range files {
		whole, err := lib.File(ctx, call.WorkspaceID, f.ID)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Path, err)
		}
		entry := map[string]any{"path": f.Path, "sha256": f.SHA256}
		if whole.Binary {
			// Bytes never reach JSON as themselves (model.SkillFile keeps them
			// out of it on purpose), so an asset is carried encoded and the far
			// end decodes it before the hash is checked.
			entry["base64"] = base64.StdEncoding.EncodeToString(whole.Bytes)
		} else {
			entry["text"] = whole.Text
		}
		carried = append(carried, entry)
	}

	answer, err := askComputer(ctx, machines, call, machine.SkillInstallName, map[string]any{
		"skill":   s.Name,
		"version": s.ActiveVersionID,
		"files":   carried,
	})
	if err != nil {
		return err
	}
	if answer.res.OK {
		return nil
	}
	return fmt.Errorf("%s", answer.res.Message)
}

// How much a skill may carry to somebody's computer.
//
// The import's own bound, deliberately the same number rather than one chosen
// here: anything the product accepted can be put on a computer, and a package
// refused at install time that an administrator had already imported and
// switched on would be a skill that exists and can never run.
//
// So this is not a transport limit. The link carries a stream per call and is
// ours on both ends; what the check buys is that a package over the bound is
// refused by name and size, which somebody can act on, rather than the store
// being read and the bytes encoded first.
const maxInstall = skill.MaxExpandedBytes

// --- talking to the computer -----------------------------------------------------

// link is one answer from the person's computer, with the machinery of reading
// it kept in one place.
type link struct {
	res     mlink.Result
	content map[string]any
}

// askComputer makes one link call and decodes what came back.
func askComputer(ctx context.Context, machines machine.Machines, call tool.Call, name string, args map[string]any) (link, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return link{}, fmt.Errorf("the call could not be written: %w", err)
	}
	res, err := machines.Call(ctx, call.WorkspaceID, call.UserID, call.DeviceID,
		name, raw, "Run a skill's script")
	if err != nil {
		return link{}, err
	}
	answered := link{res: res}
	if len(res.Content) > 0 {
		_ = json.Unmarshal(res.Content, &answered.content)
	}
	return answered, nil
}

// missing reports that the computer has never seen the version it was asked
// about. It is read off the KIND, not off the words: a message is for a person
// and changing one must not change what the code does.
func (l link) missing() bool { return !l.res.OK && l.res.Kind == missingKind }

// result is what the model is handed: what the script printed, either way.
//
// A script that exits non-zero is not a tool that failed. It ran, it said
// something, and what it said is the answer: an assistant that is told "the
// tool failed" reaches for another tool, where one told "it exited 2 and
// printed no such file" reads the message and fixes the argument.
func (l link) result() (tool.Result, error) {
	if l.res.OK {
		return toolkit.Success(l.res.Content)
	}
	switch l.res.Kind {
	case string(tool.ErrorBadArguments):
		return toolkit.BadArguments(l.res.Message)
	case string(tool.ErrorDenied):
		return toolkit.Blocked(l.res.Message)
	default:
		return toolkit.Failed(l.res.Message)
	}
}

// reached turns a link failure into something a person reads, the same three
// answers every machine tool gives (tools/machine.Dispatch).
func reached(err error) (tool.Result, error) {
	switch {
	case errors.Is(err, mlink.ErrNoMachine):
		return toolkit.Failed("the chat application is not connected on this computer")
	case errors.Is(err, mlink.ErrTimeout):
		return toolkit.Failed("your computer did not answer in time")
	default:
		return toolkit.Failed(fmt.Sprintf("your computer could not do that: %s", err))
	}
}

// scriptPaths is what a version actually ships to run, for a refusal that tells
// the model what it could have said instead.
func scriptPaths(files []*model.SkillFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f.FileType == model.SkillFileScript {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}
