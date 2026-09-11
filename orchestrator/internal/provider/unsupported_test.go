package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// The promise printed under the checkbox is that a model without reasoning
// ignores the setting. On a strict wire that is not free: the request is
// REFUSED. These pin that we make the promise true rather than print it.

// newResponsesFor points the Responses adapter at a scripted vendor.
func newResponsesFor(t *testing.T, baseURL string) *OpenAIResponses {
	t.Helper()
	p, err := NewOpenAIResponses(dialects[model.VendorOpenAI], "sk-test", baseURL, nil)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	return p
}

func TestAStrictVendorRefusingReasoningIsAskedAgainWithoutIt(t *testing.T) {
	var attempts []bool // whether each attempt carried the reasoning parameter

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, asked := body["reasoning"]
		attempts = append(attempts, asked)

		if asked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'reasoning' is not supported with this model.","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"type":"response.output_text.delta","delta":"the answer"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
		} {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
	}))
	defer srv.Close()

	p := newResponsesFor(t, srv.URL)
	text, err := drain(t, p, GenerateRequest{
		Model:     "a-model-that-cannot-think",
		Reasoning: true,
		Settings:  model.Settings{"reasoning_effort": "high"},
		Messages:  []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("the turn failed instead of degrading: %v", err)
	}
	if text != "the answer" {
		t.Fatalf("content %q, want the answer", text)
	}
	if len(attempts) != 2 || !attempts[0] || attempts[1] {
		t.Fatalf("attempts %v, want one with reasoning then one without", attempts)
	}

	// And it is remembered, so the SECOND turn does not pay for the discovery.
	before := len(attempts)
	if _, err := drain(t, p, GenerateRequest{
		Model:     "a-model-that-cannot-think",
		Reasoning: true,
		Messages:  []Message{{Role: RoleUser, Content: "again"}},
	}); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if got := attempts[before:]; len(got) != 1 || got[0] {
		t.Fatalf("second turn made %v, want a single attempt with no reasoning", got)
	}
}

// The narrowness matters more than the retry. A refusal about anything else is
// a real error: retrying it silently would drop reasoning, answer without
// thinking, and never tell anybody the request was wrong.
func TestAnUnrelatedRefusalIsNotRetried(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid value for 'temperature'.","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	_, err := drain(t, newResponsesFor(t, srv.URL), GenerateRequest{
		Model: "m", Reasoning: true,
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("an unrelated refusal was swallowed")
	}
	if attempts != 1 {
		t.Fatalf("%d attempts, want 1: only a complaint about reasoning may be retried", attempts)
	}
}

func TestReadingAVendorsComplaint(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		{"Unsupported parameter: 'reasoning' is not supported with this model.", true},
		{"model does not support thinking", true},
		{"budget_tokens: extra inputs are not permitted", true},
		{"Invalid value for 'temperature'.", false},
		{"reasoning effort must be one of low, medium, high", false},
		{"rate limit exceeded", false},
		{"", false},
	} {
		got := isReasoningRefusal(errorString(c.text))
		if c.text == "" {
			got = isReasoningRefusal(nil)
		}
		if got != c.want {
			t.Fatalf("%q read as %v, want %v", c.text, got, c.want)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// drain reads a stream into its text, or the error it failed with.
func drain(t *testing.T, p Provider, req GenerateRequest) (string, error) {
	t.Helper()
	events, err := p.Stream(context.Background(), req)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for event := range events {
		switch event.Kind {
		case EventContentDelta:
			text.WriteString(event.ContentDelta)
		case EventError:
			return text.String(), event.Err
		}
	}
	return text.String(), nil
}
