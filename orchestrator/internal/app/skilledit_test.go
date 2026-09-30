package app_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/linktest"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/skill"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/skills"
)

// Editing a script and publishing it, all the way to the code that runs.
//
// This is the whole feature in one test, and the only one that can show it
// works, because the claim spans four places that each believe something about
// the others: the package is rewritten in Go, stored as a new version in
// MariaDB, made live by the same route a rollback uses, and then fetched by the
// REAL chat application because the version id it keys its folder by has
// changed. Nothing here is a stand-in.
//
// It can come out the other way, which is the point of the last step. If
// publishing did not move the pointer, or if the far end keyed its copy by the
// skill rather than by the version, this test would run the OLD script and say
// so: the answer carries which version printed it.
//
//	SAG_LINK_E2E=1 SAG_TEST_DSN='...' make link-e2e
func TestPublishingAnEditRunsTheEditedScript(t *testing.T) {
	linktest.Skip(t)
	e := newEnv(t)
	ctx := context.Background()
	person := e.user("author@acme.test")

	held := importSkill(t, e, packageWithAScriptSaying(t, "version one"))
	first := held.ActiveVersionID

	// The real application at the far end of a real link.
	const device = "the-laptop"
	const token = "a-real-looking-token"
	registry := link.NewRegistry(zerolog.Nop(), func(given string) (int64, int64, string, time.Time, bool) {
		if given == token {
			return person.ID, e.ws.ID, device, time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Time{}, false
	}, []string{"*"})
	client := linktest.Start(t, linktest.Serve(t, registry), token)
	linktest.Await(t, "the real chat application to link", func() bool {
		return registry.Online(e.ws.ID, person.ID, device)
	})
	if runs := registry.Runs(e.ws.ID, person.ID, device); runs[machine.SkillRunName] == 0 {
		t.Skip("this build of the application does not run skills")
	}
	far := &countedLink{Machines: registry}
	e.app.Machines = far

	if err := e.app.Store.Agents().Create(ctx, &model.Agent{
		WorkspaceID: e.ws.ID, Key: "author", Name: "Author",
		Status: model.StatusActive, Skills: []int64{held.ID},
	}, model.Nobody()); err != nil {
		t.Fatalf("create the agent: %v", err)
	}
	// Resolved fresh before each run, the way a turn resolves one: the loadout
	// holds the skill's id, and which VERSION it reaches for is read when the
	// tool runs. Reusing one loadout across a publish would prove nothing about
	// what a later turn does.
	run := func(what string) string {
		t.Helper()
		resolved, err := e.app.ResolveAgent(ctx, e.ws.ID, person.ID,
			app.Computer{DeviceID: device}, "author")
		if err != nil {
			t.Fatalf("resolve the agent (%s): %v", what, err)
		}
		args, _ := json.Marshal(map[string]any{
			"skill": "pdf-processing", "script": "scripts/say.py",
		})
		res, err := resolved.Tools.Handlers[skills.ExecName](ctx, tool.Call{
			WorkspaceID: e.ws.ID, UserID: person.ID, DeviceID: device,
			SessionID: 5150, Name: skills.ExecName, Args: args,
		})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if res.Err != "" {
			t.Fatalf("%s was refused: %s\n%s", what, res.Err, res.Content)
		}
		return string(res.Content)
	}

	// 1. Before the edit: the script that was imported runs.
	said := run("the first run")
	if !strings.Contains(said, "version one") {
		t.Fatalf("the imported script did not run:\n%s", said)
	}
	if put := far.installs.Load(); put != 1 {
		t.Fatalf("the package crossed the link %d times, want once", put)
	}

	// 2. Edit it. Through the app, which is the path the console's save takes.
	edited, changed, err := e.app.DraftSkillVersion(ctx, e.ws.ID, held.ID, app.SkillEdit{
		From: first,
		Edits: []skill.Edit{{
			Path: "scripts/say.py",
			Text: "print('version two')\n",
		}},
	}, model.Actor{Name: "Author"})
	if err != nil {
		t.Fatalf("save the edit: %v", err)
	}
	if edited.Status != model.SkillVersionDraft {
		t.Fatalf("the edit saved as %q, want a draft", edited.Status)
	}
	if len(changed) != 1 || changed[0] != "scripts/say.py" {
		t.Fatalf("changed = %v", changed)
	}

	// 3. A DRAFT changes nothing. The agent goes on running what it was running,
	//    and nothing new crosses the link: this is what makes a draft worth
	//    having rather than a version that goes live when somebody saves.
	before := far.installs.Load()
	if said = run("after saving the draft"); !strings.Contains(said, "version one") {
		t.Fatalf("an unpublished draft changed what runs:\n%s", said)
	}
	if put := far.installs.Load() - before; put != 0 {
		t.Errorf("a draft was sent to the computer %d times", put)
	}

	// 4. Publish it, through the one route that makes a version live.
	if _, err := e.app.Store.Skills().Update(ctx, e.ws.ID, held.ID, store.SkillUpdate{
		Title: held.Title, Description: held.Description,
		Status: model.SkillActive, VersionID: edited.ID,
	}, model.Actor{Name: "Author"}); err != nil {
		t.Fatalf("publish the draft: %v", err)
	}

	// 5. THE CLAIM: the next run is the edited script, on the person's own
	//    computer, without anybody reinstalling anything by hand.
	said = run("after publishing")
	if !strings.Contains(said, "version two") {
		t.Fatalf("the published edit did not reach the computer:\n%s", said)
	}
	if strings.Contains(said, "version one") {
		t.Fatalf("the old script ran after publishing:\n%s", said)
	}
	// It was fetched again, once, because the version id is the key.
	if put := far.installs.Load(); put != 2 {
		t.Errorf("the package crossed the link %d times, want twice (one per version)", put)
	}

	// 6. And both versions are on the disk, each under its own id, which is
	//    what makes the old one still runnable after a rollback.
	for _, version := range []int64{first, edited.ID} {
		home := filepath.Join(client.State, ".sag", "skill", "pdf-processing",
			strconv.FormatInt(version, 10), "scripts", "say.py")
		body, err := os.ReadFile(home) //nolint:gosec // a path this test built
		if err != nil {
			t.Fatalf("version %d is not on the disk: %v", version, err)
		}
		want := "version one"
		if version == edited.ID {
			want = "version two"
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("version %d on disk says %q, want %q", version, body, want)
		}
	}

	// 7. Rolling back runs the old one again, off the copy already there.
	before = far.installs.Load()
	if _, err := e.app.Store.Skills().Update(ctx, e.ws.ID, held.ID, store.SkillUpdate{
		Title: held.Title, Description: held.Description,
		Status: model.SkillActive, VersionID: first,
	}, model.Actor{Name: "Author"}); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if said = run("after rolling back"); !strings.Contains(said, "version one") {
		t.Fatalf("a rollback did not go back:\n%s", said)
	}
	// Nothing crossed the link for it: that version is still on the computer,
	// which is the other half of keying the folder by an immutable id.
	if put := far.installs.Load() - before; put != 0 {
		t.Errorf("a rollback sent the package %d times, want none: it was already there", put)
	}
}

// packageWithAScriptSaying is a real archive whose one script prints what it is
// told to, so the answer from a run says WHICH version produced it.
func packageWithAScriptSaying(t *testing.T, what string) []byte {
	t.Helper()
	const manifest = `---
name: pdf-processing
description: Extract, inspect, and transform PDF files. Use for PDF-related tasks.
---

# PDF processing

Run scripts/say.py.
`
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	add := func(name, body string) {
		t.Helper()
		out, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("pack %s: %v", name, err)
		}
		if _, err := out.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	add("pdf-processing/"+skill.Manifest, manifest)
	add("pdf-processing/scripts/say.py", fmt.Sprintf("print(%q)\n", what))
	if err := w.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	return buf.Bytes()
}
