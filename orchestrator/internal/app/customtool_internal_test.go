package app

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/crypto"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
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

	schema, _, ok := a.bindCustom(row, tool.OwnerNone)
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
