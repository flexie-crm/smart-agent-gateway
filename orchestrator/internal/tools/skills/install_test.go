package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mlink "flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// A computer that counts what crossed the link and can be made slow, so a
// second caller genuinely arrives while the first is still installing.
type counted struct {
	*computer
	installs atomic.Int64
	runs     atomic.Int64
	slow     time.Duration
	release  chan struct{} // when set, an install waits for this before finishing
	landed   chan struct{} // when set, an install FINISHES and then waits for this
}

func newCounted() *counted {
	return &counted{computer: newComputer()}
}

func (c *counted) Call(ctx context.Context, ws, user int64, device, name string, args json.RawMessage, reason string) (mlink.Result, error) {
	switch name {
	case machine.SkillInstallName:
		c.installs.Add(1)
		if c.release != nil {
			select {
			case <-c.release:
			case <-ctx.Done():
				return mlink.Result{}, ctx.Err()
			}
		}
		if c.slow > 0 {
			time.Sleep(c.slow)
		}
		// The package is put there FIRST and the leader is held afterwards, so
		// a waiter can time out at a moment when the skill is genuinely on the
		// computer. That is the ordinary version of running out of patience.
		res, err := c.computer.Call(ctx, ws, user, device, name, args, reason)
		if c.landed != nil {
			select {
			case <-c.landed:
			case <-ctx.Done():
				return mlink.Result{}, ctx.Err()
			}
		}
		return res, err
	case machine.SkillRunName:
		c.runs.Add(1)
	}
	return c.computer.Call(ctx, ws, user, device, name, args, reason)
}

// aDeviceOfItsOwn names the computer for one test.
//
// The gate is process-wide, which is right (it is a fact about a machine, not
// about a turn) and a trap for tests: sharing a device id shares its key, so a
// leader still blocked when one test ends holds the key into the next, and the
// symptom lands in a test that has nothing to do with it.
func aDeviceOfItsOwn(t *testing.T) string {
	t.Helper()
	return "laptop-" + t.Name()
}

// fleet runs n concurrent skill_exec calls, the way n agents of one fleet do,
// and hands back what each was told.
func fleet(t *testing.T, lib Library, machines machine.Machines, n int) []answer {
	t.Helper()
	device := aDeviceOfItsOwn(t)
	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	handler := NewExec(lib, machines, []int64{1}).Handle

	answers := make([]answer, n)
	var running sync.WaitGroup
	for i := 0; i < n; i++ {
		running.Add(1)
		go func(at int) {
			defer running.Done()
			res, err := handler(context.Background(), tool.Call{
				WorkspaceID: 1, UserID: 2, DeviceID: device, Args: raw,
			})
			if err != nil {
				answers[at] = answer{why: err.Error()}
				return
			}
			got := answer{kind: res.Err}
			if err := json.Unmarshal(res.Content, &got.body); err != nil {
				got.why = fmt.Sprintf("unreadable: %v", err)
			} else {
				got.why, _ = got.body["error"].(string)
			}
			answers[at] = got
		}(i)
	}
	running.Wait()
	return answers
}

// THE CASE THIS EXISTS FOR. Twenty agents of one fleet reach for one skill that
// is not on the computer, and ONE copy of it crosses the link.
func TestAFleetReachingForOneSkillInstallsItOnce(t *testing.T) {
	lib := stocked()
	c := newCounted()
	// Held until every caller has arrived, so they are genuinely concurrent
	// rather than twenty calls that happened to be quick.
	c.release = make(chan struct{})
	go func() {
		// Long enough for the others to pile up behind the leader.
		time.Sleep(150 * time.Millisecond)
		close(c.release)
	}()

	answers := fleet(t, lib, c, 20)
	for at, got := range answers {
		if got.kind != tool.ErrorNone {
			t.Fatalf("agent %d failed: %s", at, got.why)
		}
	}

	if sent := c.installs.Load(); sent != 1 {
		t.Errorf("the package crossed the link %d times, want once", sent)
	}
	// And every agent's script ran: waiting is not skipping.
	c.mu.Lock()
	ran := len(c.ran)
	c.mu.Unlock()
	if ran != 20 {
		t.Errorf("%d scripts ran, want 20", ran)
	}
	// The package was read out of the store once, too: the point is not only
	// the bytes on the wire.
	lib.mu.Lock()
	read := lib.read
	lib.mu.Unlock()
	c.mu.Lock()
	inPackage := len(c.installed[10])
	c.mu.Unlock()
	if read > inPackage {
		t.Errorf("the store was read %d times for one package of %d files",
			read, inPackage)
	}
}

// And a version already on the computer costs no install at all, which is the
// ordinary case after the first agent has been through.
func TestAFleetReachingForASkillAlreadyThereInstallsNothing(t *testing.T) {
	lib := stocked()
	c := newCounted()
	c.mu.Lock()
	c.installed[10] = map[string]string{"scripts/extract.py": "x"}
	c.mu.Unlock()

	for at, got := range fleet(t, lib, c, 8) {
		if got.kind != tool.ErrorNone {
			t.Fatalf("agent %d failed: %s", at, got.why)
		}
	}
	if sent := c.installs.Load(); sent != 0 {
		t.Errorf("it installed %d times over a version already there", sent)
	}
}

// An install that fails tells everybody waiting, and does NOT leave the version
// marked as impossible: the next turn tries again.
func TestAFailedInstallIsToldToEverybodyAndNotRemembered(t *testing.T) {
	lib := stocked()
	c := newCounted()
	c.release = make(chan struct{})
	c.mu.Lock()
	c.failGet = "there is no room on this disk"
	c.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(c.release)
	}()

	for at, got := range fleet(t, lib, c, 6) {
		if got.kind == tool.ErrorNone {
			t.Fatalf("agent %d reported success over a failed install", at)
		}
		if !strings.Contains(got.why, "no room on this disk") {
			t.Errorf("agent %d was told %q, not the reason", at, got.why)
		}
	}
	// One attempt for the six of them.
	if sent := c.installs.Load(); sent != 1 {
		t.Errorf("it tried %d times, want once", sent)
	}
	// And the key is free, so a later turn tries again rather than the fleet
	// being told for ever that a package it barely tried is broken.
	c.mu.Lock()
	c.failGet = ""
	c.mu.Unlock()
	if got := fleet(t, lib, c, 1)[0]; got.kind != tool.ErrorNone {
		t.Errorf("a later call was refused without trying: %s", got.why)
	}
	if sent := c.installs.Load(); sent != 2 {
		t.Errorf("the second attempt did not happen (%d installs)", sent)
	}
}

// The ceiling on waiting. A leader that never finishes must not hold the others
// for as long as their turns live, and what they get is an ordinary tool error.
func TestAWaiterGivesUpAndSaysSo(t *testing.T) {
	was := waitForInstall
	waitForInstall = 200 * time.Millisecond
	t.Cleanup(func() { waitForInstall = was })

	lib := stocked()
	c := newCounted()
	// Never released: the leader is still installing when the waiter's ceiling
	// arrives.
	c.release = make(chan struct{})
	t.Cleanup(func() { close(c.release) })

	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	handler := NewExec(lib, c, []int64{1}).Handle
	call := tool.Call{WorkspaceID: 1, UserID: 2, DeviceID: aDeviceOfItsOwn(t), Args: raw}

	// The leader, left hanging in its install.
	go func() { _, _ = handler(context.Background(), call) }()
	waitFor(t, func() bool { return c.installs.Load() == 1 })

	started := time.Now()
	res, err := handler(context.Background(), call)
	if err != nil {
		t.Fatalf("system error: %v", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the waiter waited %s: the ceiling did not apply", took)
	}
	if !res.Failed() {
		t.Fatal("the waiter reported success while the install was still going")
	}
	// An ordinary tool error the model reads, saying the skill is not at fault.
	if !strings.Contains(string(res.Content), "still being put on your computer") {
		t.Errorf("the answer does not say what happened: %s", res.Content)
	}
	// It did not push a second copy to get around the wait.
	if sent := c.installs.Load(); sent != 1 {
		t.Errorf("%d installs: the waiter pushed its own copy", sent)
	}
}

// A waiter whose own turn is cancelled stops waiting, rather than holding a
// goroutine until the leader is done.
func TestAWaiterStopsWhenItsOwnTurnIsCancelled(t *testing.T) {
	lib := stocked()
	c := newCounted()
	c.release = make(chan struct{})
	t.Cleanup(func() { close(c.release) })

	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	handler := NewExec(lib, c, []int64{1}).Handle
	call := tool.Call{WorkspaceID: 1, UserID: 2, DeviceID: aDeviceOfItsOwn(t), Args: raw}

	go func() { _, _ = handler(context.Background(), call) }()
	waitFor(t, func() bool { return c.installs.Load() == 1 })

	stopping, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = handler(stopping, call)
	}()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled waiter went on waiting")
	}
}

// Two versions install at the SAME TIME, which is why the gate is keyed on the
// version rather than on the skill.
//
// Keyed on the skill, an agent needing version 20 would wait for version 10's
// install and still be missing when it finished. That happens when somebody
// re-imports a package while a fleet is running.
func TestTwoVersionsDoNotWaitForEachOther(t *testing.T) {
	lib := stocked()
	// The second skill needs a script of its own to be asked for.
	lib.files[20] = append(lib.files[20], &model.SkillFile{
		ID: 201, Path: "scripts/other.py", FileType: model.SkillFileScript, SizeBytes: 10,
	})
	lib.text[201] = "print('other')\n"

	c := newCounted()
	c.release = make(chan struct{})

	device := aDeviceOfItsOwn(t)
	start := func(skill, script string, allowed []int64) {
		raw, _ := json.Marshal(map[string]any{"skill": skill, "script": script})
		go func() {
			_, _ = NewExec(lib, c, allowed).Handle(context.Background(), tool.Call{
				WorkspaceID: 1, UserID: 2, DeviceID: device, Args: raw,
			})
		}()
	}
	start("pdf-processing", "scripts/extract.py", []int64{1})
	waitFor(t, func() bool { return c.installs.Load() == 1 })

	// The second version's install begins while the first is still held, which
	// it could not do if the two shared a key.
	start("csv-tools", "scripts/other.py", []int64{2})
	waitFor(t, func() bool { return c.installs.Load() == 2 })
	close(c.release)
}

// waitFor is a short poll, for a condition another goroutine brings about.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition never came about")
}

// A waiter that runs out of patience asks the computer anyway, because the
// likeliest reason to be here is an install that landed while the leader was
// still finishing up.
//
// Without that extra question this is a turn that failed for nothing: the skill
// is on the disk, the script would have run, and the agent is told to try again.
func TestAWaiterOutOfPatienceAsksAnyway(t *testing.T) {
	was := waitForInstall
	waitForInstall = 200 * time.Millisecond
	t.Cleanup(func() { waitForInstall = was })

	lib := stocked()
	c := newCounted()
	// Two holds, and the ORDER is the whole test. The leader is stopped before
	// the package lands, so the waiter arrives to a computer that really does
	// not have it and goes into the gate; the package then lands while the
	// waiter is in there; and the leader is stopped again afterwards so it
	// never releases the key and the waiter has to run out of patience.
	//
	// Getting this wrong is a test that passes for the wrong reason: land the
	// package first and the waiter's own first question finds it, so the branch
	// under test is never reached. That version passed in 10ms, which was the
	// clue: it cannot have waited 200.
	c.release = make(chan struct{})
	c.landed = make(chan struct{})
	t.Cleanup(func() { close(c.landed) })

	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	device := aDeviceOfItsOwn(t)
	handler := NewExec(lib, c, []int64{1}).Handle
	call := tool.Call{WorkspaceID: 1, UserID: 2, DeviceID: device, Args: raw}

	go func() { _, _ = handler(context.Background(), call) }()
	waitFor(t, func() bool { return c.installs.Load() == 1 })

	// The package lands a quarter of the way into the wait.
	go func() {
		time.Sleep(waitForInstall / 4)
		close(c.release)
	}()

	began := time.Now()
	res, err := handler(context.Background(), call)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("the waiter got an error: %v", err)
	}
	if res.Err != "" {
		var body map[string]any
		_ = json.Unmarshal(res.Content, &body)
		t.Fatalf("the waiter was refused a skill that is on the computer: %v (%s)",
			body["error"], res.Err)
	}
	// It got there by running out of patience, not by finding it on the way in.
	if took < waitForInstall {
		t.Fatalf("it answered in %v, sooner than the %v wait: it never went into the gate",
			took, waitForInstall)
	}
	if got := c.installs.Load(); got != 1 {
		t.Fatalf("it installed %d times, want once", got)
	}
}

// Two installs of one key NEVER run at the same time, under load.
//
// This is the invariant the whole arrangement exists for, and it is worth
// asserting directly rather than only through its consequences. The tests above
// count installs, which is a consequence and depends on when callers happen to
// arrive; this one watches the thing itself. `put` records how many copies of
// itself are running and the high-water mark must be one, over a hundred
// thousand arrivals with installs constantly starting and finishing underneath
// them.
//
// It is also the test that covers releasing the key, which cannot be driven
// deterministically: the interesting moment is the microsecond in which a
// leader is finishing while somebody else arrives. That moment cannot be
// paused without a hook in production code, so it is hit by volume instead.
func TestTwoInstallsOfOneKeyNeverOverlap(t *testing.T) {
	var running, most atomic.Int64
	var installs atomic.Int64
	put := func() error {
		installs.Add(1)
		now := running.Add(1)
		for {
			high := most.Load()
			if now <= high || most.CompareAndSwap(high, now) {
				break
			}
		}
		// Long enough that an overlapping install would be caught rather than
		// slipping between two instants.
		time.Sleep(20 * time.Microsecond)
		running.Add(-1)
		return nil
	}

	key := installKey{workspaceID: 1, userID: 2, deviceID: aDeviceOfItsOwn(t), versionID: 10}
	const arrivals, rounds = 50, 2000
	var done sync.WaitGroup
	for i := 0; i < arrivals; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			for r := 0; r < rounds; r++ {
				if err := gate.once(context.Background(), key, put); err != nil {
					t.Errorf("once: %v", err)
					return
				}
			}
		}()
	}
	done.Wait()

	if high := most.Load(); high != 1 {
		t.Fatalf("%d installs of one key ran at the same time, want never more than 1", high)
	}
	// And the gate was actually exercised rather than every caller being a
	// leader in turn: far fewer installs than arrivals means callers really did
	// find a flight in progress and wait for it.
	t.Logf("%d arrivals shared %d installs", arrivals*rounds, installs.Load())
	if installs.Load() >= arrivals*rounds {
		t.Fatalf("%d installs for %d arrivals: nobody ever waited, so this proves nothing",
			installs.Load(), arrivals*rounds)
	}
	// The key is not left behind.
	gate.mu.Lock()
	left := len(gate.running)
	gate.mu.Unlock()
	if left != 0 {
		t.Errorf("%d keys were left in the map", left)
	}
}
