package api

import (
	"testing"

	"flexie.io/sag/internal/tools/template"
)

// What a template said about an action has to reach the console whole.
//
// actionBody used to list the fields it copied out of ActionResult by hand,
// which made it a second declaration of the type that nothing held to the
// first. It drifted, and the way it drifts is by saying LESS than the result
// did, which no error can report: the body stays well formed and a field simply
// never arrives. It marshals the result now, so a field added later reaches the
// console because it exists rather than because somebody remembered this
// function.
func TestAnActionsResultReachesTheConsoleWhole(t *testing.T) {
	prompt := &template.ActionPrompt{Action: "verify", Token: "t-1", Title: "Enter the code"}
	body := actionBody(template.ActionResult{
		OK:      false,
		Message: "The service asked for a code.",
		Prompt:  prompt,
	})

	if body["ok"] != false {
		t.Fatalf("the outcome did not come back: %v", body)
	}
	if body["message"] != "The service asked for a code." {
		t.Fatalf("the message did not reach the console: %v", body)
	}
	if _, present := body["prompt"]; !present {
		t.Fatalf("an unfinished action did not ask for anything: %v", body)
	}
	// The older name for a failure's reason, which a client that only knows
	// about testing still reads.
	if body["error"] != "The service asked for a code." {
		t.Fatalf("the reason is missing under its older name: %v", body)
	}
}

// And it says nothing it was not given. An absent field stays absent rather
// than arriving as an empty string the console has to test for.
//
// This is the half that still discriminates against the hand-written version,
// which set "message" unconditionally and so handed over "" for every result
// that carried no message.
func TestNothingIsInventedForAnEmptyResult(t *testing.T) {
	body := actionBody(template.ActionResult{OK: true})
	for _, key := range []string{"message", "prompt", "error"} {
		if _, present := body[key]; present {
			t.Fatalf("%q was invented for a result that carried none: %v", key, body)
		}
	}
	if body["ok"] != true {
		t.Fatalf("the outcome itself is missing: %v", body)
	}
}
