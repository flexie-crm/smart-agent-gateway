package toolkit

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/tool"
)

// Each helper must carry the right error classification, because the loop keys
// its behaviour off it: only a bad-arguments result teaches, and only a
// transient one invites a retry. A miscategorised result sends the loop the
// wrong way.
func TestResultClassification(t *testing.T) {
	cases := []struct {
		name  string
		build func() (tool.Result, error)
		kind  tool.ErrorKind
		retry bool
	}{
		{"bad args", func() (tool.Result, error) { return BadArguments("nope") }, tool.ErrorBadArguments, false},
		{"denied", func() (tool.Result, error) { return Denied() }, tool.ErrorDenied, false},
		{"blocked", func() (tool.Result, error) { return Blocked("nope") }, tool.ErrorBlocked, false},
		{"transient", func() (tool.Result, error) { return Transient("later") }, tool.ErrorTransient, true},
		{"failed", func() (tool.Result, error) { return Failed("nope") }, tool.ErrorFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if res.Err != tc.kind {
				t.Fatalf("kind = %q, want %q", res.Err, tc.kind)
			}
			if !res.Failed() {
				t.Fatal("an error result must report Failed()")
			}
			var payload struct {
				Success bool `json:"success"`
				Retry   bool `json:"retry"`
			}
			if err := json.Unmarshal(res.Content, &payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if payload.Success {
				t.Fatal("an error result must not report success")
			}
			if payload.Retry != tc.retry {
				t.Fatalf("retry = %v, want %v", payload.Retry, tc.retry)
			}
		})
	}
}

func TestSuccessIsNotAFailure(t *testing.T) {
	res, err := Success(map[string]any{"status": 200})
	if err != nil {
		t.Fatalf("success: %v", err)
	}
	if res.Failed() || res.Err != tool.ErrorNone {
		t.Fatalf("a success carries no error: %+v", res)
	}
}
