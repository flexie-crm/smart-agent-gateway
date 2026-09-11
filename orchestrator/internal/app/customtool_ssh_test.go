package app_test

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// The SSH tool through the generic layer it is stored by.
//
// The template's own tests prove what it does on a live server. These prove the
// seams around it, which no test inside the template can reach: the credential
// is sealed into the row and comes back openable, an edit finds the tool's
// variant again and carries a credential nobody re-entered, and the bound
// handler that comes out of all that is the one the assistant is handed.

// closedPort returns a port on the loopback that nothing is listening on, so a
// connection to it is refused at once rather than hanging.
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// sshSettings point at a server that really answers, because a tool is not
// stored until it has been proved to connect.
func sshSettings(srv *sshTestServer) map[string]any {
	return map[string]any{
		"host":             srv.Host,
		"port":             float64(srv.Port),
		"username":         "deploy",
		"host_key":         srv.HostKey,
		"auth.password":    srv.Password,
		"auth.private_key": "",
		"auth.passphrase":  "",
		"policy.mode":      "allowlist",
		"policy.allowed":   "uptime\nsystemctl restart application",
		"policy.sudo":      "deny",
	}
}

// Create, store, load, run: the whole path, with the key sealed in the row and
// opened again on the way to the handler.
func TestCreateAndLoadACustomSSHTool(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops@acme.test")

	srv := startSSHServer(t)
	settings := sshSettings(srv)

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
		Alias: "production", Variant: "ssh", Settings: settings,
	})
	if err != nil {
		t.Fatalf("create custom tool: %v", err)
	}
	if created.Name != "ssh_production" || created.Kind != string(tool.KindCustom) || created.Template != "ssh" {
		t.Fatalf("the custom tool row is wrong: %+v", created)
	}
	if created.Risk != string(tool.RiskDestructiveAction) {
		t.Fatalf("risk = %q, want destructive_action", created.Risk)
	}

	// The key is sealed in the stored row, not sitting there in the clear.
	row, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("reload tool: %v", err)
	}
	if strings.Contains(string(row.Config), srv.Password) {
		t.Fatal("the password is stored in the clear")
	}
	var stored struct {
		Driver string `json:"driver"`
		Auth   struct {
			Password string `json:"password"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(row.Config, &stored); err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !strings.HasPrefix(stored.Auth.Password, "enc:") {
		t.Fatalf("the password is not sealed: %q", stored.Auth.Password)
	}
	// The variant is recorded where the generic layer reads every custom tool's
	// variant from. An edit cannot find the template's form without it.
	if stored.Driver != "ssh" {
		t.Fatalf("driver = %q, want ssh", stored.Driver)
	}

	// It resolves into a loadout with its handler, its guide, the system-owned
	// note naming the machine, and topics namespaced to this tool.
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{"ssh_production"}, nil, nil, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	schema, ok := loadout.Schema("ssh_production")
	if !ok {
		t.Fatal("the custom tool did not reach the loadout")
	}
	if len(schema.Guide) == 0 {
		t.Fatal("the custom tool did not get its template's guide")
	}
	if !strings.Contains(schema.Description, srv.Host) || !strings.Contains(schema.Description, "deploy") {
		t.Fatalf("the loaded description does not name the machine: %q", schema.Description)
	}
	var commands bool
	for _, topic := range schema.Topics {
		if topic.ID == "ssh_production/commands" {
			commands = true
		}
	}
	if !commands {
		t.Fatalf("the tool's topics are not namespaced to it: %+v", schema.Topics)
	}

	handler, ok := loadout.Handlers["ssh_production"]
	if !ok {
		t.Fatal("the custom tool has no handler")
	}

	// The policy came out of the row and is enforced by the bound handler: a
	// command that is not permitted is refused before anything is dialled.
	args, _ := json.Marshal(map[string]any{"operation": "execute", "command": "rm -rf /"})
	res, err := handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Err != tool.ErrorBlocked {
		t.Fatalf("a command outside the policy gave %q, want blocked: %s", res.Err, res.Content)
	}

	// A permitted command reaches the server and runs, which is what proves the
	// sealed credential was opened on the way: a password that came back damaged
	// would be refused at the sign-in.
	args, _ = json.Marshal(map[string]any{"operation": "execute", "command": "uptime"})
	res, err = handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Failed() {
		t.Fatalf("the stored tool could not run a permitted command: %s", res.Content)
	}
	// Whatever it says, it never says the credential.
	if strings.Contains(string(res.Content), srv.Password) {
		t.Fatal("the credential reached the assistant")
	}
}

// Editing keeps the credential nobody re-entered, and finds the template's form
// again from the stored variant.
func TestUpdateCustomSSHToolKeepsItsCredential(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := e.user("ops2@acme.test")

	srv := startSSHServer(t)
	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
		Alias: "staging", Variant: "ssh", Settings: sshSettings(srv),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The edit view finds the variant and hands back everything but the secrets.
	stored, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	view, err := e.app.CustomToolForEdit(stored)
	if err != nil {
		t.Fatalf("edit view: %v", err)
	}
	if view.Variant != "ssh" {
		t.Fatalf("variant = %q, want ssh: an edit cannot build the form without it", view.Variant)
	}
	if view.Settings["auth.password"] != "" {
		t.Fatal("the credential was handed back to the form")
	}
	if view.Settings["host"] != srv.Host || view.Settings["policy.allowed"] == "" {
		t.Fatalf("the non-secret settings did not come back: %+v", view.Settings)
	}

	// Edit a non-secret and leave the key blank, the way an administrator would.
	edited := view.Settings
	edited["policy.allowed"] = "uptime"
	if _, err := e.app.UpdateCustomTool(ctx, e.ws.ID, created.ID, template.Input{
		Settings:    edited,
		Description: "The staging box.",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reloaded, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var after struct {
		Auth struct {
			Password string `json:"password"`
		} `json:"auth"`
		Policy struct {
			Allowed string `json:"allowed"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(reloaded.Config, &after); err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if after.Auth.Password == "" || !strings.HasPrefix(after.Auth.Password, "enc:") {
		t.Fatalf("the credential was lost or exposed on edit: %q", after.Auth.Password)
	}
	if after.Policy.Allowed != "uptime" {
		t.Fatalf("the edited policy did not persist: %q", after.Policy.Allowed)
	}

	// The edited tool binds, and the carried key is still usable: the command it
	// no longer permits is refused, and the one it does reaches the connection.
	loadout, err := e.app.Loadout(ctx, e.ws.ID, user.ID, "", []string{"ssh_staging"}, nil, nil, tool.OwnerOfAgent())
	if err != nil {
		t.Fatalf("loadout: %v", err)
	}
	handler, ok := loadout.Handlers["ssh_staging"]
	if !ok {
		t.Fatal("the edited tool has no handler")
	}

	args, _ := json.Marshal(map[string]any{"operation": "execute", "command": "systemctl restart application"})
	res, err := handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Err != tool.ErrorBlocked {
		t.Fatalf("a command the edit removed gave %q, want blocked: %s", res.Err, res.Content)
	}

	args, _ = json.Marshal(map[string]any{"operation": "execute", "command": "uptime"})
	res, err = handler(ctx, tool.Call{WorkspaceID: e.ws.ID, UserID: user.ID, Args: args})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Failed() {
		t.Fatalf("the carried credential did not open: %s", res.Content)
	}
}

// A tool whose settings do not describe a working server is refused when it is
// created, not when an assistant first reaches for it.
func TestCreateCustomSSHToolRefusesBrokenSettings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"an allowlist with nothing on it", func(s map[string]any) { s["policy.allowed"] = "" }, "list the commands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := sshSettings(startSSHServer(t))
			tc.change(settings)
			_, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
				Alias: "broken", Variant: "ssh", Settings: settings,
			})
			if err == nil {
				t.Fatal("a broken configuration was stored")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A tool with no credential is refused at create, along with everything else
// that cannot connect.
//
// This used to be the one broken configuration that got stored: an edit reaches
// the same validation with its secrets blanked, so the settings check cannot
// insist on a credential. Proving the connection before storing closes it, since
// a configuration with nothing to sign in with cannot connect either.
func TestACustomSSHToolWithNoCredentialIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	settings := sshSettings(startSSHServer(t))
	settings["auth.private_key"] = ""
	settings["auth.password"] = ""

	_, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
		Alias: "nocredential", Variant: "ssh", Settings: settings,
	})
	if err == nil {
		t.Fatal("a tool with nothing to sign in with was stored")
	}
	if !strings.Contains(err.Error(), "could not be connected to") {
		t.Fatalf("got %q, want it to say the server could not be connected to", err)
	}
}

// A tool is not stored until it has been proved to connect. Settings that
// describe a server nobody can reach would otherwise sit in the catalog looking
// usable and fail the first time an assistant was asked for something.
func TestCreateCustomSSHToolRefusesAServerItCannotReach(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	srv := startSSHServer(t)

	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"nothing listening", func(s map[string]any) { s["port"] = float64(closedPort(t)) }},
		{"wrong password", func(s map[string]any) { s["auth.password"] = "not the password" }},
		{"a different machine's host key", func(s map[string]any) { s["host_key"] = startSSHServer(t).HostKey }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := sshSettings(srv)
			tc.change(settings)

			_, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
				Alias: "unreachable", Variant: "ssh", Settings: settings,
			})
			if err == nil {
				t.Fatal("a tool was created for a server it cannot connect to")
			}
			if !strings.Contains(err.Error(), "could not be connected to") {
				t.Fatalf("got %q, want it to say the server could not be connected to", err)
			}
		})
	}
}

// A parameter with nothing stored against it comes back in the edit form with
// the template's own wording, not as an empty box.
//
// The template is where a tool's starting values come from; after that the row
// is what the tool is. So a parameter the template has gained since this tool
// was made has nothing stored, and the form seeds it the same way creating a new
// tool would. Saving then makes it the tool's own.
func TestEditPrefillFillsParametersAddedSinceTheToolWasMade(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	created, err := e.app.CreateCustomTool(ctx, e.ws.ID, "ssh", template.Input{
		Alias: "prefill", Variant: "ssh", Settings: sshSettings(startSSHServer(t)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A tool stored by an older build: its schema knows only one parameter.
	created.InputSchema = json.RawMessage(
		`{"type":"object","properties":{"command":{"type":"string","description":"Commands for the release box."}}}`)
	if err := e.app.Store.Tools().UpdateCustom(ctx, created); err != nil {
		t.Fatalf("store an older schema: %v", err)
	}

	row, err := e.app.Store.Tools().GetByID(ctx, e.ws.ID, created.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	view, err := e.app.CustomToolForEdit(row)
	if err != nil {
		t.Fatalf("edit view: %v", err)
	}

	tmpl, _ := e.app.Templates.Get("ssh")
	for _, p := range tmpl.Params() {
		if strings.TrimSpace(view.ParamDescriptions[p.Key]) == "" {
			t.Fatalf("%q came back with nothing to show in the form", p.Key)
		}
	}
	// What the administrator wrote is still theirs, not overwritten by the default.
	if view.ParamDescriptions["command"] != "Commands for the release box." {
		t.Fatalf("the administrator's wording was replaced: %q", view.ParamDescriptions["command"])
	}
}
