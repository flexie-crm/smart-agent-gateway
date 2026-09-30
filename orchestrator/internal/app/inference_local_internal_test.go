package app

import (
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
)

// What the Inference screen may say exists.
//
// A personal installation runs a node of its own only where an engine shipped
// with the application. Upgrade an installation that HAD one to a build that
// does not, and the row it registered stays behind: a machine called "This
// computer", permanently unreachable, on the one screen whose job is to say
// which machines are up. That is exactly what somebody saw.

func nodesAt(addresses ...string) []*model.InferenceNode {
	rows := make([]*model.InferenceNode, len(addresses))
	for i, address := range addresses {
		rows[i] = &model.InferenceNode{ID: int64(i + 1), BaseURL: address}
	}
	return rows
}

func names(rows []*model.InferenceNode) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.BaseURL
	}
	return out
}

func TestThisComputerIsNotAMachineOnABuildThatCarriesNoEngine(t *testing.T) {
	rows := nodesAt(
		"http://127.0.0.1:8081/v1",
		"http://localhost:8081/v1",
		"http://[::1]:8081/v1",
		"https://gpu-1.example.com/v1",
	)

	// Both sides of the constant, in one test, so it states the rule rather than
	// the answer on whichever platform it happens to run on. Without that, the
	// half that matters is the half that never executes here.
	a := &App{Config: &config.Config{Personal: true}}
	kept := names(a.withoutAMachineThatCannotExistHere(rows))

	if config.EngineBundled {
		if len(kept) != 4 {
			t.Fatalf("a build WITH an engine hid one of its own machines: %v", kept)
		}
		return
	}
	if len(kept) != 1 || kept[0] != "https://gpu-1.example.com/v1" {
		t.Fatalf("expected only the machine that is somewhere else, got %v", kept)
	}
}

// And a deployment keeps everything, on every platform. Its own gateway is
// reachable, so a machine at loopback there is a machine somebody ran beside it
// on purpose, not a fossil.
func TestADeploymentKeepsAMachineOnItsOwnHost(t *testing.T) {
	a := &App{Config: &config.Config{Personal: false}}
	kept := names(a.withoutAMachineThatCannotExistHere(nodesAt("http://127.0.0.1:8081/v1")))
	if len(kept) != 1 {
		t.Fatalf("a deployment hid a machine on its own host: %v", kept)
	}
}

// A host that merely begins with one of the loopback names is somewhere else.
// Matching on a prefix would have hidden it.
func TestAHostThatOnlyLooksLikeLoopbackIsKept(t *testing.T) {
	a := &App{Config: &config.Config{Personal: true}}
	for _, address := range []string{
		"https://127.0.0.1.example.com/v1",
		"https://localhost.example.com/v1",
		"https://notlocalhost/v1",
	} {
		if kept := a.withoutAMachineThatCannotExistHere(nodesAt(address)); len(kept) != 1 {
			t.Fatalf("%s was read as this computer", address)
		}
	}
}

// And that the list actually goes through it.
//
// Not a formality. The three tests above call the rule directly, so they all
// went on passing when the one line applying it was taken out of Nodes: they
// prove the rule and say nothing about whether anything obeys it, which is the
// exact shape of a measurement that cannot come out the other way.
//
// Asserted from the source because the honest alternative is not available
// cheaply: Nodes takes a 25-method Store and probes every row over the network,
// so a behavioural test here means a fake for all of it plus a connection
// attempt per machine, and the slow, network-dependent test that results is one
// somebody eventually deletes.
func TestTheListAppliesTheRule(t *testing.T) {
	source, err := os.ReadFile("inference.go")
	if err != nil {
		t.Fatal(err)
	}
	const applied = "rows = a.withoutAMachineThatCannotExistHere(rows)"
	if !strings.Contains(string(source), applied) {
		t.Fatalf("Nodes no longer applies the rule: %q is gone, so a build with no "+
			"engine lists this computer again", applied)
	}
}
