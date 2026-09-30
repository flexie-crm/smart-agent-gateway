package skills

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	mlink "flexie.io/sag/internal/link"
	"flexie.io/sag/internal/linktest"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// The ceiling on waiting, against the REAL chat application.
//
// The unit tests prove the ceiling with a computer written here in Go, which is
// enough to show the timer works and not enough to show what an agent actually
// gets. These two drive the real application: a real package really crosses the
// link and really lands on a real disk, and the only thing standing in for
// anything is a slow machine, which is a wrapper holding the install's ANSWER
// back rather than faking the install.
//
// There are two ways to run out of patience and they must not be one behaviour.
// The package has landed and the leader is merely slow to finish: asking once
// more gets a working script, and giving up there would be a turn that failed
// for nothing. The package has NOT landed: there is nothing to run, and the
// agent is told so in a way it can act on.
//
//	SAG_LINK_E2E=1 make link-e2e

func TestTheCeilingAgainstTheRealApplication(t *testing.T) {
	linktest.Skip(t)

	// Short enough to run, long enough that nothing else can finish inside it
	// by accident. Two minutes is right for the product and would make this a
	// test nobody runs.
	was := waitForInstall
	waitForInstall = 400 * time.Millisecond
	t.Cleanup(func() { waitForInstall = was })

	t.Run("the package landed while the leader was still finishing", func(t *testing.T) {
		const device = "ceiling-landed"
		far, release := realComputer(t, device)
		// It goes through a quarter of the way into the wait, so the waiter is
		// already in the gate when it lands, and the leader is then held so it
		// never releases the key. That is the shape being tested: on the disk,
		// but nobody has said so yet.
		far.before = func() { time.Sleep(waitForInstall / 4) }
		far.after = func() { <-release }

		lib := stocked()
		handler := NewExec(lib, far, []int64{1}).Handle
		call := callFor(device)

		go func() { _, _ = handler(context.Background(), call) }()
		waitForCount(t, &far.installs, 1)

		began := time.Now()
		res, err := handler(context.Background(), call)
		took := time.Since(began)
		if err != nil {
			t.Fatalf("the waiter got an error: %v", err)
		}
		if res.Err != "" {
			t.Fatalf("the waiter was refused a skill that is on the computer: %s\n%s",
				res.Err, res.Content)
		}
		if took < waitForInstall {
			t.Fatalf("it answered in %v, sooner than the %v wait: it never ran out of patience",
				took, waitForInstall)
		}
		// One install, and the script really ran on the real machine.
		if got := far.installs.Load(); got != 1 {
			t.Errorf("the package crossed the link %d times, want once", got)
		}
		if !strings.Contains(string(res.Content), "extract.py") {
			t.Errorf("the script does not look like it ran:\n%s", res.Content)
		}
	})

	t.Run("the package had not landed when patience ran out", func(t *testing.T) {
		const device = "ceiling-stuck"
		far, release := realComputer(t, device)
		// Held BEFORE it goes through, so nothing reaches the disk at all.
		far.before = func() { <-release }

		lib := stocked()
		handler := NewExec(lib, far, []int64{1}).Handle
		call := callFor(device)

		go func() { _, _ = handler(context.Background(), call) }()
		waitForCount(t, &far.installs, 1)

		res, err := handler(context.Background(), call)
		if err != nil {
			t.Fatalf("the waiter got an error: %v", err)
		}
		if res.Err == "" {
			t.Fatalf("a skill that is not on the computer ran anyway:\n%s", res.Content)
		}
		// What the agent is told matters as much as that it failed: nothing is
		// wrong with the skill, and trying again is the right next move.
		said := string(res.Content)
		if !strings.Contains(said, "still being put on your computer") ||
			!strings.Contains(said, "try again") {
			t.Errorf("the agent was not told it can try again:\n%s", said)
		}
	})
}

// realComputer is the person's actual machine: the real registry, the real
// application running as its own process, and a hold on the install so a
// waiter can be made to run out of patience.
func realComputer(t *testing.T, device string) (*heldMachines, chan struct{}) {
	t.Helper()
	const token = "a-real-looking-token"
	registry := mlink.NewRegistry(zerolog.Nop(), func(given string) (int64, int64, string, time.Time, bool) {
		if given == token {
			return 2, 1, device, time.Now().Add(time.Hour), true
		}
		return 0, 0, "", time.Time{}, false
	}, []string{"*"})
	linktest.Start(t, linktest.Serve(t, registry), token)
	linktest.Await(t, "the real chat application to link", func() bool {
		return registry.Online(1, 2, device)
	})
	if runs := registry.Runs(1, 2, device); runs[machine.SkillRunName] == 0 {
		t.Skip("this build of the application does not run skills")
	}
	// Closed at the end so the held leader is let go rather than left on a
	// channel nobody will ever close.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return &heldMachines{Machines: registry}, release
}

// heldMachines is the real link with the install slowed down.
//
// Nothing is faked: the call goes through to the real application and the real
// package lands on the real disk. What is held is the ANSWER, which is what a
// machine on a slow network or a laptop going to sleep does to a caller.
type heldMachines struct {
	machine.Machines
	installs atomic.Int64
	before   func()
	after    func()

	mu       sync.Mutex
	versions []int64 // which version each install carried, in arrival order
}

func (h *heldMachines) Call(ctx context.Context, workspaceID, userID int64, deviceID, name string,
	args json.RawMessage, reason string,
) (mlink.Result, error) {
	if name != machine.SkillInstallName {
		return h.Machines.Call(ctx, workspaceID, userID, deviceID, name, args, reason)
	}
	h.installs.Add(1)
	var carried struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(args, &carried)
	h.mu.Lock()
	h.versions = append(h.versions, carried.Version)
	h.mu.Unlock()
	if h.before != nil {
		h.before()
	}
	res, err := h.Machines.Call(ctx, workspaceID, userID, deviceID, name, args, reason)
	if h.after != nil {
		h.after()
	}
	return res, err
}

func callFor(device string) tool.Call {
	raw, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	return tool.Call{WorkspaceID: 1, UserID: 2, DeviceID: device, Args: raw}
}

func waitForCount(t *testing.T, counter *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 10s for the count to reach %d, it is %d", want, counter.Load())
}

// An install the real application REFUSES: everybody waiting is told, and the
// key is let go so the next turn tries again.
//
// The refusal is real rather than injected. The package's stored hash is wrong,
// so the application writes the file, hashes what arrived, finds it does not
// match and throws the whole staging tree away, which is the same path a
// truncated transfer takes. The unit tests cover this with a computer that
// answers "no"; what they cannot show is that the far end's own refusal reaches
// five waiting agents as an error rather than as a hang or a wrong success.
func TestARefusedInstallReachesEveryWaiterAgainstTheRealApplication(t *testing.T) {
	linktest.Skip(t)
	was := waitForInstall
	waitForInstall = 30 * time.Second // long: the ceiling must not be what ends this
	t.Cleanup(func() { waitForInstall = was })

	const device = "refused-install"
	far, _ := realComputer(t, device)

	// Held briefly BEFORE the install goes through, so all five callers are
	// genuinely inside the gate when it fails. Without this the refusal comes
	// back so fast that each caller can be its own leader in turn, and the
	// test would pass while proving only that five calls failed, which is not
	// what it says it proves.
	far.before = func() { time.Sleep(250 * time.Millisecond) }

	lib := stocked()
	// One file that cannot arrive intact, which is a whole package that cannot
	// be installed: the tree is published or it is not.
	lib.files[10][0].SHA256 = "not-the-hash-of-anything"

	handler := NewExec(lib, far, []int64{1}).Handle
	call := callFor(device)

	const waiters = 5
	type outcome struct {
		kind string
		body string
	}
	got := make([]outcome, waiters)
	var ready, done sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < waiters; i++ {
		ready.Add(1)
		done.Add(1)
		go func(at int) {
			defer done.Done()
			ready.Done()
			<-release
			res, err := handler(context.Background(), call)
			if err != nil {
				got[at] = outcome{kind: "error", body: err.Error()}
				return
			}
			got[at] = outcome{kind: string(res.Err), body: string(res.Content)}
		}(i)
	}
	ready.Wait()
	close(release)
	done.Wait()

	// They WAITED for it rather than each trying their own: one install for
	// five callers is the whole claim, and without this the assertions below
	// pass with no gate in the code.
	if put := far.installs.Load(); put != 1 {
		t.Fatalf("%d installs for %d callers, want one: they did not wait for each other",
			put, waiters)
	}
	for at, one := range got {
		if one.kind == "" {
			t.Errorf("waiter %d was told the install worked:\n%s", at, one.body)
			continue
		}
		// It says the skill could not be put on the computer, and it says why,
		// which is the far end's own words about arrival.
		if !strings.Contains(one.body, "could not be put on your computer") ||
			!strings.Contains(one.body, "intact") {
			t.Errorf("waiter %d was not told why:\n%s", at, one.body)
		}
	}

	// The key was let go: a good package installs on the very next call, with
	// nothing left over from the failure.
	installsBefore := far.installs.Load()
	lib.files[10][0].SHA256 = ""
	res, err := handler(context.Background(), call)
	if err != nil {
		t.Fatalf("the retry errored: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("the retry after a failed install was refused: %s\n%s", res.Err, res.Content)
	}
	if put := far.installs.Load() - installsBefore; put != 1 {
		t.Errorf("the retry installed %d times, want once", put)
	}
}

// A waiter whose own turn is cancelled stops waiting, rather than holding on
// for a leader nobody is listening for any more.
func TestACancelledTurnStopsWaitingAgainstTheRealApplication(t *testing.T) {
	linktest.Skip(t)
	was := waitForInstall
	waitForInstall = 30 * time.Second // the ceiling must not be what ends this
	t.Cleanup(func() { waitForInstall = was })

	const device = "cancelled-waiter"
	far, release := realComputer(t, device)
	far.before = func() { <-release } // the leader never finishes

	lib := stocked()
	handler := NewExec(lib, far, []int64{1}).Handle
	call := callFor(device)

	go func() { _, _ = handler(context.Background(), call) }()
	waitForCount(t, &far.installs, 1)

	mine, stop := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		stop()
	}()
	began := time.Now()
	_, _ = handler(mine, call)
	took := time.Since(began)

	// Promptly, and nowhere near either the ceiling or the leader's hold.
	if took > 5*time.Second {
		t.Fatalf("a cancelled turn waited %v", took)
	}
	if took < 100*time.Millisecond {
		t.Fatalf("it came back in %v, before the cancel: it never waited at all", took)
	}
}

// Two versions install at the same time and neither waits for the other, which
// is why the gate is keyed on the version rather than on the skill.
//
// On the real client this is also a claim about the disk: two versions are two
// directories and two staging directories, so they have nothing to contend
// over. Keyed on the skill, the second would sit behind the first and still be
// missing when it finished.
func TestTwoVersionsDoNotBlockEachOtherAgainstTheRealApplication(t *testing.T) {
	linktest.Skip(t)
	was := waitForInstall
	waitForInstall = 30 * time.Second
	t.Cleanup(func() { waitForInstall = was })

	const device = "two-versions"
	far, release := realComputer(t, device)
	far.after = func() { <-release } // both leaders are held after landing

	lib := stocked()
	// csv-tools ships no script, so give version 20 one. Done here rather than
	// in the shared fixture, which other tests assert the contents of.
	lib.files[20] = append(lib.files[20], &model.SkillFile{
		ID: 201, Path: "scripts/extract.py", FileType: model.SkillFileScript, SizeBytes: 30,
	})
	lib.text[201] = "import sys\nprint(sys.argv)\n"

	handler := NewExec(lib, far, []int64{1, 2}).Handle
	first, _ := json.Marshal(map[string]any{
		"skill": "pdf-processing", "script": "scripts/extract.py",
	})
	second, _ := json.Marshal(map[string]any{
		"skill": "csv-tools", "script": "scripts/extract.py",
	})

	go func() {
		_, _ = handler(context.Background(), tool.Call{
			WorkspaceID: 1, UserID: 2, DeviceID: device, Args: first,
		})
	}()
	waitForCount(t, &far.installs, 1)

	// The second version's install must START while the first is still held.
	// If it were waiting for the first, this count would never reach two.
	go func() {
		_, _ = handler(context.Background(), tool.Call{
			WorkspaceID: 1, UserID: 2, DeviceID: device, Args: second,
		})
	}()
	waitForCount(t, &far.installs, 2)

	far.mu.Lock()
	versions := append([]int64(nil), far.versions...)
	far.mu.Unlock()
	if len(versions) != 2 || versions[0] == versions[1] {
		t.Fatalf("the two installs were for versions %v, want two different ones", versions)
	}
}
