package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/crypto"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/apitool"
	"flexie.io/sag/internal/tools/query"
	"flexie.io/sag/internal/tools/template"
)

func testApp(t *testing.T) *App {
	t.Helper()
	kr, err := crypto.ParseKeyring("1:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff", "1")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return &App{Keyring: kr}
}

// A secret in the config is sealed in place: it is gone from the JSON in the
// clear, replaced by a marked ciphertext, and it comes back exactly on open.
// Non-secret fields are untouched, and open needs no knowledge of which fields
// were secret, it walks the config and opens what is marked.
func TestConfigSecretRoundTrip(t *testing.T) {
	a := testApp(t)
	config := json.RawMessage(`{
		"driver": "mysql", "host": "db.internal", "password": "s3cr3t",
		"tls": {"mode": "require"},
		"ssh": {"host": "bastion", "password": "sshpw", "private_key": "PEMDATA"}
	}`)

	sealed, err := a.sealConfigSecrets(config, []string{"password", "ssh.password", "ssh.private_key"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	s := string(sealed)
	// The plaintext secrets are gone; the marker is present.
	for _, secret := range []string{"s3cr3t", "sshpw", "PEMDATA"} {
		if strings.Contains(s, secret) {
			t.Fatalf("a secret is stored in the clear: %q in %s", secret, s)
		}
	}
	// The marker is printable, so it appears verbatim in the JSON.
	if !strings.Contains(s, "enc:") {
		t.Fatalf("sealed values are not marked: %s", s)
	}
	// Non-secret fields are untouched.
	if !strings.Contains(s, "db.internal") || !strings.Contains(s, "bastion") {
		t.Fatalf("a non-secret field was altered: %s", s)
	}

	opened, err := a.openConfigSecrets(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(opened, &m); err != nil {
		t.Fatalf("opened config: %v", err)
	}
	if m["password"] != "s3cr3t" {
		t.Fatalf("password did not round-trip: %v", m["password"])
	}
	ssh := m["ssh"].(map[string]any)
	if ssh["password"] != "sshpw" || ssh["private_key"] != "PEMDATA" {
		t.Fatalf("ssh secrets did not round-trip: %+v", ssh)
	}
	if ssh["host"] != "bastion" {
		t.Fatalf("a non-secret ssh field changed: %+v", ssh)
	}
}

// On an edit, a blank secret is filled with the plaintext of the value sealed in
// the old config (so the connection still works and it re-seals cleanly), while
// a secret typed through is kept, and a non-secret field takes its new value.
func TestCarrySecrets(t *testing.T) {
	a := testApp(t)
	// Seal real values into the old config, the way a stored tool holds them.
	oldCfg, err := a.sealConfigSecrets(
		json.RawMessage(`{"host":"old.host","password":"s3cr3t","ssh":{"password":"sshpw"}}`),
		[]string{"password", "ssh.password"})
	if err != nil {
		t.Fatalf("seal old: %v", err)
	}
	newCfg, _ := json.Marshal(map[string]any{
		"host":     "new.host",
		"password": "",                                   // blank: carry the old
		"ssh":      map[string]any{"password": "typed!"}, // typed: keep the new
	})

	carried, err := a.carrySecrets(newCfg, oldCfg, []string{"password", "ssh.password"})
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(carried, &m)
	if m["password"] != "s3cr3t" {
		t.Fatalf("a blank secret was not carried forward as plaintext: %v", m["password"])
	}
	if ssh := m["ssh"].(map[string]any); ssh["password"] != "typed!" {
		t.Fatalf("a typed secret was overwritten: %v", ssh["password"])
	}
	if m["host"] != "new.host" {
		t.Fatalf("a non-secret field was not updated: %v", m["host"])
	}
}

// A bound custom tool always carries the engine note in its description (so the
// model knows it is MySQL no matter what the form said) and its drill-down
// topics, namespaced by the tool's name.
func TestBindCustomAddsEngineNoteAndTopics(t *testing.T) {
	a := testApp(t)
	a.Log = zerolog.Nop()
	a.Templates = template.NewRegistry()
	a.Templates.Add(query.New(nil))

	inst, err := query.New(nil).Build(template.Input{
		Alias: "orders", Variant: "mysql",
		Settings:    map[string]any{"access": "read", "host": "h", "database": "d", "username": "u"},
		Description: "Demo CRM database.",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := &model.Tool{
		Name: inst.Schema.Name, Template: "query", Kind: string(tool.KindCustom),
		FriendlyName: inst.Schema.FriendlyName, Description: inst.Schema.Description,
		InputSchema: inst.Schema.InputSchema, Risk: string(inst.Schema.Risk), Config: inst.Config,
	}

	schema, _, ok := a.bindCustom(row, tool.OwnerNone, nil)
	if !ok {
		t.Fatal("bind failed")
	}
	if !strings.Contains(schema.Description, "Demo CRM database.") {
		t.Fatalf("the admin description was lost: %q", schema.Description)
	}
	if !strings.Contains(schema.Description, "MySQL") {
		t.Fatalf("the engine note is missing from the description: %q", schema.Description)
	}
	found := false
	for _, tp := range schema.Topics {
		if tp.ID == "query_orders/engine" {
			found = true
		}
	}
	if !found {
		t.Fatalf("topics are not namespaced by the tool name: %+v", schema.Topics)
	}
}

// The stored config reads back into the flat, dotted settings the form uses, the
// driver is read off it, and the parameter descriptions come out of the schema.
func TestFlattenDriverAndParams(t *testing.T) {
	cfg := json.RawMessage(`{"driver":"mysql","host":"h","tls":{"mode":"require"},"ssh":{"host":"b"}}`)
	if driverName(cfg) != "mysql" {
		t.Fatalf("driver: %q", driverName(cfg))
	}
	var nested map[string]any
	_ = json.Unmarshal(cfg, &nested)
	delete(nested, "driver")
	flat := map[string]any{}
	flattenConfig("", nested, flat)
	if flat["host"] != "h" || flat["tls.mode"] != "require" || flat["ssh.host"] != "b" {
		t.Fatalf("the config did not flatten to dotted keys: %+v", flat)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string","description":"the sql"}}}`)
	if paramDescriptions(schema)["sql"] != "the sql" {
		t.Fatalf("the parameter description was not read: %+v", paramDescriptions(schema))
	}
}

// Sealing leaves an empty secret blank (a blank field on an edit means
// unchanged), and seals a value whatever it looks like: a plaintext secret that
// happens to begin with the marker is still sealed and comes back intact, so the
// readable marker can never leave a secret in the clear.
func TestSealSkipsEmptyAndSealsMarkerLikeValues(t *testing.T) {
	a := testApp(t)
	tricky := sealMarker + "not actually sealed"
	sealed, err := a.sealConfigSecrets(
		json.RawMessage(`{"password": `+strconv.Quote(tricky)+`, "blank": ""}`),
		[]string{"password", "blank"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(sealed, &m)
	if m["blank"] != "" {
		t.Fatalf("an empty secret was sealed: %v", m["blank"])
	}
	// The marker-like plaintext is gone from the clear, and it opens back exactly.
	if strings.Contains(string(sealed), "not actually sealed") {
		t.Fatalf("a marker-like secret was stored in the clear: %s", sealed)
	}
	opened, err := a.openConfigSecrets(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = json.Unmarshal(opened, &m)
	if m["password"] != tricky {
		t.Fatalf("the marker-like secret did not round-trip: %v", m["password"])
	}
}

// A tool built before a template improved still gets the improvement.
//
// This is the defect it closes, and it is why a fix can be real and still not
// reach the person who reported it. The parameters handed to a model were
// `row.InputSchema`: the copy written when the tool was created, frozen there.
// The HTTP template learnt that a path may be left out (an endpoint has nothing
// to append to) and that a body need not be JSON, and a tool created the day
// before went on being handed `required: [method, path]` with no `body_type`.
// The form does not offer the shape, because the shape is not an
// administrator's to choose, so there was nothing they could do about it.
//
// Driven through bindCustom, which is the path the agent loop itself takes.
func TestAToolBuiltBeforeATemplateImprovedGetsTheImprovement(t *testing.T) {
	a := testApp(t)
	a.Log = zerolog.Nop()
	a.Templates = template.NewRegistry()
	a.Templates.Add(apitool.New(nil))

	inst, err := apitool.New(nil).Build(template.Input{
		Alias: "dynamic_endpoint", Variant: apitool.AuthNone,
		Settings: map[string]any{
			"base_url":    "https://fx.example.com/listener/a1b2c3d4/e5f6a7b8",
			"policy.mode": apitool.Denylist, "policy.verbs": "DELETE",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// What a tool made before the template moved carries: the parameters as
	// they were then, and the administrator's own wording for one of them.
	stale := json.RawMessage(`{"type":"object","properties":{
		"method":{"type":"string","description":"The HTTP verb."},
		"path":{"type":"string","description":"Use /v2 for everything here."},
		"query":{"type":"object","description":"Parameters."},
		"body":{"type":"object","description":"The body."}
	},"required":["method","path"]}`)

	row := &model.Tool{
		Name: inst.Schema.Name, Template: "api", Kind: string(tool.KindCustom),
		FriendlyName: inst.Schema.FriendlyName, Description: inst.Schema.Description,
		InputSchema: stale, Risk: string(inst.Schema.Risk), Config: inst.Config,
	}

	schema, _, ok := a.bindCustom(row, tool.OwnerNone, nil)
	if !ok {
		t.Fatal("the tool could not be bound")
	}

	var got struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema.InputSchema, &got); err != nil {
		t.Fatalf("the schema handed to the model is not readable: %v", err)
	}

	// The template's shape, not the frozen one.
	if _, offered := got.Properties["body_type"]; !offered {
		t.Fatalf("a parameter the template has gained is not offered: %v", got.Properties)
	}
	if strings.Join(got.Required, ",") != "method" {
		t.Fatalf("required = %v; a path this tool cannot omit cannot reach an endpoint "+
			"that has none", got.Required)
	}
	// And the administrator's own wording survives, because that half IS theirs.
	if got.Properties["path"].Description != "Use /v2 for everything here." {
		t.Fatalf("the administrator's own description was thrown away: %q",
			got.Properties["path"].Description)
	}
	// A parameter they never described reads as the template describes it.
	want := ""
	for _, p := range apitool.New(nil).Params() {
		if p.Key == "body_type" {
			want = p.Description
		}
	}
	if want == "" {
		t.Fatal("the template has no body_type parameter to compare against")
	}
	if got.Properties["body_type"].Description != want {
		t.Fatalf("a parameter the template gained has no description: %q",
			got.Properties["body_type"].Description)
	}
}

// The edit form shows what was configured, and the credential is the only
// thing it withholds.
//
// Reported twice. "I open the tool and I see values are not in the edit modal,
// did you removed them? SO I had values in that tool and now on edit they are
// not!" -- the additional headers and query parameters were sealed, and the
// form blanks a secret, so somebody who had just configured two of them saw
// nothing. They are ordinary settings now ("the additional params are not a
// secret, you should not mask them"), and a value sealed under the old rule
// still has to READ, or the fix reaches new tools only.
func TestTheEditFormShowsWhatWasConfiguredAndHidesOnlyTheCredential(t *testing.T) {
	a := testApp(t)
	a.Log = zerolog.Nop()
	a.Templates = template.NewRegistry()
	a.Templates.Add(apitool.New(nil))

	inst, err := apitool.New(nil).Build(template.Input{
		Alias: "dynamic_endpoint", Variant: apitool.AuthJWTBearer,
		Settings: map[string]any{
			"base_url":              "https://fx.example.com/listener/a1b2c3d4/e5f6a7b8",
			"auth.issuer":           "an-issuer",
			"auth.private_key":      "the-shared-secret",
			"auth.algorithm":        apitool.AlgHS256,
			"auth.name":             apitool.HeaderAuthorization,
			"extra_headers":         "X-Account: acct_123",
			"extra_query":           "version: 2",
			"policy.mode":           apitool.Denylist,
			"policy.verbs":          "DELETE",
			"auth.lifetime_minutes": 30,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Stored the way a tool made under the OLD rule is: the extras sealed too.
	sealed, err := a.sealConfigSecrets(inst.Config,
		[]string{"auth.private_key", "extra_headers", "extra_query"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	row := &model.Tool{
		ID: 1, Name: inst.Schema.Name, Template: "api", Kind: string(tool.KindCustom),
		FriendlyName: "Dynamic Endpoint", Config: sealed, InputSchema: inst.Schema.InputSchema,
	}

	edit, err := a.CustomToolForEdit(row)
	if err != nil {
		t.Fatalf("CustomToolForEdit: %v", err)
	}

	// What was configured, readable.
	for key, want := range map[string]string{
		"base_url":      "https://fx.example.com/listener/a1b2c3d4/e5f6a7b8",
		"auth.issuer":   "an-issuer",
		"extra_headers": "X-Account: acct_123",
		"extra_query":   "version: 2",
	} {
		if got := fmt.Sprint(edit.Settings[key]); got != want {
			t.Fatalf("%s reads %q, want %q", key, got, want)
		}
	}
	// The credential, and only the credential, withheld.
	if got := fmt.Sprint(edit.Settings["auth.private_key"]); got != "" {
		t.Fatalf("the credential reached the form: %q", got)
	}
	// And nothing sealed is shown as ciphertext anywhere.
	whole, err := json.Marshal(edit.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), "enc:") {
		t.Fatalf("a sealed value reached the form as ciphertext:\n%s", whole)
	}
	if strings.Contains(string(whole), "the-shared-secret") {
		t.Fatalf("the credential is readable in the form's answer:\n%s", whole)
	}
}

// The reserved grant subtree never reaches a browser.
//
// The form is built from what the form DECLARES, which is what closes this: a
// tool somebody has signed in to holds an access token and a refresh token in
// its config under `grant`, no field names either of them, and flattening the
// whole config put both into the answer.
func TestTheGrantNeverReachesTheForm(t *testing.T) {
	a := testApp(t)
	a.Log = zerolog.Nop()
	a.Templates = template.NewRegistry()
	a.Templates.Add(apitool.New(nil))

	inst, err := apitool.New(nil).Build(template.Input{
		Alias: "signed_in", Variant: apitool.AuthAuthorizationCode,
		Settings: map[string]any{
			"base_url":           "https://api.example.com/v1",
			"auth.authorize_url": "https://api.example.com/oauth2/authorize",
			"auth.token_url":     "https://api.example.com/oauth2/token",
			"auth.client_id":     "a-client",
			"auth.client_secret": "a-client-secret",
			"policy.mode":        apitool.Denylist,
			"policy.verbs":       "DELETE",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// A connected tool: the grant is written into the config, beside the rest.
	var cfg map[string]any
	if err := json.Unmarshal(inst.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg[apitool.GrantPath] = map[string]any{
		"access_token":  "live-access-token",
		"refresh_token": "live-refresh-token",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.sealConfigSecrets(raw, []string{"auth.client_secret"})
	if err != nil {
		t.Fatal(err)
	}

	edit, err := a.CustomToolForEdit(&model.Tool{
		ID: 2, Name: inst.Schema.Name, Template: "api", Kind: string(tool.KindCustom),
		Config: sealed, InputSchema: inst.Schema.InputSchema,
	})
	if err != nil {
		t.Fatalf("CustomToolForEdit: %v", err)
	}

	whole, err := json.Marshal(edit.Settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"live-access-token", "live-refresh-token", "grant"} {
		if strings.Contains(string(whole), secret) {
			t.Fatalf("%q reached the form:\n%s", secret, whole)
		}
	}
	// The control: the form is not simply empty.
	if fmt.Sprint(edit.Settings["auth.client_id"]) != "a-client" {
		t.Fatalf("the form shows nothing at all: %s", whole)
	}
}
