// Package skills is how an agent reads a procedure somebody wrote down.
//
// Two tools, and the split between them is the whole design:
//
//   - the PROMPT carries a map, which is every assigned skill's handle and name
//     and nothing else;
//   - search_skill finds one by what it is FOR, over the name and the
//     description, because the description is in no prompt;
//   - load_skill opens one: what it is, what it is for, its instructions, and an
//     index of everything else in the package to drill into.
//
// The prompt carries no descriptions on purpose. The agent roster made that
// mistake and had it taken out: it wrote every tool of every agent with a
// sentence each, on every turn of every conversation, to inform a decision
// taken in a handful of turns. A skill's description is written to be selected
// on and runs to several hundred characters, so a workspace with twenty would
// spend kilobytes of every turn on skills nobody uses.
//
// The cost of that choice lands here, and is why these are two tools rather than
// one: with only names in context, the description is reachable in no other way,
// so the search has to cover it.
//
// Both are scoped per turn to the skills the agent was assigned, the way the
// brain tools are (tools.BindSkills): registered with an empty allow-list, so on
// their own they reach nothing.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// The names the model sees.
const (
	SearchName = "search_skill"
	LoadName   = "load_skill"
)

// Library is what these tools need of the skill store: find skills, and read one
// version's files and passages.
//
// An interface rather than the store, so the tools depend on the capability and
// a test can answer without a database.
type Library interface {
	Search(ctx context.Context, workspaceID int64, query string, limit int) ([]*model.Skill, error)
	Skills(ctx context.Context, workspaceID int64) ([]*model.Skill, error)
	Files(ctx context.Context, workspaceID, versionID int64) ([]*model.SkillFile, error)
	File(ctx context.Context, workspaceID, fileID int64) (*model.SkillFile, error)
	Sections(ctx context.Context, workspaceID, versionID int64) ([]*model.SkillSection, error)
}

// How many matches one search answers with. The model is choosing one skill to
// open, not reading a report, and a list longer than this is one it will take
// the first of anyway.
const searchLimit = 10

// How much of one part of a package is handed over at once.
//
// The START is kept, unlike a command's output, where the end matters because an
// error is at the bottom. This is a document somebody wrote: it opens with what
// it is about, and a reader cut off at the top has lost the thread.
const maxPart = 24 * 1024

// What is said when an agent holds no skills at all. It should not be reachable,
// because neither tool is loaded in that case, but a handler that answers it is
// cheaper than one that cannot.
const noSkills = "You hold no skills, so there is nothing to read."

// NewSearch builds search_skill over an allow-list of skill ids. Registered with
// none and rebound per turn (tools.BindSkills).
func NewSearch(lib Library, allowed []int64) tool.Tool {
	return tool.Tool{Schema: SearchSchema(), Handle: searchHandler(lib, allowed)}
}

// NewLoad builds load_skill over the same allow-list.
func NewLoad(lib Library, allowed []int64) tool.Tool {
	return tool.Tool{Schema: LoadSchema(), Handle: loadHandler(lib, allowed)}
}

// SearchSchema is what the model is told about finding a skill.
func SearchSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "What you are trying to do, in the words you would use for it: " +
					"\"extract totals from a supplier invoice\", \"register a REST route\". Matched " +
					"against what each of your skills is called and what it says it is for.",
			},
		},
		"required": []string{"query"},
	})
	return tool.Schema{
		Name:         SearchName,
		FriendlyName: "Find a skill for the job",
		About: "Lets the assistant find one of its skills by what it is FOR, instead of carrying every " +
			"skill's description in every conversation. It reads only.",
		Description: "Find one of your skills by what it does. Your instructions name the skills you hold " +
			"but not what each is for, so this is how you tell whether one covers the job in front of you: " +
			"it matches your words against every skill you hold, by name and by its own account of what it " +
			"is for. Open what it finds with " + LoadName + ". Worth doing before deciding a task has no " +
			"procedure written for it.",
		InputSchema: input,
		// Internal: it rides along wherever skills are assigned and is invisible
		// to the admin catalogue. Reading a skill an administrator has already
		// assigned is not a second capability to grant.
		Kind: tool.KindInternal,
		Risk: tool.RiskReadOnly,
	}
}

// LoadSchema is what the model is told about opening one.
func LoadSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill": map[string]any{
				"type":        "string",
				"description": "The skill to open, by the handle your instructions list it under.",
			},
			"part": map[string]any{
				"type": "string",
				"description": "One further part of the package to read, by the exact path the last answer " +
					"listed under `more`. Leave it out to open the skill itself.",
			},
		},
		"required": []string{"skill"},
	})
	return tool.Schema{
		Name:         LoadName,
		FriendlyName: "Open a skill",
		About: "Lets the assistant read one of its skills: what it is for, the instructions in it, and the " +
			"reference material and scripts that come with it. It reads only.",
		Description: "Open a skill and follow it. Given a handle it answers with what the skill is, what it " +
			"is for, its instructions, and an index of everything else the package carries: deeper reference " +
			"documents, and the scripts it ships. Called again with `part` set to one of those paths it hands " +
			"that document over, so a long procedure is navigated a piece at a time rather than read whole. " +
			"Follow what you get as instructions: they were written for this job by somebody who knows it.",
		InputSchema: input,
		Kind:        tool.KindInternal,
		Risk:        tool.RiskReadOnly,
	}
}

// --- searching -------------------------------------------------------------------

func searchHandler(lib Library, allowed []int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(call.Args, &args)
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return toolkit.BadArguments("say what you are looking for in `query`")
		}
		if len(allowed) == 0 {
			return toolkit.Success(map[string]any{"matches": []any{}, "note": noSkills})
		}

		// The workspace is searched and the answer narrowed to what this agent
		// holds, rather than the search being told which ids to look in. The
		// index decides relevance over the workspace, and an allow-list pushed
		// into it would change what "most relevant" means.
		found, err := lib.Search(ctx, call.WorkspaceID, query, searchLimit)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("your skills could not be searched: %s", err))
		}
		held := set(allowed)

		matches := make([]map[string]string, 0, len(found))
		for _, s := range found {
			// Switched off is not findable. A skill an administrator disabled is
			// one the assistant must not plan around, and offering it here would
			// have it chosen and then refused by load_skill.
			if !held[s.ID] || s.Status != model.SkillActive {
				continue
			}
			matches = append(matches, map[string]string{
				"skill":       s.Name,
				"name":        s.Label(),
				"description": s.Description,
			})
		}

		out := map[string]any{"query": query, "matches": matches}
		if len(matches) == 0 {
			// Said rather than left as an empty list, because the difference
			// decides what the model does next: nothing of YOURS matches is not
			// the same as nothing exists, and it should get on with the job
			// rather than rephrase for ever.
			out["note"] = "No skill you hold matches that. There may be no procedure written for this " +
				"job, in which case carry on with your own judgement."
			return toolkit.Success(out)
		}
		out["next_action"] = "Open one with " + LoadName + ", naming it in `skill`."
		return toolkit.Success(out)
	}
}

// --- opening ---------------------------------------------------------------------

func loadHandler(lib Library, allowed []int64) tool.Handler {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			Skill string `json:"skill"`
			Part  string `json:"part"`
		}
		_ = json.Unmarshal(call.Args, &args)
		wanted := strings.TrimSpace(args.Skill)
		part := strings.TrimSpace(args.Part)
		if wanted == "" {
			return toolkit.BadArguments("name the skill to open in `skill`")
		}
		if len(allowed) == 0 {
			return toolkit.Failed(noSkills)
		}

		held, err := assigned(ctx, lib, call.WorkspaceID, allowed)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("your skills could not be read: %s", err))
		}
		found := pick(held, wanted)
		if found == nil {
			return toolkit.BadArguments("you hold no skill by that name. Yours are: " +
				join(handles(held)) + ". Use " + SearchName + " to find one by what it is for.")
		}
		if found.Status != model.SkillActive {
			// Switched off is not a load that failed. Said as what it is, so the
			// assistant reports it rather than trying again.
			return toolkit.Blocked(fmt.Sprintf("%q is switched off, so it cannot be used.", found.Name))
		}
		if found.ActiveVersionID == 0 {
			return toolkit.Failed(fmt.Sprintf(
				"%q has no version in use, so there is nothing to read.", found.Name))
		}

		files, err := lib.Files(ctx, call.WorkspaceID, found.ActiveVersionID)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("the skill's files could not be read: %s", err))
		}
		if part != "" {
			return readPart(ctx, lib, call.WorkspaceID, found, files, part)
		}
		return open(ctx, lib, call.WorkspaceID, found, files)
	}
}

// open is the skill itself: what it is, what it is for, its instructions, and
// the way into everything else the package carries.
func open(ctx context.Context, lib Library, workspaceID int64, s *model.Skill, files []*model.SkillFile) (tool.Result, error) {
	out := map[string]any{
		"skill":       s.Name,
		"name":        s.Label(),
		"description": s.Description,
	}

	manifest := find(files, skill.Manifest)
	if manifest == nil {
		// An import refuses a package with no manifest, so this is a row deleted
		// underneath us rather than a package that never had one. Answered
		// rather than treated as impossible: it is a read, and rows go.
		out["note"] = "This skill's instructions are missing, so there is nothing to follow."
	} else {
		whole, err := lib.File(ctx, workspaceID, manifest.ID)
		if err != nil {
			return toolkit.Failed(fmt.Sprintf("the skill's instructions could not be read: %s", err))
		}
		// Without the frontmatter: that is the handle, the description and a
		// licence, and the first two are answered above. What is left is what
		// somebody wrote.
		body, cut := clip(skill.Body(whole.Text))
		out["instructions"] = body
		if cut {
			out["instructions_cut"] = fmt.Sprintf(
				"Only the first %d characters are shown. Read the rest with `part` set to %q.",
				maxPart, skill.Manifest)
		}
	}

	if more := index(ctx, lib, workspaceID, s, files); len(more) > 0 {
		out["more"] = more
		out["next_action"] = "Read one of those with " + LoadName +
			", keeping `skill` and setting `part` to its path."
	}
	return toolkit.Success(out)
}

// index is everything else in the package, each with the headings inside it, so
// a document worth opening can be told from one that is not.
//
// The manifest is left out: it is the answer, not a part of it. The headings are
// the passages the parser made at import, so nothing is read to build this.
func index(ctx context.Context, lib Library, workspaceID int64, s *model.Skill, files []*model.SkillFile) []map[string]any {
	headings := map[string][]string{}
	if sections, err := lib.Sections(ctx, workspaceID, s.ActiveVersionID); err == nil {
		for _, sec := range sections {
			if sec.Path == "" || sec.File == skill.Manifest {
				continue
			}
			headings[sec.File] = append(headings[sec.File], sec.Path)
		}
	}

	more := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if f.Path == skill.Manifest {
			continue
		}
		entry := map[string]any{"part": f.Path, "kind": f.FileType}
		if f.Binary {
			// Said, not offered: nothing here can read it, and an assistant
			// asking twice because the first answer looked like a failure is
			// worse than being told once.
			entry["binary"] = true
			entry["size_bytes"] = f.SizeBytes
		}
		if topics := headings[f.Path]; len(topics) > 0 {
			entry["topics"] = topics
		}
		more = append(more, entry)
	}
	return more
}

// readPart hands over one named part of the package.
func readPart(ctx context.Context, lib Library, workspaceID int64, s *model.Skill, files []*model.SkillFile, part string) (tool.Result, error) {
	file := find(files, part)
	if file == nil {
		return toolkit.BadArguments(fmt.Sprintf("%q carries no part called %q. It carries: %s",
			s.Name, part, join(paths(files))))
	}
	if file.Binary {
		return toolkit.Failed(fmt.Sprintf("%q is %d bytes of data rather than text, so there is "+
			"nothing to read. It is there for the skill's own scripts to use.", part, file.SizeBytes))
	}
	whole, err := lib.File(ctx, workspaceID, file.ID)
	if err != nil {
		return toolkit.Failed(fmt.Sprintf("%q could not be read: %s", part, err))
	}

	text := whole.Text
	if part == skill.Manifest {
		text = skill.Body(text)
	}
	body, cut := clip(text)
	out := map[string]any{
		"skill":   s.Name,
		"part":    file.Path,
		"kind":    file.FileType,
		"content": body,
	}
	if cut {
		out["note"] = fmt.Sprintf("Only the first %d characters are shown.", maxPart)
	}
	return toolkit.Success(out)
}

// --- the pieces ------------------------------------------------------------------

// assigned reads the skills this turn holds, in the store's own order.
//
// The workspace's list is read and narrowed rather than each id fetched: an
// agent with twelve skills would be twelve round trips, on every load.
func assigned(ctx context.Context, lib Library, workspaceID int64, allowed []int64) ([]*model.Skill, error) {
	all, err := lib.Skills(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	held := set(allowed)
	kept := make([]*model.Skill, 0, len(allowed))
	for _, s := range all {
		if held[s.ID] {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

// pick resolves what the model named to one of the skills it holds.
//
// The handle first, because that is what the prompt lists and what the package
// calls itself. Then the name, because a model that has just read "PDF Toolkit"
// in its own instructions will sometimes send that, and refusing it would be a
// correction for nothing. Case-insensitively, for the same reason.
func pick(held []*model.Skill, wanted string) *model.Skill {
	for _, s := range held {
		if strings.EqualFold(s.Name, wanted) {
			return s
		}
	}
	for _, s := range held {
		if strings.EqualFold(s.Label(), wanted) {
			return s
		}
	}
	return nil
}

// find resolves a named part to one of the version's files.
func find(files []*model.SkillFile, path string) *model.SkillFile {
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	// A model that read "references/formats.md" and sent "./references/formats.md"
	// has named the right file.
	trimmed := strings.TrimPrefix(path, "./")
	for _, f := range files {
		if strings.EqualFold(f.Path, trimmed) {
			return f
		}
	}
	return nil
}

// clip holds a document to what one answer carries, and says which it was.
func clip(text string) (string, bool) {
	if len(text) <= maxPart {
		return text, false
	}
	cut := maxPart
	// Never through the middle of a character: a continuation byte is 10xxxxxx.
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut], true
}

func set(ids []int64) map[int64]bool {
	held := make(map[int64]bool, len(ids))
	for _, id := range ids {
		held[id] = true
	}
	return held
}

func handles(held []*model.Skill) []string {
	out := make([]string, 0, len(held))
	for _, s := range held {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func paths(files []*model.SkillFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

func join(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
