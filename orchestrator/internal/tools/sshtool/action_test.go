package sshtool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/tools/template"
)

// Testing a connection from the console, including a server that will not accept
// one until somebody reads out a code.
//
// The console knows none of this: it runs an action, and either gets an outcome
// or a set of fields to collect. So what these prove is that the tool says
// enough for that to work, and that a test never leaves anything behind.

func actionRequest(t *testing.T, token string, values map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"token": token, "values": values})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return raw
}

func configJSON(t *testing.T, cfg Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	return raw
}

func TestActionTestReportsAWorkingConnection(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	tmpl := New(nil)
	defer tmpl.Close()

	result, err := tmpl.Action(context.Background(),
		configJSON(t, serverConfig(srv, Auth{Password: "hunter2"})), template.ActionTest, nil)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if !result.OK {
		t.Fatalf("a working connection was reported as a failure: %+v", result)
	}
	if result.Prompt != nil {
		t.Fatal("a working connection asked for something")
	}
	if result.Message == "" {
		t.Fatal("a successful test said nothing")
	}
}

func TestActionTestReportsAFailureInPlainWords(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	tmpl := New(nil)
	defer tmpl.Close()

	result, err := tmpl.Action(context.Background(),
		configJSON(t, serverConfig(srv, Auth{Password: "wrong"})), template.ActionTest, nil)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if result.OK {
		t.Fatal("a wrong password passed the test")
	}
	// What an administrator reads is a sentence, not the plumbing underneath.
	if strings.Contains(result.Message, "handshake") || strings.Contains(result.Message, "ssh:") {
		t.Fatalf("the plumbing reached the form: %q", result.Message)
	}
	if !strings.HasSuffix(result.Message, ".") {
		t.Fatalf("the message is not a sentence: %q", result.Message)
	}
}

// The whole point of the seam: a server that wants a code turns into a field to
// fill in, and the console needs to understand nothing beyond that.
func TestActionTestAsksForAVerificationCode(t *testing.T) {
	srv := startServer(t, serverOptions{
		Password: "hunter2", Code: "424242",
		CodeQuestion:    "One-time code: ",
		CodeInstruction: "A code has been sent to your phone.",
	})
	tmpl := New(nil)
	defer tmpl.Close()

	result, err := tmpl.Action(context.Background(),
		configJSON(t, serverConfig(srv, Auth{PrivateKey: srv.ClientKey})), template.ActionTest, nil)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if result.OK {
		t.Fatal("a test reported success before the server had accepted the connection")
	}
	if result.Prompt == nil {
		t.Fatalf("the server's question did not reach the form: %+v", result)
	}
	prompt := result.Prompt
	if prompt.Action != actionVerify || prompt.Token == "" {
		t.Fatalf("the form cannot send an answer back: %+v", prompt)
	}
	if len(prompt.Fields) != 1 {
		t.Fatalf("fields = %d, want one for the code", len(prompt.Fields))
	}
	field := prompt.Fields[0]
	// A code is not something to show on screen, and it has to be filled in.
	if field.Type != template.FieldPassword || !field.Required {
		t.Fatalf("the code field is wrong: %+v", field)
	}
	// The server's own wording, so a person recognises what is being asked.
	if !strings.Contains(field.Label, "One-time code") {
		t.Fatalf("label = %q, want the server's own question", field.Label)
	}
	if !strings.Contains(prompt.Hint, "sent to your phone") {
		t.Fatalf("hint = %q, want what the server said alongside", prompt.Hint)
	}
}

// The answer finishes the sign-in, and the test reports that it worked.
func TestActionVerifyFinishesTheTest(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	tmpl := New(nil)
	defer tmpl.Close()
	config := configJSON(t, serverConfig(srv, Auth{PrivateKey: srv.ClientKey}))

	asked, err := tmpl.Action(context.Background(), config, template.ActionTest, nil)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if asked.Prompt == nil {
		t.Fatal("no code was asked for")
	}

	done, err := tmpl.Action(context.Background(), config, asked.Prompt.Action,
		actionRequest(t, asked.Prompt.Token, map[string]string{"code": "424242"}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !done.OK {
		t.Fatalf("the code was given but the test failed: %+v", done)
	}
	if done.Prompt != nil {
		t.Fatal("the test asked for something after it had finished")
	}
}

func TestActionVerifyRefusesAWrongCode(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	tmpl := New(nil)
	defer tmpl.Close()
	config := configJSON(t, serverConfig(srv, Auth{PrivateKey: srv.ClientKey}))

	asked, err := tmpl.Action(context.Background(), config, template.ActionTest, nil)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	done, err := tmpl.Action(context.Background(), config, actionVerify,
		actionRequest(t, asked.Prompt.Token, map[string]string{"code": "000000"}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if done.OK {
		t.Fatal("a wrong code passed the test")
	}
}

// An answer is used once. A token that leaks, or a form submitted twice, cannot
// be replayed against whatever question happens to be open later.
func TestActionVerifyTokenIsUsedOnce(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	tmpl := New(nil)
	defer tmpl.Close()
	config := configJSON(t, serverConfig(srv, Auth{PrivateKey: srv.ClientKey}))

	asked, err := tmpl.Action(context.Background(), config, template.ActionTest, nil)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	token := asked.Prompt.Token

	if _, err := tmpl.Action(context.Background(), config, actionVerify,
		actionRequest(t, token, map[string]string{"code": "424242"})); err != nil {
		t.Fatalf("verify: %v", err)
	}

	again, err := tmpl.Action(context.Background(), config, actionVerify,
		actionRequest(t, token, map[string]string{"code": "424242"}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if again.OK {
		t.Fatal("the same answer was accepted twice")
	}
	if !strings.Contains(again.Message, "again") {
		t.Fatalf("the administrator is not told what to do: %q", again.Message)
	}
}

func TestActionVerifyNeedsACode(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2", Code: "424242"})
	tmpl := New(nil)
	defer tmpl.Close()
	config := configJSON(t, serverConfig(srv, Auth{PrivateKey: srv.ClientKey}))

	asked, err := tmpl.Action(context.Background(), config, template.ActionTest, nil)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	result, err := tmpl.Action(context.Background(), config, actionVerify,
		actionRequest(t, asked.Prompt.Token, map[string]string{"code": "   "}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.OK {
		t.Fatal("an empty code was accepted")
	}
}

func TestActionRefusesAnUnknownAction(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	tmpl := New(nil)
	defer tmpl.Close()

	if _, err := tmpl.Action(context.Background(),
		configJSON(t, serverConfig(srv, Auth{Password: "hunter2"})), "reboot", nil); err == nil {
		t.Fatal("an action this tool does not have was run")
	}
}

// Testing settings that are not yet a working configuration is an answer on the
// form, not an error: the administrator is still filling it in.
func TestActionTestReportsBadSettingsOnTheForm(t *testing.T) {
	tmpl := New(nil)
	defer tmpl.Close()

	result, err := tmpl.Action(context.Background(),
		json.RawMessage(`{"driver":"ssh","host":"","username":""}`), template.ActionTest, nil)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if result.OK {
		t.Fatal("settings that cannot connect passed the test")
	}
	if result.Message == "" {
		t.Fatal("the administrator was told nothing about what is missing")
	}
}

// A test proves the settings and gets out of the way: it must not leave a
// connection pooled for a tool that may never be saved.
func TestActionTestLeavesNoConnectionBehind(t *testing.T) {
	srv := startServer(t, serverOptions{Password: "hunter2"})
	tmpl := New(nil)
	defer tmpl.Close()

	cfg := serverConfig(srv, Auth{Password: "hunter2"})
	if _, err := tmpl.Action(context.Background(), configJSON(t, cfg), template.ActionTest, nil); err != nil {
		t.Fatalf("action: %v", err)
	}

	tmpl.pool.mu.Lock()
	servers := len(tmpl.pool.servers)
	tmpl.pool.mu.Unlock()
	if servers != 0 {
		t.Fatalf("a test left %d server(s) in the pool", servers)
	}
}
