package sshtool

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

func settings() map[string]any {
	return map[string]any{
		"host":                   "server.internal",
		"port":                   2222,
		"username":               "deploy",
		"host_key":               "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample",
		"auth.password":          "hunter2",
		"policy.mode":            string(cmdpolicy.PolicyAllowlist),
		"policy.allowed":         "systemctl restart application\nuptime",
		"limits.max_channels":    6,
		"limits.idle_minutes":    15,
		"limits.connect_seconds": 12,
	}
}

func TestBuildProducesTheTool(t *testing.T) {
	inst, err := sshTemplate{}.Build(template.Input{
		Alias: "production", Variant: VariantSSH, Settings: settings(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inst.Schema.Name != "ssh_production" {
		t.Fatalf("name = %q, want ssh_production", inst.Schema.Name)
	}
	if inst.Schema.Kind != tool.KindCustom {
		t.Fatalf("kind = %q, want custom", inst.Schema.Kind)
	}
	// A tool that runs commands on a machine is never anything but the top of
	// the risk scale: the confirmation card's default copy is keyed to it.
	if inst.Schema.Risk != tool.RiskDestructiveAction {
		t.Fatalf("risk = %q, want destructive_action", inst.Schema.Risk)
	}
	if strings.TrimSpace(inst.Guide) == "" {
		t.Fatal("a tool was built with no guide")
	}
}

// The stored config is what the handler and the connection are built from, so
// the form's dotted keys have to arrive as the nested shape, with the variant
// recorded where the app layer reads every custom tool's variant from.
func TestBuildNestsSettingsAndRecordsTheVariant(t *testing.T) {
	inst, err := sshTemplate{}.Build(template.Input{
		Alias: "production", Variant: VariantSSH, Settings: settings(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var stored struct {
		Driver string `json:"driver"`
		Host   string `json:"host"`
		Auth   struct {
			Password string `json:"password"`
		} `json:"auth"`
		Limits struct {
			MaxChannels int `json:"max_channels"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(inst.Config, &stored); err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if stored.Driver != VariantSSH {
		t.Fatalf("driver = %q, want %q", stored.Driver, VariantSSH)
	}
	if stored.Host != "server.internal" || stored.Auth.Password != "hunter2" {
		t.Fatalf("connection settings were not stored: %+v", stored)
	}
	if stored.Limits.MaxChannels != 6 {
		t.Fatalf("max channels = %d, want 6", stored.Limits.MaxChannels)
	}
}

func TestBuildRefusesBadInput(t *testing.T) {
	cases := []struct {
		name  string
		input template.Input
		want  string
	}{
		{"bad alias", template.Input{Alias: "Production Server", Variant: VariantSSH, Settings: settings()}, "must start with a letter"},
		{"unknown variant", template.Input{Alias: "production", Variant: "telnet", Settings: settings()}, "unknown variant"},
		{"incomplete settings", template.Input{Alias: "production", Variant: VariantSSH, Settings: map[string]any{"host": "h"}}, "username is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (sshTemplate{}).Build(tc.input); err == nil {
				t.Fatal("expected the input to be refused")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The model is told the operations it may ask for, rather than being left to
// guess and have the call refused.
func TestInputSchemaConstrainsTheOperation(t *testing.T) {
	inst, err := sshTemplate{}.Build(template.Input{
		Alias: "production", Variant: VariantSSH, Settings: settings(),
		ParamDescriptions: map[string]string{"command": "Commands for the release box."},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(inst.Schema.InputSchema, &schema); err != nil {
		t.Fatalf("input schema: %v", err)
	}
	// Three inputs and no mode to pick between: what a call means follows from
	// what it set, so there is nothing to choose wrongly.
	for _, key := range []string{"command", "input", "wait", "stop"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Fatalf("the schema has no %s", key)
		}
	}
	if len(schema.Properties) != 4 {
		t.Fatalf("the schema takes %d inputs, want the four: %v", len(schema.Properties), schema.Properties)
	}
	if _, gone := schema.Properties["operation"]; gone {
		t.Fatal("the schema still asks the assistant to pick an operation")
	}
	// Nothing is required: a call with none of them reads what is running.
	if len(schema.Required) != 0 {
		t.Fatalf("required = %v, want none", schema.Required)
	}
	if schema.Properties["stop"].Type != "boolean" {
		t.Fatalf("stop type = %q, want boolean", schema.Properties["stop"].Type)
	}
	// An administrator's wording is used; the identity is not theirs to change.
	if schema.Properties["command"].Description != "Commands for the release box." {
		t.Fatalf("the administrator's description was not used: %q", schema.Properties["command"].Description)
	}
	if schema.Properties["command"].Type != "string" {
		t.Fatalf("command type = %q, want string", schema.Properties["command"].Type)
	}
}

// Every credential leaf must be named, whichever method is configured, or
// switching method later would leave one of them in the clear.
func TestSecretPathsCoverEveryCredential(t *testing.T) {
	paths := sshTemplate{}.SecretPaths(VariantSSH)
	for _, want := range []string{"auth.private_key", "auth.passphrase", "auth.password"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q is not sealed; sealed paths are %v", want, paths)
		}
	}
}

// A secret must never be declared as anything but a secret field, or the console
// would show it and the app would store it in the clear.
func TestFieldsMarkCredentialsSecret(t *testing.T) {
	sections, err := (&sshTemplate{}).Fields(VariantSSH)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	secret := map[string]bool{}
	for _, section := range sections {
		for _, field := range section.Fields {
			secret[field.Key] = field.Secret
		}
	}
	for _, key := range (sshTemplate{}).SecretPaths(VariantSSH) {
		if !secret[key] {
			t.Fatalf("%q is sealed at rest but is not a secret field in the form", key)
		}
	}
	if secret["host_key"] {
		t.Fatal("the host key is public and must not be write-only in the form")
	}
	if _, err := (&sshTemplate{}).Fields("telnet"); err == nil {
		t.Fatal("an unknown variant should have no form")
	}
}

// The note and topics are what the model knows however the form is edited, so
// they must name the machine and never carry a credential.
func TestDocumentationNamesTheServerAndKeepsSecrets(t *testing.T) {
	config, err := sshTemplate{}.Config(VariantSSH, settings())
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	doc := sshTemplate{}.Documentation(config)
	if !strings.Contains(doc.Note, "server.internal") || !strings.Contains(doc.Note, "deploy") {
		t.Fatalf("the note does not say which server this is: %q", doc.Note)
	}
	if len(doc.Topics) != 7 {
		t.Fatalf("topics = %d, want 7", len(doc.Topics))
	}
	all := doc.Note
	commands := ""
	for _, topic := range doc.Topics {
		all += topic.Body
		if len(topic.Edges) == 0 {
			t.Fatalf("topic %q has no edges, so the model cannot navigate from it", topic.ID)
		}
		if topic.ID == "commands" {
			commands = topic.Body
		}
	}
	// The model is told what may run, so it reaches for a permitted command
	// instead of proposing one that will be refused.
	if !strings.Contains(commands, "systemctl restart application") || !strings.Contains(commands, "uptime") {
		t.Fatalf("the permitted commands are not documented: %q", commands)
	}
	if strings.Contains(all, "hunter2") {
		t.Fatal("a credential reached the model-facing documentation")
	}
}

func TestValidateCall(t *testing.T) {
	cases := []struct {
		name string
		args CallArgs
		want string
	}{
		{"stop with a command", CallArgs{Stop: true, Command: "uptime"}, "on its own"},
		{"stop with input", CallArgs{Stop: true, Input: "yes"}, "on its own"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCall(tc.args)
			if err == nil {
				t.Fatal("expected the call to be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
	// Everything else is answered by the handler, which knows what is actually
	// running and can say so precisely. An empty call is how the running command
	// is read, so it is valid.
	for _, ok := range []CallArgs{
		{Command: "uptime"}, {Input: "yes"}, {Stop: true}, {}, {Command: "uptime", Input: "424242"},
	} {
		if err := ValidateCall(ok); err != nil {
			t.Fatalf("a valid call was refused: %+v: %v", ok, err)
		}
	}
}

// The guide is what makes the tool usable rather than merely available, so the
// things an assistant most often gets wrong are asserted rather than left to
// drift: that nothing carries between commands, that a log is followed by
// reading an open session, and that output comes back capped.
func TestTheGuideTeachesHowToUseTheServerWell(t *testing.T) {
	config, err := sshTemplate{}.Config(VariantSSH, settings())
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	topics := map[string]string{}
	for _, topic := range (sshTemplate{}).Documentation(config).Topics {
		topics[topic.ID] = topic.Body
	}

	must := []struct {
		topic, teaches, phrase string
	}{
		{"working", "that a command does not keep its state", "carries from one command to the next"},
		{"working", "to narrow the output at the server", "grep"},
		{"working", "that a truncated result cannot be concluded from", "output_dropped"},
		{"working", "that a non-zero exit is an answer", "exit_code"},
		{"interactive", "that a running command is followed by reading", "tail -f"},
		{"interactive", "that reading again is how a log is followed", "since you last looked"},
		{"interactive", "that a running shell keeps its state", "KEEPS ITS STATE"},
		{"interactive", "to stop what it is done with", "stop"},
		{"limits", "how long a command may run", "seconds"},
	}
	for _, want := range must {
		body, ok := topics[want.topic]
		if !ok {
			t.Fatalf("there is no %q topic", want.topic)
		}
		if !strings.Contains(body, want.phrase) {
			t.Fatalf("the %q topic no longer teaches %s", want.topic, want.teaches)
		}
	}

	// And each of the three inputs says what it is for, because there is no mode
	// to look up and nothing else that could explain them.
	described := map[string]string{
		"command": "the command to run",
		"input":   "what to type",
		"wait":    "seconds to wait",
		"stop":    "ends the command",
	}
	for _, p := range (sshTemplate{}).Params() {
		want, ok := described[p.Key]
		if !ok {
			t.Fatalf("%q is offered but this test does not know what it is for", p.Key)
		}
		if !strings.Contains(strings.ToLower(p.Description), want) {
			t.Fatalf("the %q parameter does not say what it is for: %q", p.Key, p.Description)
		}
	}
}

// A new tool starts as a denylist with the dangerous commands already in it.
// The default is checked as behaviour, not as text: a list that looks right in
// a form and refuses nothing is the failure worth catching.
func TestANewToolIsSeededWithTheDangerousCommandsRefused(t *testing.T) {
	sections, err := (&sshTemplate{}).Fields(VariantSSH)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	var denied template.Field
	for _, section := range sections {
		for _, field := range section.Fields {
			if field.Key == "policy.denied" {
				denied = field
			}
		}
	}
	if denied.Key == "" {
		t.Fatal("the form has no field for the refused commands")
	}
	// And the rule it seeds is the one the list belongs to, or a new tool would
	// start as an allowlist with a refused list nothing consults.
	for _, section := range sections {
		for _, field := range section.Fields {
			if field.Key == "policy.mode" && field.Default != string(cmdpolicy.PolicyDenylist) {
				t.Fatalf("a new tool starts as %q, want a denylist so the seeded list is the one in force", field.Default)
			}
		}
	}
	// The four kinds of damage the default is there for: unrecoverable, the
	// machine gone, locked out, and the account it signs in as removed.
	for _, command := range []string{"rm", "dd", "mkfs", "shutdown", "iptables", "userdel"} {
		if !slices.Contains(strings.Fields(denied.Default), command) {
			t.Fatalf("a new tool does not refuse %q by default: %q", command, denied.Default)
		}
	}
	// And what it seeds actually refuses those commands, rather than being text
	// that looks right in a form.
	seeded := cmdpolicy.Policy{Mode: cmdpolicy.PolicyDenylist, Denied: denied.Default, Sudo: cmdpolicy.SudoDeny}
	for _, command := range []string{"rm -rf /var/lib/app", "sudo shutdown now", "find . -exec rm {} ;"} {
		if reason := seeded.Check(command); reason == "" {
			t.Fatalf("the seeded list permitted %q", command)
		}
	}
	if reason := seeded.Check("systemctl status application"); reason != "" {
		t.Fatalf("the seeded list refuses ordinary work: %s", reason)
	}
}

// The policy is the last thing on the form, and it is called the same thing
// here as on every other tool that has one, so an administrator looking for
// what an agent may do finds it in the same place each time.
func TestTheFormEndsWithThePolicy(t *testing.T) {
	sections, err := (&sshTemplate{}).Fields(VariantSSH)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	var titles []string
	for _, section := range sections {
		titles = append(titles, section.Title)
	}
	if got := strings.Join(titles, " | "); got != "Connection | Limits | Policy" {
		t.Fatalf("sections = %q", got)
	}
	for _, field := range sections[len(sections)-1].Fields {
		if !strings.HasPrefix(field.Key, "policy.") {
			t.Fatalf("a field that is not part of the policy landed in it: %q", field.Key)
		}
	}
}
