package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tools/machine"
)

// What the person's computer is, all the way from the application's own words
// to the instructions the model is handed.
//
// The unit tests either side of this each prove half of it and neither proves
// the seam: the Rust half can describe a machine correctly into a JSON shape
// the Go half does not read, and the Go half can render a paragraph from a
// struct nothing ever fills in. Both would be green.
//
// So the payload here is not written by hand. testdata/machine_env.json is what
// `cargo run -p sag-desktop --example machine_env` printed on a real machine,
// with the home directory and hostname replaced (they name whoever ran it).
// Regenerate it the same way if the Rust struct changes; a renamed key then
// leaves a zero value here and the assertions below say which one.
func TestTheComputerReachesTheModelEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A computer, linked, declaring the tools at the versions we speak, so the
	// turn actually holds one that runs there. Without that the paragraph is
	// deliberately withheld, which is the next assertion down.
	env.app.Machines = linkedMachine{runs: machine.Versions()}

	said := []string{
		`{"choices":[{"index":0,"delta":{"content":"Noted."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	vendor := newFakeVendor(t, said, said, said)
	placeholder := newFakeVendor(t)
	modelID := env.registerModel(placeholder)
	env.pointModelAt(modelID, vendor)

	rec := env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": "default", "name": "Gateway",
		"tools": []string{"terminal", "machine_info", "current_time"},
	})
	env.expectStatus(rec, http.StatusCreated)

	raw, err := os.ReadFile("testdata/machine_env.json")
	if err != nil {
		t.Fatalf("read the machine the Rust half described: %v", err)
	}
	var described map[string]any
	if err := json.Unmarshal(raw, &described); err != nil {
		t.Fatalf("the recorded machine is not JSON: %v", err)
	}

	env.streamTurn(token, map[string]any{
		"prompt": "hello", "model_id": modelID,
		"device_id": "the-laptop", "machine": described,
	})

	system := vendor.sentSystemPromptAsking("hello")
	// Each of these is something a model guessing from the median of what it
	// has read would get wrong, and each had to survive the whole path: the
	// Rust struct's field name, the JSON key, the Go tag, the render.
	for _, want := range []string{
		"You are working on a macOS computer",
		"a-laptop",
		"/bin/zsh",
		"Installed: ",
		"Not installed: ",
		"not what you are allowed to run",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the instructions never said %q.\n--- system prompt ---\n%s", want, system)
		}
	}
	// A program the example found, and one it did not, from the same file. A
	// rendered paragraph that dropped one of the two lists would still pass the
	// "Installed:" check above.
	if !strings.Contains(system, "git") {
		t.Errorf("a program the machine reported having never reached the model:\n%s", system)
	}
	if !strings.Contains(system, "pwsh") {
		t.Errorf("a program the machine reported lacking never reached the model:\n%s", system)
	}

	// The other side of it, which is what stops this proving only that a string
	// can be concatenated: a browser sends no machine, so nothing is said about
	// one rather than something invented.
	env.streamTurn(token, map[string]any{"prompt": "from a browser", "model_id": modelID})
	if browser := vendor.sentSystemPromptAsking("from a browser"); strings.Contains(browser, "You are working on") {
		t.Errorf("a browser was told about a computer:\n%s", browser)
	}
}
