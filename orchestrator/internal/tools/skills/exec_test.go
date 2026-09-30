package skills

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	mlink "flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// A computer, as this package talks to one.
//
// It remembers which VERSIONS it has been given, which is the whole protocol:
// asked to run a script of a version it does not hold it says so, and that is
// what the handler is supposed to answer by installing.
type computer struct {
	// LOCKED for the same reason the library is: the fleet tests call this from
	// twenty goroutines, and an unguarded append silently loses some.
	mu        sync.Mutex
	installed map[int64]map[string]string // version -> path -> content
	calls     []string                    // every link call made, in order
	ran       []map[string]any            // the run requests it accepted
	raw       []json.RawMessage           // exactly what was sent, byte for byte
	failRun   string                      // a failure the script itself reports
	failGet   string                      // a refusal of the install
	forget    bool                        // report missing even after installing
}

func newComputer() *computer {
	return &computer{installed: map[int64]map[string]string{}}
}

func (c *computer) Runs(int64, int64, string) map[string]int {
	return map[string]int{
		machine.SkillRunName:     machine.VersionOf(machine.SkillRunName),
		machine.SkillInstallName: machine.VersionOf(machine.SkillInstallName),
	}
}

func (c *computer) Call(_ context.Context, _, _ int64, _, name string, args json.RawMessage, _ string) (mlink.Result, error) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.raw = append(c.raw, args)
	c.mu.Unlock()
	var in struct {
		Skill   string   `json:"skill"`
		Version int64    `json:"version"`
		Script  string   `json:"script"`
		Args    []string `json:"args"`
		Files   []struct {
			Path   string `json:"path"`
			Text   string `json:"text"`
			Base64 string `json:"base64"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	_ = json.Unmarshal(args, &in)

	c.mu.Lock()
	failGet, failRun, forget := c.failGet, c.failRun, c.forget
	c.mu.Unlock()

	switch name {
	case machine.SkillInstallName:
		if failGet != "" {
			return mlink.Result{OK: false, Message: failGet}, nil
		}
		held := map[string]string{}
		for _, f := range in.Files {
			body := f.Text
			if f.Base64 != "" {
				raw, err := base64.StdEncoding.DecodeString(f.Base64)
				if err != nil {
					return mlink.Result{OK: false, Message: "undecodable: " + f.Path}, nil
				}
				body = string(raw)
			}
			held[f.Path] = body
		}
		c.mu.Lock()
		c.installed[in.Version] = held
		c.mu.Unlock()
		return mlink.Result{OK: true}, nil

	case machine.SkillRunName:
		c.mu.Lock()
		_, have := c.installed[in.Version]
		c.mu.Unlock()
		if !have || forget {
			// The shape the handler reads: a KIND, not words.
			return mlink.Result{
				OK: false, Kind: missingKind,
				Message: "this computer does not have that version of the skill",
			}, nil
		}
		c.mu.Lock()
		c.ran = append(c.ran, map[string]any{
			"skill": in.Skill, "version": in.Version, "script": in.Script, "args": in.Args,
		})
		c.mu.Unlock()
		if failRun != "" {
			return mlink.Result{OK: false, Kind: "failed", Message: failRun}, nil
		}
		out, _ := json.Marshal(map[string]any{"output": "ran " + in.Script, "exit_code": 0})
		return mlink.Result{OK: true, Content: out}, nil
	}
	return mlink.Result{OK: false, Message: "unknown call " + name}, nil
}

// counts how many times one call was made.
func (c *computer) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, made := range c.calls {
		if made == name {
			n++
		}
	}
	return n
}

func run(t *testing.T, lib Library, c *computer, allowed []int64, args map[string]any) answer {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := NewExec(lib, c, allowed).Handle(context.Background(), tool.Call{
		WorkspaceID: 1, UserID: 2, DeviceID: "the-laptop", Args: raw,
	})
	if err != nil {
		t.Fatalf("the handler returned an error rather than a result: %v", err)
	}
	got := answer{kind: res.Err}
	if err := json.Unmarshal(res.Content, &got.body); err != nil {
		t.Fatalf("the answer could not be read: %v (%s)", err, res.Content)
	}
	got.why, _ = got.body["error"].(string)
	return got
}

// THE PROTOCOL. A computer that has never seen this version is sent the whole
// package and then asked again, and the model sees one call either way.
func TestAFirstRunSendsThePackageAndThenRunsIt(t *testing.T) {
	lib := stocked()
	c := newComputer()

	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py", "args": []string{"in.pdf"},
	})
	if got.kind != tool.ErrorNone {
		t.Fatalf("the run failed: %s", got.why)
	}

	// run, install, run: asked, sent, asked again.
	if want := []string{machine.SkillRunName, machine.SkillInstallName, machine.SkillRunName}; strings.Join(c.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("the calls were %v, want %v", c.calls, want)
	}
	if len(c.ran) != 1 {
		t.Fatalf("the script ran %d times, want once", len(c.ran))
	}
	// The script ran with what the model asked for, under the version it was
	// installed as.
	if c.ran[0]["script"] != "scripts/extract.py" {
		t.Errorf("it ran %v", c.ran[0]["script"])
	}
	// int64, not float64: this map is built in Go by the fake rather than
	// decoded from JSON, so it holds what the struct field held.
	if c.ran[0]["version"] != int64(10) {
		t.Errorf("it ran version %v (%T), want 10", c.ran[0]["version"], c.ran[0]["version"])
	}

	// THE WHOLE TREE went down, not the one script that was asked for: a script
	// opens its siblings and nothing here can know which.
	held := c.installed[10]
	for _, want := range []string{
		"SKILL.md", "references/formats.md", "scripts/extract.py", "assets/template.docx",
	} {
		if _, sent := held[want]; !sent {
			t.Errorf("%s was not sent (sent: %d files)", want, len(held))
		}
	}

	// And a SECOND run installs nothing: the version is the key, and it has it.
	got = run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if got.kind != tool.ErrorNone {
		t.Fatalf("the second run failed: %s", got.why)
	}
	if c.count(machine.SkillInstallName) != 1 {
		t.Errorf("the package was sent %d times", c.count(machine.SkillInstallName))
	}
	if len(c.ran) != 2 {
		t.Errorf("the script ran %d times across two calls", len(c.ran))
	}
}

// A binary asset survives the trip, which it only does because it is carried
// encoded: model.SkillFile keeps raw bytes out of JSON on purpose.
func TestAnAssetCrossesTheLinkIntact(t *testing.T) {
	lib := stocked()
	lib.text[103] = "" // the docx is binary; its bytes come from the store
	c := newComputer()
	run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if body, sent := c.installed[10]["assets/template.docx"]; !sent {
		t.Error("the asset was not sent at all")
	} else if body != string(binaryAsset) {
		t.Errorf("the asset arrived as %q, want %q", body, binaryAsset)
	}
}

// A script that exits non-zero is not a tool that failed: it ran, and what it
// said is the answer. An assistant told "the tool failed" reaches for another
// tool; one told what the script printed fixes the argument.
func TestAScriptThatFailsSaysWhatItSaid(t *testing.T) {
	lib := stocked()
	c := newComputer()
	c.failRun = "Traceback: no such file: in.pdf"
	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if !strings.Contains(got.why, "no such file") {
		t.Errorf("the script's own words did not reach the model: %+v", got.body)
	}
}

// The script must be one the package actually ships, checked HERE. A path the
// package does not carry is the model misreading an index, and it must never
// reach a filesystem as a path to try.
func TestOnlyAScriptThePackageShipsCanBeRun(t *testing.T) {
	lib := stocked()
	for _, named := range []string{
		"../../../etc/passwd",
		"scripts/nope.py",
		"references/formats.md", // real, in the package, and not a script
		"assets/template.docx",
	} {
		c := newComputer()
		got := run(t, lib, c, []int64{1}, map[string]any{
			"skill": "pdf-processing", "script": named,
		})
		if got.kind != tool.ErrorBadArguments {
			t.Errorf("%q = %q, want bad arguments", named, got.kind)
		}
		if len(c.calls) != 0 {
			t.Errorf("%q reached the computer anyway: %v", named, c.calls)
		}
		// And the refusal says what could have been asked for instead.
		if !strings.Contains(got.why, "scripts/extract.py") {
			t.Errorf("%q: the refusal does not name the scripts: %q", named, got.why)
		}
	}
}

func TestAScriptOfASkillTheAgentDoesNotHoldIsRefused(t *testing.T) {
	lib := stocked()
	c := newComputer()
	got := run(t, lib, c, []int64{1, 2}, map[string]any{
		"skill": "payroll-checks", "script": "scripts/extract.py",
	})
	if got.kind == tool.ErrorNone {
		t.Fatal("it ran a script of a skill this agent was never assigned")
	}
	if len(c.calls) != 0 {
		t.Errorf("it reached the computer: %v", c.calls)
	}
}

func TestASwitchedOffSkillRunsNothing(t *testing.T) {
	lib := stocked()
	lib.all[1].Status = model.SkillDisabled // pdf-processing
	c := newComputer()
	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if got.kind != tool.ErrorBlocked {
		t.Errorf("a disabled skill = %q, want blocked", got.kind)
	}
	if len(c.calls) != 0 {
		t.Errorf("it reached the computer: %v", c.calls)
	}
}

// Two ends that disagree are answered, not looped over. The computer here says
// it is missing the version even after being sent it.
func TestAComputerThatKeepsReportingItMissingIsAnsweredNotRetried(t *testing.T) {
	lib := stocked()
	c := newComputer()
	c.forget = true
	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if got.kind == tool.ErrorNone {
		t.Fatal("it reported success")
	}
	if !strings.Contains(got.why, "still reports it as missing") {
		t.Errorf("the answer does not say what happened: %q", got.why)
	}
	// Exactly one install and two asks. A third would be a loop.
	if c.count(machine.SkillInstallName) != 1 || c.count(machine.SkillRunName) != 2 {
		t.Errorf("it looped: %v", c.calls)
	}
}

func TestAnInstallThatIsRefusedSaysSo(t *testing.T) {
	lib := stocked()
	c := newComputer()
	c.failGet = "there is no room on this disk"
	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if got.kind == tool.ErrorNone {
		t.Fatal("it reported success")
	}
	if !strings.Contains(got.why, "no room on this disk") {
		t.Errorf("the computer's own reason did not reach the answer: %q", got.why)
	}
	if len(c.ran) != 0 {
		t.Error("it ran the script after failing to send it")
	}
}

func TestRunningNeedsAComputer(t *testing.T) {
	lib := stocked()
	raw, _ := json.Marshal(map[string]any{"skill": "pdf-processing", "script": "scripts/extract.py"})
	res, err := NewExec(lib, newComputer(), []int64{1}).Handle(context.Background(),
		tool.Call{WorkspaceID: 1, UserID: 2, Args: raw}) // no DeviceID: a browser
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !res.Failed() {
		t.Fatal("a conversation with no computer ran a script")
	}
	if !strings.Contains(string(res.Content), "your own computer") {
		t.Errorf("the reason is not readable: %s", res.Content)
	}
}

func TestExecIsInternalAndClassifiedLikeTheTerminal(t *testing.T) {
	s := ExecSchema()
	if s.Kind != tool.KindInternal {
		t.Errorf("kind = %q: it would appear in the admin catalogue", s.Kind)
	}
	// The terminal's own level, because it is the same act: code somebody wrote
	// running on their computer as them.
	if s.Risk != tool.RiskDestructiveAction {
		t.Errorf("risk = %q, want the terminal's", s.Risk)
	}
	if s.RequiresApproval {
		t.Error("it asks for approval: a skill is vetted when it is imported")
	}
}

// A package too big to send is refused by name and size, BEFORE it is read.
//
// The ceiling exists so a very large package is not loaded into the gateway's
// memory and pushed at somebody's laptop. A check made while the files were
// being read would have done exactly that and then refused, which is the cost
// it was meant to avoid; the size is on the listing, so it is decided first.
func TestAPackageTooBigToSendIsRefusedBeforeItIsRead(t *testing.T) {
	lib := stocked()
	// One enormous file, by its LISTED size. Nothing needs to allocate it: what
	// is under test is that the decision is taken from the listing.
	lib.files[10][3].SizeBytes = maxInstall + 1

	c := newComputer()
	got := run(t, lib, c, []int64{1}, map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	if got.kind == tool.ErrorNone {
		t.Fatal("an oversized package was sent")
	}
	// The size is named, so somebody can act on it.
	if !strings.Contains(got.why, "MB") {
		t.Errorf("the refusal does not say how big it is: %q", got.why)
	}
	// It never reached the computer, and no file was read to find that out.
	if c.count(machine.SkillInstallName) != 0 {
		t.Error("it tried to send it anyway")
	}
	if lib.read != 0 {
		t.Errorf("it read %d files before refusing: the ceiling is meant to avoid exactly that",
			lib.read)
	}
}

// A script that takes no arguments is not a broken call.
//
// `args` is optional in the schema, so a model running a script that takes none
// leaves it out. An absent field unmarshals to a nil slice, and a nil slice
// marshals back as `null`, which the far end reads as a malformed request: the
// real application answered "that is not a script to run: invalid type: null,
// expected a sequence" and the agent was told its own call was wrong when the
// call was right.
//
// So this asserts the BYTES, not the decoded value. Decoding proves nothing
// here: Go reads null into a nil slice quite happily, which is exactly how this
// went unnoticed.
func TestAScriptWithNoArgumentsSendsAListNotNull(t *testing.T) {
	lib := stocked()
	c := newComputer()
	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	res, err := NewExec(lib, c, []int64{1}).Handle(context.Background(), tool.Call{
		WorkspaceID: 1, UserID: 2, DeviceID: "the-laptop", Args: raw,
	})
	if err != nil {
		t.Fatalf("running with no arguments: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("running with no arguments was refused: %s\n%s", res.Err, res.Content)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.raw) == 0 {
		t.Fatal("nothing was sent to the computer")
	}
	for at, sent := range c.raw {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(sent, &body); err != nil {
			t.Fatalf("call %d could not be read: %v", at, err)
		}
		args, carried := body["args"]
		if !carried {
			continue // the install carries no arguments at all
		}
		if string(args) != "[]" {
			t.Errorf("call %d sent args as %s, want []", at, args)
		}
	}
}
