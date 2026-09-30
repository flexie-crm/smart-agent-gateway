package apitool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

func build(t *testing.T, variant string, settings map[string]any) template.Instance {
	t.Helper()
	inst, err := New(nil).Build(template.Input{Alias: "billing", Variant: variant, Settings: settings})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return inst
}

// One configured API is one tool, named after it.
func TestBuildMakesOneToolPerApi(t *testing.T) {
	inst := build(t, AuthBearer, map[string]any{
		"base_url":     "https://api.example.com/v1/",
		"auth.token":   "sk_live_abc",
		"policy.mode":  Denylist,
		"policy.verbs": "POST\nDELETE\n",
	})

	if inst.Schema.Name != "api_billing" {
		t.Fatalf("the tool is called %q", inst.Schema.Name)
	}
	if inst.Schema.Kind != tool.KindCustom {
		t.Fatalf("kind = %q", inst.Schema.Kind)
	}
	// Reaching outside the workspace, and NOT raised because writes are
	// allowed: "this deletes something and cannot be undone" is a claim about a
	// remote service we cannot make, and the approval card is keyed to this.
	if inst.Schema.Risk != tool.RiskExternalCommunication {
		t.Fatalf("risk = %q", inst.Schema.Risk)
	}
	// A trailing slash somebody typed is KEPT, because a slash is part of an
	// address rather than noise on it: where the base is the whole endpoint, a
	// service that requires one answers a redirect or a 404 without it.
	// Joining is still one rule, because confinement compares without it.
	s, err := Parse(inst.Config)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.BaseURL != "https://api.example.com/v1/" {
		t.Fatalf("base = %q", s.BaseURL)
	}
	// The textarea became a list, sorted and normalised.
	if strings.Join(s.Policy.Verbs, ",") != "DELETE,POST" {
		t.Fatalf("verbs = %v", s.Policy.Verbs)
	}
	// The model's inputs are the fixed four, and nothing about the API's own
	// operations: that is the whole reason the spec lives in a skill.
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(inst.Schema.InputSchema, &schema); err != nil {
		t.Fatalf("input schema: %v", err)
	}
	// The fixed five: the verb, where, the parameters, the body and how the
	// body is encoded. Counted, so a sixth cannot be added without somebody
	// deciding to: this is the whole vocabulary a model has for one API, and
	// every addition is context spent on every call of every tool.
	for _, want := range []string{"method", "path", "query", "body", "body_type"} {
		if schema.Properties[want] == nil {
			t.Errorf("the tool does not take %q", want)
		}
	}
	if len(schema.Properties) != 5 {
		t.Fatalf("the tool takes %d inputs, want the fixed five: %v", len(schema.Properties), schema.Properties)
	}
	// The verb and nothing else. The path is optional on purpose: a tool whose
	// base address is the whole endpoint (a webhook listener) has nothing to
	// append, and a required path left a model with only "/" to send, which is
	// a different address.
	if strings.Join(schema.Required, ",") != "method" {
		t.Fatalf("required = %v", schema.Required)
	}
}

// A bad alias is refused, because the alias becomes the tool's name and a name
// is what a grant hangs off.
func TestABadAliasIsRefused(t *testing.T) {
	for _, alias := range []string{"", "Billing", "1billing", "billing-api", "billing api", strings.Repeat("a", 60)} {
		_, err := New(nil).Build(template.Input{
			Alias: alias, Variant: AuthNone,
			Settings: map[string]any{"base_url": "https://api.test", "policy.mode": Denylist},
		})
		if err == nil {
			t.Errorf("the alias %q was accepted", alias)
		}
	}
}

// Every secret the form collects is one the app is told to seal.
//
// Asserted against the FORM rather than a list written twice: a password field
// that nobody sealed would be a credential in plaintext in the database, and
// the way that happens is somebody adding a field and forgetting the other
// list.
func TestEverySecretFieldOnTheFormIsSealed(t *testing.T) {
	tpl := New(nil)
	for _, v := range tpl.Variants() {
		sections, err := tpl.Fields(v.Key)
		if err != nil {
			t.Fatalf("fields(%s): %v", v.Key, err)
		}
		sealed := map[string]bool{}
		for _, path := range tpl.SecretPaths(v.Key) {
			sealed[path] = true
		}
		for _, section := range sections {
			for _, f := range section.Fields {
				if f.Secret && !sealed[f.Key] {
					t.Errorf("%s: %q is a secret on the form and is NOT in SecretPaths, so it would be stored in plaintext", v.Key, f.Key)
				}
				if !f.Secret && sealed[f.Key] {
					t.Errorf("%s: %q is sealed but is not marked secret on the form, so the form would show it back", v.Key, f.Key)
				}
			}
		}
	}
}

// A configuration that could not work is refused before it is saved, so
// approval equals success.
func TestAConfigurationThatCouldNotWorkIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		variant  string
		settings map[string]any
	}{
		{"no address", AuthNone, map[string]any{"policy.mode": Denylist}},
		{"not a url", AuthNone, map[string]any{"base_url": "not a url", "policy.mode": Denylist}},
		{"the wrong scheme", AuthNone, map[string]any{"base_url": "ftp://files.test", "policy.mode": Denylist}},
		{"a fragment on the base", AuthNone, map[string]any{"base_url": "https://api.test/v1#top", "policy.mode": Denylist}},
		{"bearer with no token", AuthBearer, map[string]any{"base_url": "https://api.test", "policy.mode": Denylist}},
		{"a key with no name", AuthAPIKey, map[string]any{"base_url": "https://api.test", "auth.key": "k", "auth.placement": InHeader, "policy.mode": Denylist}},
		{"a key sent nowhere sensible", AuthAPIKey, map[string]any{"base_url": "https://api.test", "auth.key": "k", "auth.name": "K", "auth.placement": "cookie", "policy.mode": Denylist}},
		{"an additional header with no colon", AuthNone, map[string]any{"base_url": "https://api.test", "extra_headers": "X-Account 123", "policy.mode": Denylist}},
		{"an additional header with no value", AuthNone, map[string]any{"base_url": "https://api.test", "extra_headers": "X-Account:", "policy.mode": Denylist}},
		{"an additional header with no name", AuthNone, map[string]any{"base_url": "https://api.test", "extra_headers": ": 123", "policy.mode": Denylist}},
		{"the same additional header twice", AuthNone, map[string]any{"base_url": "https://api.test", "extra_headers": "X-Account: 1\nx-account: 2", "policy.mode": Denylist}},
		{"an additional query parameter with no colon", AuthNone, map[string]any{"base_url": "https://api.test", "extra_query": "version 2", "policy.mode": Denylist}},
		{"an allowlist of nothing", AuthNone, map[string]any{"base_url": "https://api.test", "policy.mode": Allowlist, "policy.verbs": ""}},
		{"a verb that is not one", AuthNone, map[string]any{"base_url": "https://api.test", "policy.mode": Denylist, "policy.verbs": "TRACE"}},
	}
	for _, c := range cases {
		_, err := New(nil).Build(template.Input{Alias: "x", Variant: c.variant, Settings: c.settings})
		if err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	// The control: the same shape, correct, is accepted. Without this every
	// refusal above could be one shared mistake in the fixture.
	if _, err := New(nil).Build(template.Input{
		Alias: "x", Variant: AuthAPIKey,
		Settings: map[string]any{
			"base_url": "https://api.test/v2", "auth.key": "k", "auth.name": "X-Api-Key",
			"auth.placement": InHeader, "policy.mode": Denylist, "policy.verbs": "DELETE",
		},
	}); err != nil {
		t.Fatalf("a correct configuration was refused: %v", err)
	}
}

// Test reads the API's ANSWER rather than demanding a 200.
//
// A 401 means the credential is wrong, which is the thing an administrator
// needs to hear before saving. A 404 at the root means the address is reachable
// and authenticated and simply has nothing there, which is true of most APIs
// and is not a fault: failing on it would make the button useless.
func TestTheTestButtonTellsABadCredentialFromAnEmptyRoot(t *testing.T) {
	tpl := New(nil)
	answer := func(status int) json.RawMessage {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		cfg, err := tpl.Config(AuthBearer, map[string]any{
			"base_url": srv.URL, "auth.token": "t", "policy.mode": Denylist,
		})
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		return cfg
	}

	if err := tpl.Test(context.Background(), answer(404)); err != nil {
		t.Errorf("a 404 at the root was reported as a fault: %v", err)
	}
	if err := tpl.Test(context.Background(), answer(200)); err != nil {
		t.Errorf("a 200 was reported as a fault: %v", err)
	}
	for _, status := range []int{401, 403} {
		err := tpl.Test(context.Background(), answer(status))
		if err == nil {
			t.Errorf("a %d was accepted, so a wrong credential would be saved as working", status)
			continue
		}
		if !strings.Contains(err.Error(), "credentials") {
			t.Errorf("the %d message does not say what is wrong: %v", status, err)
		}
	}
	// And somewhere that is not there at all fails rather than passing.
	dead, err := tpl.Config(AuthNone, map[string]any{
		"base_url": "http://127.0.0.1:1", "policy.mode": Denylist,
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := tpl.Test(context.Background(), dead); err == nil {
		t.Error("an address nothing answers on was accepted")
	}
}

// The note the form cannot remove says where calls go and what the rule is, so
// the model always knows even if an administrator rewrote the description.
func TestTheDocumentationSaysWhereCallsGoAndWhatIsAllowed(t *testing.T) {
	inst := build(t, AuthNone, map[string]any{
		"base_url": "https://api.test/v1", "policy.mode": Allowlist, "policy.verbs": "GET",
	})
	note := New(nil).Documentation(inst.Config).Note
	for _, want := range []string{"https://api.test/v1", "only use GET"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not carry %q: %q", want, note)
		}
	}
}
