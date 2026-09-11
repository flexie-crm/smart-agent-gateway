package query

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// The documentation the model always gets names the engine (so it does not guess
// the wrong SQL dialect) and offers drill-down topics for the guide graph.
func TestTemplateDocumentation(t *testing.T) {
	cfg, err := tmpl().Config("mysql", map[string]any{
		"access": "read", "host": "h", "database": "d", "username": "u",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	doc := tmpl().Documentation(cfg)
	if !strings.Contains(doc.Note, "MySQL") {
		t.Fatalf("the note does not name the engine: %q", doc.Note)
	}
	ids := map[string]bool{}
	for _, tp := range doc.Topics {
		ids[tp.ID] = true
	}
	if !ids["engine"] || !ids["limits"] {
		t.Fatalf("the drill-down topics are missing: %+v", doc.Topics)
	}
}

// A read that runs past its deadline is cancelled promptly on the database and
// comes back as a failure carrying the query plan, not rows and not a hang.
func TestQueryHeavyReadTimesOutWithPlan(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping the heavy-query timeout test")
	}
	prev := queryTimeout
	queryTimeout = 1 * time.Second
	defer func() { queryTimeout = prev }()

	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	inst, err := tmpl().Build(template.Input{
		Alias: "slow", Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	handler, err := tmpl().Bind(inst.Config, tool.OwnerNone)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"sql": "SELECT SLEEP(5)"})
	started := time.Now()
	res, err := handler(context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	// It failed rather than returning rows, and it came back promptly (near the
	// 1s deadline), proving the read was cancelled on the server, not waited out.
	if !res.Failed() {
		t.Fatalf("a slow read should fail: %s", res.Content)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("the read was not cancelled promptly: %v", elapsed)
	}
	// The failure carries the plan and says what happened.
	if !strings.Contains(string(res.Content), "query plan") {
		t.Fatalf("the failure does not carry the EXPLAIN plan: %s", res.Content)
	}
}

func tmpl() template.Template { return New(nil) }

// The form for a driver is composed: the access mode, the driver's own fields,
// and the shared SSH fields, so the admin configures everything in one place.
func TestTemplateFields(t *testing.T) {
	sections, err := tmpl().Fields("mysql")
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	// The form is grouped: the direct connection, then the SSH tunnel. The
	// grouping is the template's, so the console renders it without knowing what
	// a section is.
	titles := map[string][]template.Field{}
	keys := map[string]template.Field{}
	for _, s := range sections {
		titles[s.Title] = s.Fields
		for _, f := range s.Fields {
			keys[f.Key] = f
		}
	}
	for _, want := range []string{"Connection", "TLS", "SSH tunnel"} {
		if _, ok := titles[want]; !ok {
			t.Fatalf("no %q section: %+v", want, sections)
		}
	}
	for _, want := range []string{"access", "host", "database", "username", "password", "tls.mode", "tls.client_key", "ssh.host", "ssh.private_key"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("the form is missing %q", want)
		}
	}
	// Each field is in the section it belongs to: the grouping is not a UI guess.
	for _, f := range titles["SSH tunnel"] {
		if !strings.HasPrefix(f.Key, "ssh.") {
			t.Fatalf("a non-ssh field landed in the SSH section: %q", f.Key)
		}
	}
	for _, f := range titles["TLS"] {
		if !strings.HasPrefix(f.Key, "tls.") {
			t.Fatalf("a non-tls field landed in the TLS section: %q", f.Key)
		}
	}
	if !keys["password"].Secret || !keys["ssh.private_key"].Secret {
		t.Fatal("a secret field is not marked secret")
	}
	if keys["access"].Type != template.FieldSelect || len(keys["access"].Options) != 3 {
		t.Fatalf("the access field is wrong: %+v", keys["access"])
	}
	if _, err := tmpl().Fields("no-such-driver"); err == nil {
		t.Fatal("fields for an unknown driver should fail")
	}
}

// The sealed paths are the driver's secret and the SSH secrets, so the app knows
// exactly what to encrypt in the config JSON.
func TestTemplateSecretPaths(t *testing.T) {
	paths := map[string]bool{}
	for _, p := range tmpl().SecretPaths("mysql") {
		paths[p] = true
	}
	for _, want := range []string{"password", "tls.client_key", "ssh.password", "ssh.private_key", "ssh.passphrase"} {
		if !paths[want] {
			t.Fatalf("secret path %q is not sealed", want)
		}
	}
}

// Build turns the admin's flat form values into a self-describing tool and a
// nested connection config, and refuses input it cannot make a valid tool from.
func TestTemplateBuild(t *testing.T) {
	in := template.Input{
		Alias:   "prod_orders",
		Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": "db.internal", "port": float64(3306),
			"database": "orders", "username": "app_ro", "password": "secret",
			"tls.mode": "require",
		},
	}
	inst, err := tmpl().Build(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inst.Schema.Name != "query_prod_orders" || inst.Schema.Kind != tool.KindCustom {
		t.Fatalf("schema wrong: %+v", inst.Schema)
	}
	if inst.Schema.Risk != tool.RiskReadOnly {
		t.Fatalf("read access should be read-only risk: %q", inst.Schema.Risk)
	}
	// The dotted key became nested, and the driver was set.
	var cfg map[string]any
	if err := json.Unmarshal(inst.Config, &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg["driver"] != "mysql" {
		t.Fatalf("driver not set: %+v", cfg)
	}
	if tls, ok := cfg["tls"].(map[string]any); !ok || tls["mode"] != "require" {
		t.Fatalf("tls.mode was not nested: %+v", cfg["tls"])
	}
	// A write access raises the risk.
	in.Settings["access"] = "both"
	inst2, _ := tmpl().Build(in)
	if inst2.Schema.Risk != tool.RiskInternalWrite {
		t.Fatalf("write access should not be read-only risk: %q", inst2.Schema.Risk)
	}
}

func TestTemplateBuildRefusesBadInput(t *testing.T) {
	base := func() template.Input {
		return template.Input{Alias: "ok", Variant: "mysql", Settings: map[string]any{
			"access": "read", "host": "h", "database": "d", "username": "u",
		}}
	}
	bad := base()
	bad.Alias = "Has Spaces"
	if _, err := tmpl().Build(bad); err == nil {
		t.Fatal("a bad alias was accepted")
	}
	bad = base()
	bad.Variant = "oracle-not-built"
	if _, err := tmpl().Build(bad); err == nil {
		t.Fatal("an unknown driver was accepted")
	}
	bad = base()
	bad.Settings["access"] = "admin"
	if _, err := tmpl().Build(bad); err == nil {
		t.Fatal("a bad access mode was accepted")
	}
}

// The bound handler runs a real query end to end: Build the config from the
// local database, Bind it, and call the handler as the model would.
func TestTemplateBindRunsAQuery(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping bound-handler suite")
	}
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	inst, err := tmpl().Build(template.Input{
		Alias: "local", Variant: "mysql",
		Settings: map[string]any{
			"access": "read", "host": host, "port": float64(port),
			"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
			"tls.mode": "disable",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	handler, err := tmpl().Bind(inst.Config, tool.OwnerNone)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"sql": "SELECT 1 AS n"})
	res, err := handler(context.Background(), tool.Call{Args: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Failed() {
		t.Fatalf("a good query failed: %s", res.Content)
	}
	if !strings.Contains(string(res.Content), `"columns"`) {
		t.Fatalf("the result did not carry rows: %s", res.Content)
	}

	// A write through a read-mode tool is refused as bad arguments.
	writeArgs, _ := json.Marshal(map[string]any{"sql": "DELETE FROM t"})
	ref, _ := handler(context.Background(), tool.Call{Args: writeArgs})
	if ref.Err != tool.ErrorBadArguments {
		t.Fatalf("a write in read mode should be bad arguments: %q", ref.Err)
	}
}

// The policy is the last thing on the form, because it is the one part of it
// about the assistant rather than about the connection.
func TestTheFormEndsWithThePolicy(t *testing.T) {
	sections, err := tmpl().Fields("mysql")
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	var titles []string
	for _, s := range sections {
		titles = append(titles, s.Title)
	}
	if got := strings.Join(titles, " | "); got != "Connection | TLS | SSH tunnel | Policy" {
		t.Fatalf("sections = %q", got)
	}

	policy := sections[len(sections)-1]
	keys := map[string]template.Field{}
	for _, f := range policy.Fields {
		if !strings.HasPrefix(f.Key, "policy.") {
			t.Fatalf("a field that is not part of the policy landed in it: %q", f.Key)
		}
		keys[f.Key] = f
	}
	for _, want := range []string{"policy.table_mode", "policy.tables", "policy.field_mode", "policy.fields"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("the policy is missing %q: %+v", want, policy.Fields)
		}
	}
	// Each list is read the way its own rule says, and a new tool starts as the
	// tool was before there was a policy at all: everything in reach, nothing
	// hidden.
	for _, key := range []string{"policy.table_mode", "policy.field_mode"} {
		field := keys[key]
		if field.Type != template.FieldSelect || len(field.Options) != 2 {
			t.Fatalf("%s should offer the two rules: %+v", key, field)
		}
		if field.Default != string(sqlguard.ModeDenylist) {
			t.Fatalf("%s should start as a denylist, got %q", key, field.Default)
		}
	}
	if keys["policy.tables"].Default != "" || keys["policy.fields"].Default != "" {
		t.Fatal("a new tool starts with nothing kept back")
	}
}

func TestAPolicyIsStoredAndReadBack(t *testing.T) {
	settings := map[string]any{
		"access": "read", "host": "db.internal", "database": "shop", "username": "app_ro",
		"policy.table_mode": "denylist", "policy.tables": "secret_keys\nlog_*",
		"policy.field_mode": "allowlist", "policy.fields": "customers.id\ncustomers.email",
	}
	config, err := tmpl().Config("mysql", settings)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	parsed, err := ParseConfig(config)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Policy.Active() {
		t.Fatal("the policy names tables and fields, so it governs something")
	}
	// The lists come back exactly as they were typed, which is what the edit form
	// shows the administrator.
	if parsed.Policy.Tables != "secret_keys\nlog_*" || parsed.Policy.Fields != "customers.id\ncustomers.email" {
		t.Fatalf("the lists were not kept as typed: %+v", parsed.Policy)
	}
	if parsed.Policy.TableMode != sqlguard.ModeDenylist || parsed.Policy.FieldMode != sqlguard.ModeAllowlist {
		t.Fatalf("each list keeps its own rule: %+v", parsed.Policy)
	}
	// It reaches the enforcement it was written for.
	if !parsed.Policy.AllowsTable("orders") || parsed.Policy.AllowsTable("secret_keys") {
		t.Fatal("the table rule did not survive the round trip")
	}
	if parsed.Policy.MasksColumn("customers", "email") || !parsed.Policy.MasksColumn("customers", "ssn") {
		t.Fatal("the field rule did not survive the round trip")
	}

	// A tool with no policy is unchanged: nothing to enforce, nothing enforced.
	delete(settings, "policy.tables")
	delete(settings, "policy.fields")
	delete(settings, "policy.field_mode")
	config, err = tmpl().Config("mysql", settings)
	if err != nil {
		t.Fatalf("config without a policy: %v", err)
	}
	parsed, _ = ParseConfig(config)
	if parsed.Policy.Active() {
		t.Fatal("a policy that names nothing governs nothing")
	}
}

func TestAPolicyThatCouldNotBeEnforcedIsRefusedAtTheForm(t *testing.T) {
	base := map[string]any{"access": "read", "host": "db.internal", "database": "shop", "username": "app_ro"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	// A list with no rule is NOT one of these. Everything that reads a rule
	// treats a blank one as a denylist, which is what a new tool starts as and
	// what the form shows, so refusing it here would be this one check
	// disagreeing with the rest about a question they had already settled.
	if _, err := tmpl().Config("mysql", with(map[string]any{"policy.tables": "secret_keys"})); err != nil {
		t.Fatalf("a list with no rule is read as a denylist, not refused: %v", err)
	}

	for _, c := range []struct {
		name     string
		settings map[string]any
	}{
		{"an allowlist that names no table", with(map[string]any{"policy.table_mode": "allowlist"})},
		{"an allowlist that names no field", with(map[string]any{"policy.field_mode": "allowlist", "policy.fields": ""})},
		{"a rule nobody recognises", with(map[string]any{"policy.table_mode": "sometimes", "policy.tables": "x"})},
		{"a field without its table", with(map[string]any{"policy.field_mode": "denylist", "policy.fields": "password"})},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := tmpl().Config("mysql", c.settings); err == nil {
				t.Fatal("expected the form to refuse it")
			}
		})
	}
}

// A tool with a policy binds like any other. What the policy needs to be
// enforced (the database's own account of what it holds) is read when a
// statement runs, not when the tool is loaded, because a loadout is assembled
// every turn and a database is not asked about itself every turn.
func TestAToolWithAPolicyBindsLikeAnyOther(t *testing.T) {
	settings := map[string]any{
		"access": "read", "host": "db.internal", "database": "shop", "username": "app_ro",
		"policy.table_mode": "denylist", "policy.tables": "secret_keys",
		"policy.field_mode": "denylist", "policy.fields": "customers.ssn",
	}
	config, err := tmpl().Config("mysql", settings)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	handler, err := tmpl().Bind(config, tool.OwnerNone)
	if err != nil {
		t.Fatalf("a tool with a policy should bind: %v", err)
	}
	if handler == nil {
		t.Fatal("no handler came back")
	}
}

// The proxy setting is offered only where there is something to proxy through.
//
// On a personal installation the gateway is already on the person's own
// computer, so a route through the chat application would be a route from here
// to here. The form leaves it out rather than showing a control that can only
// ever do nothing.
func TestTheProxySettingIsAbsentWhereThereIsNothingToReachThrough(t *testing.T) {
	withMachines := New(stubMachines{})
	sections, err := withMachines.Fields("mysql")
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	if !hasField(sections, template.ReachChatKey) {
		t.Fatal("an installation that can reach through the chat application does not offer it")
	}

	sections, err = New(nil).Fields("mysql")
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	if hasField(sections, template.ReachChatKey) {
		t.Fatal("a personal installation offers a proxy it has nothing to proxy through")
	}
}

func hasField(sections []template.Section, key string) bool {
	for _, section := range sections {
		for _, field := range section.Fields {
			if field.Key == key {
				return true
			}
		}
	}
	return false
}

// stubMachines stands for a link that exists. Nothing calls through it here:
// what is being asked is whether the FORM changes shape, not what happens when
// somebody uses it.
type stubMachines struct{}

func (stubMachines) Dial(context.Context, int64, int64, string, string, int, string) (net.Conn, error) {
	return nil, nil
}
func (stubMachines) Online(int64, int64, string) bool { return false }
