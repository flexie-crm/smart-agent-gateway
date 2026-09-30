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
	"sync"
	"sync/atomic"
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

// The collision, with nothing faked.
//
// The gate's own unit tests drive the real handler against a computer written
// here in Go, which proves the gate holds a counter down and nothing else. This
// proves the thing itself: real agents resolved the way the product resolves
// them, a real skill read out of a real archive and imported into a real
// database, the real machine link, the REAL chat application at the far end,
// and a real directory on a real disk at the end of it.
//
// It can come out the other way, which is the point. The client's staging
// directory is named for the version ALONE, so a gate that does not hold is not
// a counter that reads twenty: it is twenty installs deleting each other's
// files mid-write, and what the fleet gets back is `File exists` or a tree that
// was renamed into place with pieces missing. That is the failure this was
// built for, and taking the gate out is how it is checked (see the control in
// KB/42).
//
//	SAG_LINK_E2E=1 SAG_TEST_DSN='...' make link-e2e

// howManyAgents is the fleet. Twenty is the product's own cap
// (DefaultMaxFleetAgents), so this is the worst case a person can actually
// create rather than a number chosen to make a point.
const howManyAgents = 20

// howManyFiles is what a collision needs to be visible. Two installs racing
// over four files can finish in an order that happens to work; over three
// hundred they cannot, because one is always deleting what another is writing.
const howManyFiles = 300

func TestAFleetOfAgentsPutsOneSkillOnTheComputerOnce(t *testing.T) {
	linktest.Skip(t)
	e := newEnv(t) // a real database, a real store, the real application object
	ctx := context.Background()
	person := e.user("fleet@acme.test")

	held := importSkill(t, e, packageOfManyFiles(t))

	// A meter in front of the real store. Everything below still goes through
	// the product's own wiring; this only counts what it asks for.
	meter := &countedSkills{SkillStore: e.app.Store.Skills()}
	e.app.Store = withSkills{Store: e.app.Store, skills: meter}

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

	// Twenty agents, each holding the skill, each resolved the way a fleet
	// member is resolved: separately, so each gets its own owner and its own
	// loadout, which is what makes them twenty callers rather than one.
	loadouts := make([]tool.Loadout, howManyAgents)
	for i := range loadouts {
		key := fmt.Sprintf("member-%02d", i)
		if err := e.app.Store.Agents().Create(ctx, &model.Agent{
			WorkspaceID: e.ws.ID, Key: key, Name: "Member " + strconv.Itoa(i),
			Status: model.StatusActive, Skills: []int64{held.ID},
		}, model.Nobody()); err != nil {
			t.Fatalf("create agent %s: %v", key, err)
		}
		resolved, err := e.app.ResolveAgent(ctx, e.ws.ID, person.ID, app.Computer{DeviceID: device}, key)
		if err != nil {
			t.Fatalf("resolve agent %s: %v", key, err)
		}
		if _, ok := resolved.Tools.Handlers[skills.ExecName]; !ok {
			t.Fatalf("agent %s was not offered %s, so nothing here would reach the computer",
				key, skills.ExecName)
		}
		loadouts[i] = resolved.Tools
	}

	// All twenty at once, twice. The first fleet arrives at a computer that has
	// never seen this skill, which is the collision. The second arrives at one
	// that now has it, which is every run after the first and must cost no
	// install at all.
	first := runFleet(t, ctx, loadouts, e.ws.ID, person.ID, device)

	// FIRST, that there was a collision at all.
	if most, want := first.mostAtOnce, int64(howManyAgents); most != want {
		t.Fatalf("only %d of %d agents were in the tool at once, so nothing here "+
			"proves anything about the gate", most, want)
	}
	// ONE package crossed the link.
	if installs := far.installs.Load(); installs != 1 {
		t.Errorf("the package crossed the link %d times, want once", installs)
	}
	// And the bodies were read out of the database once per file, not once per
	// file per agent. The LISTING is read per agent on purpose: every one of
	// them needs it to check the script it was asked for is one this version
	// ships, and it carries no file contents (sqlstore.Files selects metadata).
	// Counted from the version rather than worked out here: a package holds
	// what it holds, and the first version of this line said 302 because it
	// forgot one of the two scripts.
	shipped, err := e.app.Store.Skills().Files(ctx, e.ws.ID, held.ActiveVersionID)
	if err != nil {
		t.Fatalf("list the version's files: %v", err)
	}
	if bodies, want := meter.bodies.Load(), int64(len(shipped)); bodies != want {
		t.Errorf("file bodies were read %d times, want %d (one package, once)", bodies, want)
	}
	// One per agent, plus the one the line above just made.
	if listings, want := meter.listings.Load(), int64(howManyAgents+1); listings != want {
		t.Errorf("the file list was read %d times, want %d (one per agent)", listings, want)
	}

	// What it cost, printed rather than asserted: a threshold here would fail
	// on a busy machine. It is what the two-minute ceiling on waiting is
	// measured against.
	t.Logf("a %d file package reached the computer in %v",
		len(shipped), time.Duration(far.took.Load()))

	// Every agent's own script ran, and each got ITS OWN answer: the argument
	// each one passed comes back in the output, so a single answer handed to
	// everybody would fail here.
	for at, one := range first.got {
		if one.err != "" {
			t.Errorf("agent %d: %s\n%s", at, one.err, one.body)
			continue
		}
		want := fmt.Sprintf("ran for %d", at)
		if !strings.Contains(one.body, want) {
			t.Errorf("agent %d did not get its own answer (%q):\n%s", at, want, one.body)
		}
	}

	// And the copy on the disk is whole. This is the assertion the counter
	// cannot make: twenty installs sharing one staging directory can still end
	// with a directory, and what is wrong with it is what is missing from it.
	home := filepath.Join(client.State, ".sag", "skill", "pdf-processing",
		strconv.FormatInt(held.ActiveVersionID, 10))
	for i := 0; i < howManyFiles; i++ {
		path := filepath.Join(home, "references", fmt.Sprintf("p%d.md", i))
		body, err := os.ReadFile(path) //nolint:gosec // a path this test built
		if err != nil {
			t.Fatalf("the installed copy is missing a file: %v", err)
		}
		if want := fmt.Sprintf("# part %d\n", i); string(body) != want {
			t.Fatalf("%s holds %q, want %q", path, body, want)
		}
	}
	for _, path := range []string{"SKILL.md", "scripts/report.py", "scripts/helper.py"} {
		if _, err := os.Stat(filepath.Join(home, path)); err != nil {
			t.Fatalf("the installed copy is missing %s: %v", path, err)
		}
	}
	// AND AGAIN, now that it is there. Nothing should cross the link at all:
	// every agent's first question is answered by a computer that already holds
	// the version, so the gate is never reached and no package is read.
	beforeAgain := far.installs.Load()
	bodiesBefore := meter.bodies.Load()
	again := runFleet(t, ctx, loadouts, e.ws.ID, person.ID, device)
	if most, want := again.mostAtOnce, int64(howManyAgents); most != want {
		t.Errorf("the second fleet did not collide either (%d of %d at once)", most, want)
	}
	if put := far.installs.Load() - beforeAgain; put != 0 {
		t.Errorf("a skill already on the computer was sent %d more times", put)
	}
	if read := meter.bodies.Load() - bodiesBefore; read != 0 {
		t.Errorf("a skill already on the computer had %d file bodies read for it", read)
	}
	for at, one := range again.got {
		if one.err != "" {
			t.Errorf("second time round, agent %d: %s\n%s", at, one.err, one.body)
		}
	}

	// Nothing half-written survived either.
	leftovers, err := os.ReadDir(filepath.Dir(home))
	if err != nil {
		t.Fatalf("read the skill's folder: %v", err)
	}
	for _, entry := range leftovers {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("a staging directory was left behind: %s", entry.Name())
		}
	}
}

// fleetRun is what one fleet of agents came back with.
type fleetRun struct {
	got []outcome
	// mostAtOnce is how many of them were inside the tool at the same time.
	// Twenty calls that do not overlap are a queue, and a queue installs once
	// whether or not there is a gate, so without this number every other
	// assertion would pass with the gate deleted.
	mostAtOnce int64
}

type outcome struct {
	body string
	err  string
}

// runFleet calls skill_exec on every loadout at once, released together.
// Started one after another they would queue politely and the gate would never
// be asked anything.
func runFleet(t *testing.T, ctx context.Context, loadouts []tool.Loadout,
	workspaceID, userID int64, device string,
) fleetRun {
	t.Helper()
	got := make([]outcome, len(loadouts))
	var inside, mostAtOnce atomic.Int64
	var ready, done sync.WaitGroup
	release := make(chan struct{})
	for i := range loadouts {
		ready.Add(1)
		done.Add(1)
		go func(at int) {
			defer done.Done()
			args, _ := json.Marshal(map[string]any{
				"skill": "pdf-processing", "script": "scripts/report.py",
				"args": []string{strconv.Itoa(at)},
			})
			ready.Done()
			<-release
			// Compare-and-swap rather than load-then-store: two goroutines
			// arriving together would both read the old high-water mark and
			// both write their own, losing one. It would only ever UNDERcount,
			// so it would show up as a test failing to say the agents collided,
			// which is a bad half-hour.
			now := inside.Add(1)
			for {
				most := mostAtOnce.Load()
				if now <= most || mostAtOnce.CompareAndSwap(most, now) {
					break
				}
			}
			defer inside.Add(-1)
			res, err := loadouts[at].Handlers[skills.ExecName](ctx, tool.Call{
				WorkspaceID: workspaceID, UserID: userID, DeviceID: device,
				SessionID: 4242, Name: skills.ExecName, Args: args,
			})
			if err != nil {
				got[at] = outcome{err: err.Error()}
				return
			}
			got[at] = outcome{body: string(res.Content), err: string(res.Err)}
		}(i)
	}
	ready.Wait()
	close(release)
	done.Wait()
	return fleetRun{got: got, mostAtOnce: mostAtOnce.Load()}
}

// --- the fixtures ----------------------------------------------------------

// packageOfManyFiles is a real archive: a manifest with real frontmatter, a
// script that imports its sibling (so a tree that arrived in pieces cannot run
// it) and enough filler to make a collision visible.
func packageOfManyFiles(t *testing.T) []byte {
	t.Helper()
	const manifest = `---
name: pdf-processing
description: Extract, inspect, and transform PDF files. Use for PDF-related tasks.
---

# PDF processing

Read the file first.

## Reporting

Run scripts/report.py with the name to report for.
`
	// It imports a sibling and echoes the argument it was given, so its answer
	// proves three things at once: the whole tree is there, the script ran on
	// this machine, and this agent's own call is what produced this answer.
	const script = "import sys\n\nfrom helper import stamp\n\nprint(stamp(sys.argv[1]))\n"
	const helper = "def stamp(who):\n    return \"ran for \" + who\n"

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
	add("pdf-processing/scripts/report.py", script)
	add("pdf-processing/scripts/helper.py", helper)
	for i := 0; i < howManyFiles; i++ {
		add(fmt.Sprintf("pdf-processing/references/p%d.md", i), fmt.Sprintf("# part %d\n", i))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close the archive: %v", err)
	}
	return buf.Bytes()
}

// importSkill reads an archive with the real reader and imports it with the
// real store, which is the path an administrator's upload takes.
func importSkill(t *testing.T, e *env, archive []byte) *model.Skill {
	t.Helper()
	pkg, err := skill.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	held, written, err := e.app.Store.Skills().Import(context.Background(), e.ws.ID, pkg, model.Nobody())
	if err != nil {
		t.Fatalf("import the package: %v", err)
	}
	if !written {
		t.Fatal("the package was not written as a version")
	}
	return held
}

// --- the meters ------------------------------------------------------------

// countedLink is the real registry with a tally of what crossed it.
type countedLink struct {
	machine.Machines
	installs atomic.Int64
	runs     atomic.Int64
	// took is how long the real push of the real package took, so the ceiling
	// on waiting (skills.waitForInstall) is a number with something behind it
	// rather than a feeling.
	took atomic.Int64
}

func (c *countedLink) Call(ctx context.Context, workspaceID, userID int64, deviceID, name string,
	args json.RawMessage, reason string,
) (link.Result, error) {
	switch name {
	case machine.SkillInstallName:
		c.installs.Add(1)
		began := time.Now()
		res, err := c.Machines.Call(ctx, workspaceID, userID, deviceID, name, args, reason)
		c.took.Store(int64(time.Since(began)))
		return res, err
	case machine.SkillRunName:
		c.runs.Add(1)
	}
	return c.Machines.Call(ctx, workspaceID, userID, deviceID, name, args, reason)
}

// countedSkills is the real skill store with a tally of what was read out of it.
type countedSkills struct {
	store.SkillStore
	listings atomic.Int64 // Files: metadata, which every agent needs
	bodies   atomic.Int64 // File: one file's content, which is the expensive one
}

func (c *countedSkills) Files(ctx context.Context, workspaceID, versionID int64) ([]*model.SkillFile, error) {
	c.listings.Add(1)
	return c.SkillStore.Files(ctx, workspaceID, versionID)
}

func (c *countedSkills) File(ctx context.Context, workspaceID, fileID int64) (*model.SkillFile, error) {
	c.bodies.Add(1)
	return c.SkillStore.File(ctx, workspaceID, fileID)
}

// withSkills is the real store answering with the counted skills. Embedded, so
// every other store on it is the real one and stays the real one as they are
// added.
type withSkills struct {
	store.Store
	skills store.SkillStore
}

func (w withSkills) Skills() store.SkillStore { return w.skills }
