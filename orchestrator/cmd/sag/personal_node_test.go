package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// Choosing between the two inference builds is the one place a desktop can
// silently do the wrong thing: pick the graphics build on a machine that cannot
// load it and there is no local inference at all, pick the processor build on a
// machine with a GPU and everything is a hundred times slower. So the choice is
// tested rather than trusted.

func nodeLogger() zerolog.Logger { return zerolog.New(io.Discard) }

// fakeNodeEnv turns this test binary into one of the two inference builds.
const fakeNodeEnv = "SAG_FAKE_NODE"

// brokenMarker sits beside a fake and says it cannot load its libraries.
const brokenMarker = ".broken"

// TestMain lets this binary stand in for an inference build.
//
// chooseNode decides by RUNNING the graphics build and watching what happens,
// so what it probes has to be a program. A two-line shell script is one on unix
// and is not on Windows, and the failure that produces is the bad kind: the
// probe fails because the file will not start, which looks exactly like a
// machine with no driver. Every test here went on passing, including the one
// asserting the fallback, which was then proving nothing about the fallback at
// all. A test that is right about the answer and wrong about the question is
// worth more attention than one that simply fails.
//
// Which build a copy is pretending to be cannot come from the environment: both
// fakes are children of the same process and would read the same value. It
// comes from a file beside the executable, so each carries its own behaviour.
func TestMain(m *testing.M) {
	if os.Getenv(fakeNodeEnv) == "1" {
		if complaint, err := os.ReadFile(os.Args[0] + brokenMarker); err == nil {
			// What a missing CUDA runtime produces: the process does not come up.
			fmt.Fprintln(os.Stderr, strings.TrimSpace(string(complaint)))
			os.Exit(127)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// probeGOOS is a platform whose naming matches the machine running the test.
//
// The goos argument to chooseNode decides only what it looks FOR. The probe it
// then runs is executed by THIS machine, so a test that exercises the probe has
// to name its fakes the way this machine can start them: Windows appends .exe
// when it is asked to start a file with no extension, and a fake without one is
// simply not found. "darwin" is not an option here because that branch
// deliberately never probes, which is what TestMacsSkipTheProbeEntirely is for.
func probeGOOS() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "linux"
}

// fakeNode puts a stand-in for one of the two builds where chooseNode looks for
// it, under the name that platform uses. `runs` is whether its `check`
// succeeds, which is exactly what a machine with a usable driver looks like
// from here.
func fakeNode(t *testing.T, dir, goos, name string, runs bool) {
	t.Helper()
	name = exeNameFor(name, goos)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self) //nolint:gosec // this very process
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if !runs {
		if err := os.WriteFile(path+brokenMarker,
			[]byte("error while loading shared libraries: libcuda.so.1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Set on this process, so a fake it spawns inherits it.
	t.Setenv(fakeNodeEnv, "1")
}

func TestTheGraphicsBuildIsUsedWhenItRuns(t *testing.T) {
	dir := t.TempDir()
	goos := probeGOOS()
	fakeNode(t, dir, goos, gpuNodeBinary, true)
	fakeNode(t, dir, goos, cpuNodeBinary, true)

	choice, err := chooseNode(context.Background(), dir, goos, nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !choice.accelerated {
		t.Fatalf("a machine that can run the graphics build was put on the processor: %+v", choice)
	}
	if filepath.Base(choice.binary) != exeNameFor(gpuNodeBinary, goos) {
		t.Fatalf("chose %s", filepath.Base(choice.binary))
	}
}

func TestAMachineWithNoDriverFallsBackAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	goos := probeGOOS()
	fakeNode(t, dir, goos, gpuNodeBinary, false) // cannot load its libraries
	fakeNode(t, dir, goos, cpuNodeBinary, true)

	choice, err := chooseNode(context.Background(), dir, goos, nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	if choice.accelerated {
		t.Fatal("a build that could not run was chosen anyway; there would be no local inference at all")
	}
	if filepath.Base(choice.binary) != exeNameFor(cpuNodeBinary, goos) {
		t.Fatalf("chose %s", filepath.Base(choice.binary))
	}
	// The person has to be told, or a model answering slowly is a mystery.
	if !strings.Contains(choice.reason, "NVIDIA") || !strings.Contains(choice.reason, "processor") {
		t.Fatalf("the reason does not explain the fallback: %q", choice.reason)
	}
}

func TestOnlyOneBuildInstalledIsTheOneUsed(t *testing.T) {
	gpuOnly := t.TempDir()
	fakeNode(t, gpuOnly, "linux", gpuNodeBinary, true)
	choice, err := chooseNode(context.Background(), gpuOnly, "linux", nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !choice.accelerated {
		t.Fatal("the only installed build was not used")
	}

	cpuOnly := t.TempDir()
	fakeNode(t, cpuOnly, "linux", cpuNodeBinary, true)
	choice, err = chooseNode(context.Background(), cpuOnly, "linux", nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	if choice.accelerated {
		t.Fatal("a processor build was reported as accelerated")
	}
}

func TestMacsSkipTheProbeEntirely(t *testing.T) {
	dir := t.TempDir()
	// A build that would FAIL the probe. On a Mac it is never run: Metal is on
	// every Mac we support, so there is one build and nothing to choose. Probing
	// here would be a pointless second launch on every start.
	fakeNode(t, dir, "darwin", gpuNodeBinary, false)
	fakeNode(t, dir, "darwin", cpuNodeBinary, true)

	choice, err := chooseNode(context.Background(), dir, "darwin", nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !choice.accelerated || filepath.Base(choice.binary) != gpuNodeBinary {
		t.Fatalf("a Mac should use the graphics build without probing, got %+v", choice)
	}
}

func TestNoInstalledNodeIsReportedRatherThanGuessed(t *testing.T) {
	_, err := chooseNode(context.Background(), t.TempDir(), "linux", nodeLogger())
	if err == nil {
		t.Fatal("an installation with no inference node must say so")
	}
}

func TestWindowsLooksForExecutables(t *testing.T) {
	dir := t.TempDir()
	fakeNode(t, dir, "windows", gpuNodeBinary, true)
	fakeNode(t, dir, "windows", cpuNodeBinary, true)

	choice, err := chooseNode(context.Background(), dir, "windows", nodeLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Without the .exe suffix neither file is found and a perfectly good
	// installation reports having no inference node.
	if filepath.Base(choice.binary) != gpuNodeBinary+".exe" {
		t.Fatalf("chose %s", filepath.Base(choice.binary))
	}
}

// A machine registers its address once and keeps its identity forever after, so
// a port that changes on every start leaves the row pointing at nothing. That
// shipped, and the Machines screen called a perfectly healthy node unreachable.
func TestTheNodeKeepsItsPortAcrossRestarts(t *testing.T) {
	state := t.TempDir()

	first, err := nodeAddress(state)
	if err != nil {
		t.Fatal(err)
	}
	again, err := nodeAddress(state)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("the node moved from %s to %s between starts; its registered address is now wrong", first, again)
	}
}
