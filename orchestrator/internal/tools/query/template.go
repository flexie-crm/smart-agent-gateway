package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/tools/toolkit"
)

// TemplateName is the query template's id and the prefix of every tool it makes.
const TemplateName = "query"

// New builds the query template: the recipe for a native custom tool that runs
// SQL against a configured database. Its variants are the datasource drivers, so
// adding a driver there adds it here for free.
// New builds the template. machines is how a tool configured to be reached
// through the chat application opens its connections; nil is an installation
// that does not offer that, and such a tool is refused rather than run direct.
func New(machines template.Machines) template.Template { return queryTemplate{machines: machines} }

type queryTemplate struct {
	// machines is the chat applications connected right now. Nil on an
	// installation that does not offer this, and a tool configured to use it is
	// then refused at Bind rather than at the first query.
	machines template.Machines
}

// reachFor carries this call's connections through the caller's own computer.
func (t queryTemplate) reachFor(call tool.Call) *datasource.Reach {
	return &datasource.Reach{
		Describe: "the chat application",
		Dial: func(ctx context.Context, host string, port int) (net.Conn, error) {
			return t.machines.Dial(ctx, call.WorkspaceID, call.UserID, call.DeviceID, host, port, "a database query")
		},
	}
}

func (queryTemplate) Name() string  { return TemplateName }
func (queryTemplate) Title() string { return "Query Database" }
func (queryTemplate) Description() string {
	return "Run SQL against a database you connect to. Read, write, or both, within the access you choose."
}

// Variants are the database drivers, drawn from the shared datasource registry.
func (queryTemplate) Variants() []template.Variant {
	var out []template.Variant
	for _, d := range datasource.All() {
		out = append(out, template.Variant{Key: d.Key(), Label: d.Label()})
	}
	return out
}

// Fields composes the settings form for a driver into sections: the access mode
// and the driver's own connection fields under "Connection", and the shared SSH
// tunnel fields under their own section. The grouping lives here, in the
// template, not in the console.
func (t queryTemplate) Fields(variant string) ([]template.Section, error) {
	d, ok := datasource.Get(variant)
	if !ok {
		return nil, fmt.Errorf("unknown database driver %q", variant)
	}
	access := template.Field{
		Key: "access", Label: "Access", Type: template.FieldSelect,
		Options: []template.Option{
			template.Choice(string(AccessRead), "Read only"),
			template.Choice(string(AccessWrite), "Write only"),
			template.Choice(string(AccessBoth), "Read and write"),
		},
		Default: string(AccessRead), Required: true, Span: 3,
		Help: "What the tool may do: read, write, or both.",
	}
	// Weave access in right after the driver's endpoint fields (host, port), so
	// it sits with the database it points at rather than ahead of the address.
	var connection []template.Field
	inserted := false
	for _, f := range d.Fields() {
		if !inserted && !f.Endpoint {
			connection = append(connection, access)
			inserted = true
		}
		connection = append(connection, convertField(f))
	}
	if !inserted {
		connection = append(connection, access)
	}
	// Where the address is reached FROM, beside where it is. A database only the
	// person's own computer can see has no address this server could be given.
	//
	// Absent, not disabled, where there is nothing to reach through: a control
	// that can only ever do nothing is worse than no control. Every edition has
	// a link now, the personal one included, so the setting is normally there.
	if t.machines != nil {
		connection = append(connection, template.ReachChatField("database"))
	}

	return []template.Section{
		{Title: "Connection", Fields: connection},
		{
			Title:  "TLS",
			Hint:   "Secure the connection, and, if the database asks for it, present a client certificate.",
			Fields: convertFields(datasource.TLSFields()),
		},
		{
			Title:  "SSH tunnel",
			Hint:   "Reach a database that is only accessible through a bastion. Leave blank for a direct connection.",
			Fields: convertFields(datasource.SSHFields()),
		},
		policySection(),
	}, nil
}

// policySection is what this tool may see of the database. It comes last
// because it is the one part of the form that is about the assistant rather
// than about the connection.
func policySection() template.Section {
	return template.Section{
		Title: "Policy",
		Hint: "What this tool may see. Leave it blank and it sees everything the account it connects as can reach.\n\n" +
			"- Tables: a denylist reaches everything except what you list; an allowlist reaches only what you list.\n" +
			"- Fields: a denylist hides the fields you list; an allowlist shows only the fields you list, and only for the tables it names, so a field added to one of those tables later is hidden from the day it appears.\n" +
			"- One entry per line. A table is written on its own (orders, log_*). A field is written with the table it belongs to (customers.ssn, customers.*, *.password). A * stands for any run of characters.\n" +
			"- A hidden field comes back as " + sqlguard.Hidden + " wherever it is selected, on its own or inside an expression. It can still be used to decide which rows come back, what order they are in, and what they join to: what is kept back is the value, not the field.\n" +
			"- A query that asks about a hidden field gets an honest answer about it (how many rows match, which come first), so somebody determined can narrow a value down a question at a time. What this guarantees is that the value itself is never in the answer.\n\n" +
			"The statement is read before it runs, so a table you keep back is out of reach through a view, a WITH, or a subquery as surely as directly, and it is left out when the tables are listed. This stops a mistake and closes the ways round a list of names; " +
			"what really bounds this tool is the account it connects as, so take the grant away as well.",
		Fields: []template.Field{
			{
				Key: "policy.table_mode", Label: "Table rule", Type: template.FieldSelect, Required: true,
				Options: modeChoices("tables"),
				// A new tool starts as a denylist with nothing in it, which is the
				// tool as it was before there was a policy at all: it sees the whole
				// database, and an administrator narrows it when they mean to.
				Default: string(sqlguard.ModeDenylist), Span: 3,
			},
			{
				Key: "policy.field_mode", Label: "Field rule", Type: template.FieldSelect, Required: true,
				Options: modeChoices("fields"),
				Default: string(sqlguard.ModeDenylist), Span: 3,
			},
			{
				Key: "policy.tables", Label: "Tables", Type: template.FieldTextarea,
				Help: "Read as the table rule says. Blank with a denylist means every table is in reach.",
			},
			{
				Key: "policy.fields", Label: "Fields", Type: template.FieldTextarea,
				Help: "Each one written as table.field. Read as the field rule says; blank with a denylist means nothing is hidden.",
			},
		},
	}
}

// Display is what a person sees when they open one of these calls: the
// statement, and the rows as a table.
//
// A result set is columns AND rows, which this answers with as two fields
// because that is the compact way to carry it; drawn as two fields it is a
// list of names above a list of lists, which is the shape of the data and not
// the shape of an answer. Named together they are one table.
//
// The rows and nothing about the rows: row_count is them counted and they are
// on the screen, and truncated is a fact for the model deciding whether to ask
// again. The statement is SQL rather than a shell command, and is painted and
// laid out as one.
func (queryTemplate) Display() tool.Display {
	return tool.Display{
		Sent: []tool.Shown{tool.SQL("sql"), tool.Value("params")},
		Answered: []tool.Shown{
			tool.Table("rows", "columns"),
			// A write returns no rows, so these are its whole answer.
			tool.Value("rows_affected"),
			tool.Value("insert_id"),
		},
	}
}

// SecretPaths are the config keys sealed at rest: the driver's secrets and the
// SSH secrets, both in dotted-path form the app seals in the config JSON.
func (queryTemplate) SecretPaths(variant string) []string {
	paths := datasource.SecretFieldKeys(variant)
	paths = append(paths, datasource.TLSSecretFields()...)
	paths = append(paths, datasource.SSHSecretFields()...)
	return paths
}

// Params are the inputs the query tool takes. They are fixed (the handler
// expects exactly these), so an administrator sees them but changes only their
// descriptions, to add context like which tables a database has.
func (queryTemplate) Params() []template.Param {
	return []template.Param{
		{Key: "sql", Type: "string", Required: true, Description: "The SQL statement to run. One statement only. Use ? placeholders for values and pass them in params."},
		{Key: "params", Type: "array", Required: false, Description: "Optional positional values for the ? placeholders in sql, in order."},
		{Key: "max_rows", Type: "integer", Required: false, Description: "Optional cap on how many rows to return, up to the tool's own limit."},
	}
}

// DefaultGuide is the plain-text guide the form prefills, which an administrator
// refines for their database.
func (queryTemplate) DefaultGuide() string {
	return "Run exactly one SQL statement per call. Never build a query by pasting values into the SQL " +
		"string: put a ? where each value goes and pass the values in params, in order. The access mode " +
		"(read, write, or both) is fixed for this tool and enforced on every statement. Reads come back capped; " +
		"if the result is marked truncated it is incomplete, so narrow the query rather than reading a cut-off " +
		"result as whole.\n\nAdd the tables and columns this database has, and any conventions, so the model " +
		"writes correct queries against it."
}

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)

// Build validates the admin's input and produces the self-describing tool to
// store: its name (query_<alias>), a description that says what it connects to
// and what it may do, and the call schema. The connection settings become the
// config JSON, with secrets still plaintext for the app to seal.
func (queryTemplate) Build(in template.Input) (template.Instance, error) {
	alias := strings.ToLower(strings.TrimSpace(in.Alias))
	if !aliasPattern.MatchString(alias) {
		return template.Instance{}, fmt.Errorf("the name must start with a letter and use only lowercase letters, numbers, and underscores")
	}
	if _, ok := datasource.Get(in.Variant); !ok {
		return template.Instance{}, fmt.Errorf("unknown database driver %q", in.Variant)
	}
	access := Access(fmt.Sprint(in.Settings["access"]))
	if !access.Valid() {
		return template.Instance{}, fmt.Errorf("access must be read, write, or both")
	}

	config, err := queryTemplate{}.Config(in.Variant, in.Settings)
	if err != nil {
		return template.Instance{}, err
	}

	risk := tool.RiskReadOnly
	if access != AccessRead {
		risk = tool.RiskInternalWrite
	}
	label := describeLabel(alias, in.Variant, access)

	// The presentation fields fall back to the template's defaults when blank.
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = label
	}
	description := strings.TrimSpace(in.Description)
	if description == "" {
		description = describeDescription(label, in.Variant, access)
	}
	guide := strings.TrimSpace(in.Guide)
	if guide == "" {
		guide = queryTemplate{}.DefaultGuide()
	}

	return template.Instance{
		Schema: tool.Schema{
			Name:         TemplateName + "_" + alias,
			FriendlyName: display,
			Description:  description,
			InputSchema:  buildInputSchema(queryTemplate{}.Params(), in.ParamDescriptions),
			Kind:         tool.KindCustom,
			Risk:         risk,
		},
		Config: config,
		Guide:  guide,
	}, nil
}

// buildInputSchema assembles the tool's JSON Schema from its fixed params, using
// the administrator's edited description for each where given. The param
// identities (key, type, required) are always the template's.
func buildInputSchema(params []template.Param, descriptions map[string]string) json.RawMessage {
	properties := map[string]any{}
	var required []string
	for _, p := range params {
		desc := p.Description
		if override, ok := descriptions[p.Key]; ok && strings.TrimSpace(override) != "" {
			desc = override
		}
		prop := map[string]any{"type": p.Type, "description": desc}
		if p.Type == "array" {
			prop["items"] = map[string]any{}
		}
		properties[p.Key] = prop
		if p.Required {
			required = append(required, p.Key)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// Config builds the nested connection config from the flat form settings: the
// driver, the access mode, and the connection fields (dotted keys like tls.mode
// and ssh.host expanded into objects). It must parse as a connection, so a
// broken one is refused at create or test time, not on first use.
func (queryTemplate) Config(variant string, settings map[string]any) (json.RawMessage, error) {
	cfg := nest(settings)
	cfg["driver"] = variant
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("the settings could not be encoded")
	}
	if _, err := ParseConfig(config); err != nil {
		return nil, err
	}
	return config, nil
}

// Bind builds the live handler for a stored instance. The config's secrets have
// already been opened by the app, so the handler just parses it, runs the SQL
// through the gate and the shared datasource, and shapes the result.
// Bind takes an owner and does not use it: a query holds nothing between calls,
// so there is no state to file under an agent. Two agents querying at once get
// independent work through the shared pool, which is what a pool is for.
func (t queryTemplate) Bind(config json.RawMessage, _ tool.Owner) (tool.Handler, error) {
	settings, err := ParseConfig(config)
	if err != nil {
		return nil, err
	}
	if settings.ThroughChat && t.machines == nil {
		return nil, fmt.Errorf("this installation cannot reach a database through the chat application")
	}
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			SQL     string `json:"sql"`
			Params  []any  `json:"params"`
			MaxRows int    `json:"max_rows"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil || strings.TrimSpace(args.SQL) == "" {
			return toolkit.BadArguments("provide the SQL statement to run in sql")
		}
		// A copy per call, because the reach belongs to the CALLER: the same
		// bound tool serves two people, and the connection has to be carried
		// through the computer of whoever is asking. Writing it into the shared
		// settings would be a race and, worse, would send one person's query
		// through another person's machine.
		use := settings
		if settings.ThroughChat {
			use.Connection.Reach = t.reachFor(call)
		}
		res, reason, err := run(ctx, use, args.SQL, args.Params, args.MaxRows)
		if reason != "" {
			return toolkit.BadArguments(reason)
		}
		if err != nil {
			// A read that ran past its deadline was cancelled on the database and
			// comes back with its plan, so the model can see why it was slow and
			// narrow it, rather than only that it was slow.
			var timeout *datasource.QueryTimeout
			if errors.As(err, &timeout) {
				return toolkit.Failed(heavyQueryMessage(timeout.Plan))
			}
			// A database error (a bad column, a missing table) is useful to the
			// model, which corrects its SQL; it is the customer's own database
			// error, not an internal one.
			return toolkit.Failed(cleanDBError(err))
		}
		return toolkit.Success(res)
	}, nil
}

// Test opens a live connection for a resolved config and checks it, for the
// admin's Test button.
//
// A policy is proved here too, against the database it is written for. A rule
// that names a table with a typo in it keeps nothing back and says nothing about
// it, and somebody who wrote that rule believes the table is out of reach. This
// is the moment to say otherwise, while they are looking at the form.
func (queryTemplate) Test(ctx context.Context, config json.RawMessage) error {
	settings, err := ParseConfig(config)
	if err != nil {
		return err
	}
	// The one connection that cannot be tested from here, because the whole
	// point of it is that this server cannot reach the database: the address
	// exists on somebody else's computer, through their chat application, and
	// that person is not the administrator filling in this form. Testing the
	// administrator's own machine would prove a different computer.
	//
	// So what CAN be checked is: the settings parse, the driver is real, the
	// access mode and the policy are valid. That is everything above except the
	// wire itself. The wire is answered on the first call instead, in words that
	// name the reason ("no chat application is connected for this person"), and
	// a policy is checked against the live catalogue then rather than now.
	if settings.ThroughChat {
		return nil
	}
	conn, err := datasource.Open(settings.Connection)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Ping(ctx); err != nil {
		return err
	}
	if !settings.Policy.Active() {
		return nil
	}

	schema, err := conn.Schema(ctx)
	if err != nil {
		return fmt.Errorf("the policy could not be checked against the database: %w", err)
	}
	guard, err := sqlguard.New(settings.Connection.Driver, settings.Policy, catalogOf(settings.Connection.Driver, schema))
	if err != nil {
		return err
	}
	if missing := guard.Unmatched(); len(missing) > 0 {
		return fmt.Errorf("this database has no %s. Check the spelling, or take that line out: a name that matches nothing has no effect, so the policy would not do what it looks like it does",
			strings.Join(missing, ", "))
	}
	return nil
}

// Documentation is what the model always knows about a query tool no matter how
// an administrator edits the form: which database engine it is and its SQL
// flavour (appended to the description), and the drill-down topics, the engine
// and its introspection, and the row caps and heavy-query timeout.
func (queryTemplate) Documentation(config json.RawMessage) template.Documentation {
	d, ok := datasource.Get(driverKey(config))
	if !ok {
		return template.Documentation{}
	}
	dialect := d.Dialect()
	note := "This tool runs against a " + dialect.Name + " database. " + dialect.SQLNote
	topics := []tool.Topic{
		{
			ID:    "engine",
			Title: "Which database this is, and how to explore it",
			Body:  "This tool queries a " + dialect.Name + " database. " + dialect.SQLNote + "\n\n" + dialect.Introspection,
			Edges: []tool.TopicEdge{{To: "limits", Type: tool.EdgeCompanion, When: "before running a broad or unfamiliar read"}},
		},
		{
			ID:    "limits",
			Title: "Row caps and the heavy-query timeout",
			Body: fmt.Sprintf("A read returns at most %d rows by default and %d at the most; pass max_rows to lower it, and a result marked truncated is incomplete. "+
				"A read is given %d seconds: past that it is cancelled on the database (so a heavy query cannot lock it) and you get its EXPLAIN plan instead of rows. "+
				"When that happens, read the plan for a full scan or a missing index and narrow the query with a WHERE, a smaller LIMIT, or an indexed column.",
				defaultMaxRows, hardMaxRows, int(queryTimeout.Seconds())),
			Edges: []tool.TopicEdge{{To: "engine", Type: tool.EdgeCompanion, When: "to recall the engine's introspection commands"}},
		},
	}
	if settings, err := ParseConfig(config); err == nil && settings.Policy.Active() {
		// The model is told THAT part of the database is kept back, and never
		// which part: naming a table nobody may reach would undo the keeping back.
		// It is told because a refusal it cannot account for is a refusal it
		// spends the rest of the turn trying to word its way around.
		note += " Part of this database is kept back from it: some tables cannot be reached, and some fields come back as " +
			sqlguard.Hidden + " instead of their value. Ask for this tool's guide before writing a query against it."
		topics = append(topics, policyTopic(settings.Policy))
		for i := range topics {
			if topics[i].ID == "engine" {
				topics[i].Edges = append(topics[i].Edges, tool.TopicEdge{
					To: "policy", Type: tool.EdgeCompanion, When: "before assuming a table or a field is there to be read",
				})
			}
		}
	}
	return template.Documentation{Note: note, Topics: topics}
}

// policyTopic is what the assistant is told about what it may see.
//
// What it may reach is named; what it may not is never named. An allowlist is
// therefore the more useful of the two to work with, because the list itself is
// the answer to "what is here", and saying it costs nothing. A denylist can only
// be described, because describing it the other way round would be handing over
// exactly the names it exists to keep quiet.
func policyTopic(policy sqlguard.Policy) tool.Topic {
	var b strings.Builder
	b.WriteString("Part of this database is kept back from this tool by its configuration. This is fixed: it is not about your permissions, ")
	b.WriteString("it does not change between calls, and rewording a query will not get past it. When you are refused, say so and move on.\n\n")

	if policy.TableMode == sqlguard.ModeAllowlist {
		b.WriteString("Tables you can reach, and no others:\n")
		for _, name := range lines(policy.Tables) {
			b.WriteString("- ")
			b.WriteString(name)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("Some tables cannot be reached. They are not listed when you ask what this database holds, and they are ")
		b.WriteString("out of reach through a view, a WITH, or a subquery just as much as directly, so there is no route to one.\n")
	}

	b.WriteString("\nSome fields come back as ")
	b.WriteString(sqlguard.Hidden)
	b.WriteString(" instead of their value, wherever you select them: on their own, or inside an expression like CONCAT or an ")
	b.WriteString("aggregate, in which case the whole item comes back that way. You can still USE those fields normally: put them in a ")
	b.WriteString("WHERE, an ORDER BY, a GROUP BY, a HAVING, or a join condition and they behave as they really are, so counting, ")
	b.WriteString("filtering, sorting and joining all work. It is only the value in the answer that is kept back.\n")
	b.WriteString("\nTwo things are refused, and neither is worth working around: writing to a hidden field (this tool cannot know ")
	b.WriteString("what is really there), and copying one into another column with SET (that would put the real value somewhere ")
	b.WriteString("readable). Say so and carry on.\n")
	if policy.FieldMode == sqlguard.ModeAllowlist {
		b.WriteString("\nFields you can see the value of:\n")
		for _, name := range lines(policy.Fields) {
			b.WriteString("- ")
			b.WriteString(name)
			b.WriteString("\n")
		}
		b.WriteString("Every other field of the tables named there is hidden.\n")
	}
	b.WriteString("\nA SELECT * is answered with the columns it stands for, with the hidden ones replaced, so it stays a good way to see ")
	b.WriteString("what a table holds.")

	return tool.Topic{
		ID:    "policy",
		Title: "What this tool can and cannot see",
		Body:  b.String(),
		Edges: []tool.TopicEdge{{To: "engine", Type: tool.EdgeCompanion, When: "to recall how to list what is here"}},
	}
}

// lines splits one of the policy's lists the way the policy itself reads it.
func lines(list string) []string {
	var out []string
	for _, line := range strings.Split(list, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// heavyQueryMessage is what the model reads when a read was cancelled for
// running too long: what happened, and the plan to fix it.
func heavyQueryMessage(plan string) string {
	return fmt.Sprintf("the query did not return within %d seconds and was cancelled on the database, so no rows came back. "+
		"It looks heavy: a full table scan, a missing index, or a very large result. Here is its query plan (EXPLAIN); use it "+
		"to narrow the query with a WHERE, a smaller LIMIT, or an indexed column, then try again:\n\n%s",
		int(queryTimeout.Seconds()), plan)
}

// driverKey reads the driver from a connection config (it is not a secret).
func driverKey(config json.RawMessage) string {
	var c struct {
		Driver string `json:"driver"`
	}
	_ = json.Unmarshal(config, &c)
	return c.Driver
}

// modeChoices says what a list rule DOES, rather than naming it. "denylist" is
// a word for the code; what an administrator is choosing between is whether the
// list they are about to write is the exceptions or the whole permission.
func modeChoices(what string) []template.Option {
	return []template.Option{
		template.Choice(string(sqlguard.ModeDenylist), "All "+what+" except the ones I list"),
		template.Choice(string(sqlguard.ModeAllowlist), "Only the "+what+" I list"),
	}
}

func convertField(f datasource.Field) template.Field {
	options := make([]template.Option, len(f.Options))
	for i, o := range f.Options {
		options[i] = template.Choice(o.Value, o.Label)
	}
	return template.Field{
		Key: f.Key, Label: f.Label, Type: template.FieldType(f.Type),
		Required: f.Required, Secret: f.Secret, Options: options,
		Default: f.Default, Help: f.Help, Span: f.Span,
	}
}

func convertFields(in []datasource.Field) []template.Field {
	out := make([]template.Field, len(in))
	for i, f := range in {
		out[i] = convertField(f)
	}
	return out
}

// nest expands dotted keys (tls.mode, ssh.host) into a nested object, so the
// flat form values become the connection config the datasource reads.
func nest(flat map[string]any) map[string]any {
	out := map[string]any{}
	for key, val := range flat {
		parts := strings.Split(key, ".")
		m := out
		for i, p := range parts {
			if i == len(parts)-1 {
				m[p] = val
				break
			}
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
	}
	return out
}

func describeLabel(alias, driver string, access Access) string {
	return fmt.Sprintf("Query %s (%s, %s)", alias, driver, access)
}

func describeDescription(label, driver string, access Access) string {
	verb := "Read from"
	switch access {
	case AccessWrite:
		verb = "Write to"
	case AccessBoth:
		verb = "Read from and write to"
	}
	// Short: how to write a good query against THIS database, and what it will
	// refuse, is what its guide is for (tool_guide).
	return fmt.Sprintf("%s a %s database by running SQL (%s). The statement goes in sql, values for "+
		"its ? placeholders in params. Read its guide before writing a query.",
		verb, driver, label)
}

// cleanDBError strips the runner's wrapping prefixes so the model sees the
// database's own message, not our plumbing.
func cleanDBError(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"run query: ", "run statement: ", "read row: ", "read rows: ", "read columns: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return "the query could not be run: " + msg
}
