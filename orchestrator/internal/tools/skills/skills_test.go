package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/toolkit"
)

// A library that answers from memory, so the two tools are driven for real
// without a database: what is under test is what they DO with an answer, and
// the store has its own suite for whether the answer is right.
type library struct {
	// LOCKED, because the fleet tests drive this from twenty goroutines at
	// once. Without it the counters lose writes, and what that looks like is a
	// test failing with "19 scripts ran, want 20": a bug in the fixture wearing
	// the costume of a bug in the code.
	mu       sync.Mutex
	all      []*model.Skill
	files    map[int64][]*model.SkillFile
	sections map[int64][]*model.SkillSection
	text     map[int64]string
	searched []string
	limit    int
	fail     error
	read     int // how many files were loaded in full, so a ceiling can prove it read none
}

func (l *library) Search(_ context.Context, _ int64, query string, limit int) ([]*model.Skill, error) {
	l.mu.Lock()
	l.searched = append(l.searched, query)
	l.limit = limit
	l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	// Near enough to the index for a handler test: over the handle, the title
	// and the description, which is the trio ft_skill covers.
	var hits []*model.Skill
	for _, s := range l.all {
		hay := strings.ToLower(s.Name + " " + s.Title + " " + s.Description)
		if strings.Contains(hay, strings.ToLower(query)) {
			hits = append(hits, s)
		}
	}
	return hits, nil
}

func (l *library) Skills(context.Context, int64) ([]*model.Skill, error) {
	if l.fail != nil {
		return nil, l.fail
	}
	return l.all, nil
}

func (l *library) Files(_ context.Context, _, versionID int64) ([]*model.SkillFile, error) {
	return l.files[versionID], nil
}

func (l *library) File(_ context.Context, _, fileID int64) (*model.SkillFile, error) {
	for _, group := range l.files {
		for _, f := range group {
			if f.ID == fileID {
				l.mu.Lock()
				l.read++
				l.mu.Unlock()
				loaded := *f
				// Text or bytes, never both, which is what the store does: a
				// binary file's content reaches nothing through JSON, so this
				// is the only way it travels.
				if f.Binary {
					loaded.Bytes = binaryAsset
				} else {
					loaded.Text = l.text[fileID]
				}
				return &loaded, nil
			}
		}
	}
	return nil, fmt.Errorf("no such file")
}

func (l *library) Sections(_ context.Context, _, versionID int64) ([]*model.SkillSection, error) {
	return l.sections[versionID], nil
}

// Two skills: one a person named, one that carried no title, plus a third in the
// workspace that this agent was NOT assigned. The third is the control for every
// scoping assertion below.
var binaryAsset = []byte{0x50, 0x4b, 0x03, 0x04, 0x00, 0x62, 0x69, 0x6e}

func stocked() *library {
	pdf := &model.Skill{
		ID: 1, Name: "pdf-processing", Title: "PDF Toolkit",
		Description: "Extract totals from supplier invoices and other documents.",
		Status:      model.SkillActive, ActiveVersionID: 10,
	}
	csv := &model.Skill{
		ID: 2, Name: "csv-tools", Description: "Read and write tabular exports.",
		Status: model.SkillActive, ActiveVersionID: 20,
	}
	secret := &model.Skill{
		ID: 3, Name: "payroll-checks", Title: "Payroll",
		Description: "Check payroll before it is filed. Invoices too.",
		Status:      model.SkillActive, ActiveVersionID: 30,
	}
	return &library{
		all: []*model.Skill{csv, pdf, secret},
		files: map[int64][]*model.SkillFile{
			10: {
				{ID: 100, Path: "SKILL.md", FileType: model.SkillFileSkill, SizeBytes: 90},
				{ID: 101, Path: "references/formats.md", FileType: model.SkillFileReference, SizeBytes: 40},
				{ID: 102, Path: "scripts/extract.py", FileType: model.SkillFileScript, SizeBytes: 30},
				{ID: 103, Path: "assets/template.docx", FileType: model.SkillFileAsset, Binary: true, SizeBytes: 2048},
			},
			20: {{ID: 200, Path: "SKILL.md", FileType: model.SkillFileSkill}},
			30: {{ID: 300, Path: "SKILL.md", FileType: model.SkillFileSkill}},
		},
		sections: map[int64][]*model.SkillSection{
			10: {
				{Path: "PDF processing", File: "SKILL.md"},
				{Path: "Formats", File: "references/formats.md"},
				{Path: "Formats > Caveats", File: "references/formats.md"},
			},
		},
		text: map[int64]string{
			100: "---\nname: pdf-processing\ndescription: Extract totals.\n---\n\n# PDF processing\n\nRead the document first.\n",
			101: "# Formats\n\nPDF 1.7.\n",
			102: "import sys\nprint(sys.argv)\n",
			200: "---\nname: csv-tools\n---\n\nDo the thing.\n",
			300: "---\nname: payroll-checks\n---\n\nSecret.\n",
		},
	}
}

// answer is one call's result, decoded the way the loop hands it to a model: the
// payload as a map, the classification, and the reason a failure carries in it.
type answer struct {
	body map[string]any
	kind tool.ErrorKind
	why  string
}

// field reads one string off a decoded row, which is all a JSON map can give.
func field(row map[string]any, name string) string {
	s, _ := row[name].(string)
	return s
}

// list pulls one of the payload's arrays back out as maps. JSON has no idea
// what Go type built it, so everything arrives as []any of map[string]any.
func (a answer) list(field string) []map[string]any {
	raw, _ := a.body[field].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (a answer) text(field string) string {
	s, _ := a.body[field].(string)
	return s
}

func ask(t *testing.T, h tool.Handler, args map[string]any) answer {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := h(context.Background(), tool.Call{WorkspaceID: 1, Args: raw})
	if err != nil {
		t.Fatalf("the handler returned an error rather than a result: %v", err)
	}
	got := answer{kind: res.Err}
	if err := json.Unmarshal(res.Content, &got.body); err != nil {
		t.Fatalf("the answer could not be read: %v\n%s", err, res.Content)
	}
	got.why, _ = got.body[toolkit.FieldError].(string)
	return got
}

// --- searching -------------------------------------------------------------------

func TestSearchFindsASkillByWhatItIsFor(t *testing.T) {
	lib := stocked()
	// Assigned the first two, not the third.
	got := ask(t, NewSearch(lib, []int64{1, 2}).Handle, map[string]any{"query": "invoices"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("search failed: %s", got.why)
	}

	matches := got.list("matches")
	if len(matches) != 1 {
		t.Fatalf("matched %d skills, want 1: %+v", len(matches), got.body)
	}
	// The word is in the DESCRIPTION and in no name, which is the whole point of
	// the tool existing: the prompt carries no descriptions.
	if field(matches[0], "skill") != "pdf-processing" {
		t.Errorf("matched %q, want pdf-processing", field(matches[0], "skill"))
	}
	// And the answer carries the description, so the model can judge without a
	// second call.
	if !strings.Contains(field(matches[0], "description"), "supplier invoices") {
		t.Errorf("the match carries no description: %+v", matches[0])
	}
	if field(matches[0], "name") != "PDF Toolkit" {
		t.Errorf("name = %q, want the title a person gave it", field(matches[0], "name"))
	}
	if got.body["next_action"] == nil {
		t.Error("nothing tells the model how to open what it found")
	}
}

// The scoping control: the third skill's description ALSO says "Invoices", and
// it is in the workspace. An agent that was not assigned it must not see it.
func TestSearchReachesOnlyTheSkillsTheAgentHolds(t *testing.T) {
	lib := stocked()
	got := ask(t, NewSearch(lib, []int64{1, 2}).Handle, map[string]any{"query": "Invoices"})
	matches := got.list("matches")
	for _, m := range matches {
		if field(m, "skill") == "payroll-checks" {
			t.Fatalf("a skill this agent was not assigned was found: %+v", matches)
		}
	}
	// And it IS findable by an agent that holds it, so the assertion above is
	// about the scoping rather than about the query matching nothing.
	got = ask(t, NewSearch(lib, []int64{3}).Handle, map[string]any{"query": "Invoices"})
	matches = got.list("matches")
	if len(matches) != 1 || field(matches[0], "skill") != "payroll-checks" {
		t.Fatalf("the holder cannot find it either: %+v", got.body)
	}
}

func TestSearchSaysNothingMatchedRatherThanAnsweringEmpty(t *testing.T) {
	lib := stocked()
	got := ask(t, NewSearch(lib, []int64{1, 2}).Handle, map[string]any{"query": "helicopter"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("a search that found nothing is not a failure: %s", got.why)
	}
	// The difference decides what the model does next: it should get on with the
	// job rather than rephrase for ever.
	note := got.text("note")
	if !strings.Contains(note, "own judgement") {
		t.Errorf("no note telling the model to carry on: %+v", got.body)
	}
	if got.body["next_action"] != nil {
		t.Error("it offers a next step when there is nothing to open")
	}
}

func TestSearchIsHeldToASmallNumberOfMatches(t *testing.T) {
	lib := stocked()
	ask(t, NewSearch(lib, []int64{1, 2}).Handle, map[string]any{"query": "the"})
	if lib.limit != searchLimit {
		t.Errorf("asked for %d matches, want %d", lib.limit, searchLimit)
	}
}

func TestSearchRefusesAnEmptyQuery(t *testing.T) {
	lib := stocked()
	got := ask(t, NewSearch(lib, []int64{1}).Handle, map[string]any{"query": "  "})
	if got.kind == tool.ErrorNone || got.kind != tool.ErrorBadArguments {
		t.Errorf("an empty query = %q/%q, want bad arguments", got.kind, got.why)
	}
	if len(lib.searched) != 0 {
		t.Error("it searched anyway")
	}
}

// --- opening ---------------------------------------------------------------------

func TestLoadOpensTheSkillAndIndexesTheRest(t *testing.T) {
	lib := stocked()
	got := ask(t, NewLoad(lib, []int64{1, 2}).Handle, map[string]any{"skill": "pdf-processing"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("load failed: %s", got.why)
	}

	if got.body["skill"] != "pdf-processing" || got.body["name"] != "PDF Toolkit" {
		t.Errorf("it is not named properly: %+v", got.body)
	}
	if !strings.Contains(got.text("description"), "supplier invoices") {
		t.Errorf("no description: %+v", got.body)
	}

	// The instructions, WITHOUT the frontmatter: the handle and the description
	// are answered above, so repeating them as YAML is noise the model pays for.
	instructions := got.text("instructions")
	if !strings.Contains(instructions, "Read the document first") {
		t.Errorf("the instructions did not arrive: %q", instructions)
	}
	if strings.Contains(instructions, "name: pdf-processing") {
		t.Errorf("the frontmatter came with them: %q", instructions)
	}

	// And the way into the rest of the package, with the manifest left out
	// because it IS the answer rather than a part of it.
	more := got.list("more")
	if len(more) != 3 {
		t.Fatalf("indexed %d parts, want 3: %+v", len(more), more)
	}
	byPath := map[string]map[string]any{}
	for _, m := range more {
		byPath[field(m, "part")] = m
	}
	if _, listed := byPath["SKILL.md"]; listed {
		t.Error("the manifest is listed as a part of itself")
	}
	// A reference carries its headings, so a document worth opening can be told
	// from one that is not.
	refs, _ := byPath["references/formats.md"]["topics"].([]any)
	if len(refs) != 2 || refs[1] != "Formats > Caveats" {
		t.Errorf("the reference's headings are %v, want both", refs)
	}
	// A binary asset says it is data rather than being offered as text.
	if byPath["assets/template.docx"]["binary"] != true {
		t.Errorf("a binary asset is offered as readable: %+v", byPath["assets/template.docx"])
	}
	if byPath["scripts/extract.py"]["kind"] != model.SkillFileScript {
		t.Errorf("a script is not marked as one: %+v", byPath["scripts/extract.py"])
	}
	if got.body["next_action"] == nil {
		t.Error("nothing tells the model how to read a part")
	}
}

func TestLoadReadsOnePartOfThePackage(t *testing.T) {
	lib := stocked()
	got := ask(t, NewLoad(lib, []int64{1}).Handle,
		map[string]any{"skill": "pdf-processing", "part": "references/formats.md"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("reading a part failed: %s", got.why)
	}
	if got.body["part"] != "references/formats.md" {
		t.Errorf("it answered about %v", got.body["part"])
	}
	if !strings.Contains(got.text("content"), "PDF 1.7") {
		t.Errorf("the document did not arrive: %+v", got.body)
	}
}

func TestLoadRefusesAPartThatIsNotInThePackage(t *testing.T) {
	lib := stocked()
	got := ask(t, NewLoad(lib, []int64{1}).Handle,
		map[string]any{"skill": "pdf-processing", "part": "../../etc/passwd"})
	if got.kind == tool.ErrorNone || got.kind != tool.ErrorBadArguments {
		t.Fatalf("a part outside the package = %q/%q, want bad arguments", got.kind, got.why)
	}
	// It says what IS there, so the model corrects itself rather than guessing.
	if !strings.Contains(got.why, "references/formats.md") {
		t.Errorf("the refusal does not say what the package carries: %q", got.why)
	}
}

func TestLoadWillNotOpenASkillTheAgentDoesNotHold(t *testing.T) {
	lib := stocked()
	got := ask(t, NewLoad(lib, []int64{1, 2}).Handle, map[string]any{"skill": "payroll-checks"})
	if got.kind == tool.ErrorNone {
		t.Fatal("it opened a skill this agent was never assigned")
	}
	// Nothing of the skill leaks into the refusal, and what the agent DOES hold
	// is named so it can correct itself.
	if strings.Contains(got.why, "Secret") || strings.Contains(got.why, "Payroll") {
		t.Errorf("the refusal leaks the skill: %q", got.why)
	}
	if !strings.Contains(got.why, "pdf-processing") {
		t.Errorf("the refusal does not say what it holds: %q", got.why)
	}
	// And the holder can: the assertion above is about scoping, not about the
	// skill being unreadable.
	if got := ask(t, NewLoad(lib, []int64{3}).Handle, map[string]any{"skill": "payroll-checks"}); got.kind != tool.ErrorNone {
		t.Fatalf("the holder cannot open it either: %s", got.why)
	}
}

// A model that has just read "PDF Toolkit" in its own instructions will
// sometimes send that instead of the handle. Refusing it would be a correction
// for nothing.
func TestLoadTakesTheNameAsWellAsTheHandle(t *testing.T) {
	lib := stocked()
	for _, named := range []string{"pdf-processing", "PDF Toolkit", "PDF TOOLKIT", "pdf toolkit"} {
		got := ask(t, NewLoad(lib, []int64{1}).Handle, map[string]any{"skill": named})
		if got.kind != tool.ErrorNone {
			t.Errorf("%q was refused: %s", named, got.why)
			continue
		}
		if got.body["skill"] != "pdf-processing" {
			t.Errorf("%q opened %v", named, got.body["skill"])
		}
	}
}

// A package that carried no title is called by its handle, everywhere.
func TestLoadNamesASkillWithNoTitleByItsHandle(t *testing.T) {
	lib := stocked()
	got := ask(t, NewLoad(lib, []int64{2}).Handle, map[string]any{"skill": "csv-tools"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("load failed: %s", got.why)
	}
	if got.body["name"] != "csv-tools" {
		t.Errorf("name = %v, want the handle", got.body["name"])
	}
	// And nothing is indexed, because the package is one file: an empty `more`
	// is left out rather than offered as a dead end.
	if _, listed := got.body["more"]; listed {
		t.Errorf("a one-file package offers parts to read: %+v", got.body)
	}
}

func TestLoadWillNotOpenASwitchedOffSkill(t *testing.T) {
	lib := stocked()
	lib.all[1].Status = model.SkillDisabled // pdf-processing
	got := ask(t, NewLoad(lib, []int64{1}).Handle, map[string]any{"skill": "pdf-processing"})
	// BLOCKED, not denied and not a failure: denied is about what this person
	// may do, and a skill an administrator switched off is a policy refusal,
	// "not retryable, and not a usage mistake" (tool.ErrorBlocked). The
	// classification is what stops the loop learning a working note from it.
	if got.kind != tool.ErrorBlocked {
		t.Errorf("a disabled skill = %q/%q, want blocked", got.kind, got.why)
	}
	// And it is not findable either, so the assistant never plans around it.
	got = ask(t, NewSearch(lib, []int64{1}).Handle, map[string]any{"query": "invoices"})
	if matches := got.list("matches"); len(matches) != 0 {
		t.Errorf("a disabled skill is still findable: %+v", matches)
	}
}

func TestALongDocumentIsCutAndSaysSo(t *testing.T) {
	lib := stocked()
	// Multi-byte, so a naive cut would land inside a character.
	lib.text[101] = "# Formats\n\n" + strings.Repeat("é", maxPart)
	got := ask(t, NewLoad(lib, []int64{1}).Handle,
		map[string]any{"skill": "pdf-processing", "part": "references/formats.md"})
	if got.kind != tool.ErrorNone {
		t.Fatalf("reading a long part failed: %s", got.why)
	}
	content := got.text("content")
	if len(content) > maxPart {
		t.Errorf("handed over %d bytes, want at most %d", len(content), maxPart)
	}
	if !strings.HasPrefix(content, "# Formats") {
		t.Error("it kept the end: a document opens with what it is about")
	}
	// And not cut through the middle of a character, which a naive slice at a
	// byte offset would do to every one of these.
	if !utf8.ValidString(content) {
		t.Error("the document was cut inside a character")
	}
	if got.body["note"] == nil {
		t.Error("it was cut and did not say so")
	}
}

func TestNeitherToolIsOfferedWhenNoSkillIsAssigned(t *testing.T) {
	// The loadout leaves them out entirely (tools.AppendSkills), so this is the
	// belt to that braces: a handler reached with nothing assigned answers
	// rather than reading the whole workspace.
	lib := stocked()
	got := ask(t, NewSearch(lib, nil).Handle, map[string]any{"query": "invoices"})
	if got.body["note"] != noSkills {
		t.Errorf("search with nothing assigned says %+v", got.body)
	}
	if len(lib.searched) != 0 {
		t.Error("it searched the workspace anyway")
	}
	if got := ask(t, NewLoad(lib, nil).Handle, map[string]any{"skill": "pdf-processing"}); got.why != noSkills {
		t.Errorf("load with nothing assigned says %q", got.why)
	}
}

func TestBothToolsAreInternalAndReadOnly(t *testing.T) {
	for _, s := range []tool.Schema{SearchSchema(), LoadSchema()} {
		if s.Kind != tool.KindInternal {
			t.Errorf("%s is %q: it would appear in the admin catalogue", s.Name, s.Kind)
		}
		if s.Risk != tool.RiskReadOnly {
			t.Errorf("%s is %q, want read-only", s.Name, s.Risk)
		}
		if s.RequiresApproval {
			t.Errorf("%s asks for approval to read", s.Name)
		}
	}
}
