package sshtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// New builds the SSH template: the recipe for a native custom tool bound to one
// configured server. Registered once at wiring time, it produces as many tools
// as there are servers to reach.
//
// It owns the connections those tools work over, because a tool's handler is
// rebuilt every turn and a connection that lived there would be signed in and
// thrown away with each one. Close disconnects from every server.
// New builds the template. machines is how a tool configured to be reached
// through the chat application opens its connections; nil is an installation
// that does not offer that.
func New(machines template.Machines) *sshTemplate {
	return &sshTemplate{
		pool: newPool(), tests: newPendingTests(defaultCodeWindow),
		sessions: newSessions(), machines: machines,
	}
}

type sshTemplate struct {
	pool *pool
	// machines carries a connection to a server this gateway cannot reach,
	// through the computer of whoever is using the tool.
	machines template.Machines
	// sessions are the interactive programs open across every server, held here
	// for the same reason the connections are: they outlive the turn that started
	// them, and something has to close them.
	sessions *sessions
	// tests holds the test sign-ins waiting on an administrator's verification
	// code. They belong to the template rather than to a request, for the same
	// reason the connections do: the answer arrives on a later one.
	tests *pendingTests
}

// Close disconnects from every server this template's tools were working on. It
// is called once, when the process is shutting down.
func (t *sshTemplate) Close() {
	t.sessions.Close()
	t.pool.Close()
}

func (sshTemplate) Name() string  { return TemplateName }
func (sshTemplate) Title() string { return "Remote Server" }
func (sshTemplate) Description() string {
	return "Work on a remote server you connect to, within the commands you permit."
}

// Variants is the one kind of SSH there is. Several servers means several tools,
// not several variants.
func (sshTemplate) Variants() []template.Variant {
	return []template.Variant{{Key: VariantSSH, Label: "SSH"}}
}

// Fields are the settings to collect, grouped the way an administrator fills
// them in: where the server is, how to sign in to it, and what one tool may hold
// open at once.
func (t *sshTemplate) Fields(variant string) ([]template.Section, error) {
	if variant != VariantSSH {
		return nil, fmt.Errorf("unknown variant %q", variant)
	}
	sections := []template.Section{
		{
			Title: "Connection",
			Hint: "Where the server is, and the account to work as.\n\n" +
				"- Give a password, a private key, or both: whichever the server accepts is used.\n" +
				"- Editing later, leave a secret blank to keep the one already saved.\n" +
				"- Everything secret is encrypted before it is stored, and the agent never sees any of it.",
			Fields: []template.Field{
				{Key: "host", Label: "Server address", Type: template.FieldText, Required: true, Span: 4},
				{Key: "port", Label: "Port", Type: template.FieldNumber, Default: "22", Span: 2},
				{Key: "username", Label: "Username", Type: template.FieldText, Required: true, Span: 3},
				{Key: "auth.password", Label: "Password", Type: template.FieldPassword, Secret: true, Span: 3},
				{
					Key: "auth.private_key", Label: "Private key", Type: template.FieldTextarea, Secret: true,
					Help: "Paste the whole key, from the BEGIN line to the END line.",
				},
				{
					Key: "auth.passphrase", Label: "Key passphrase", Type: template.FieldPassword, Secret: true, Span: 6,
					Help: "Only if the private key itself is protected by a passphrase.",
				},
				{
					Key: "host_key", Label: "Known host key", Type: template.FieldTextarea,
					Help: "The server's public key (authorized_keys form). Leave blank to skip host verification (less secure).",
				},
			},
		},
		{
			Title: "Limits",
			Hint: "How much this tool does at a time, and how patient it is.\n\n" +
				"- Work beyond the first limit waits its turn rather than failing.\n" +
				"- The tool reuses its sign-in, and signs in again when it needs more room than one allows.\n" +
				"- It disconnects after the idle time and reconnects when it is next needed.",
			Fields: []template.Field{
				{Key: "limits.max_channels", Label: "Jobs at once", Type: template.FieldNumber, Default: "4", Span: 3,
					Help: "How much work this tool runs on the server at the same time. Everything past it waits its turn."},
				{
					Key: "limits.jobs_per_connection", Label: "Jobs the server allows per connection",
					Type: template.FieldNumber, Default: "10", Span: 3,
					Help: "The server's limit, not this tool's: most allow 10 at once on one sign-in (MaxSessions in its " +
						"SSH configuration) and refuse the rest. Set it to match, and the tool signs in again rather " +
						"than being refused. Lower it if your server is stricter.",
				},
				{Key: "limits.idle_minutes", Label: "Idle timeout (minutes)", Type: template.FieldNumber, Default: "30", Span: 3},
				{Key: "limits.connect_seconds", Label: "Connect timeout (seconds)", Type: template.FieldNumber, Default: "10", Span: 3},
			},
		},
		{
			Title: "cmdpolicy.Policy",
			Hint: "Choose the rule, then list the commands it applies to.\n\n" +
				"- Denylist runs anything except what you list. It starts with the commands that destroy data, take the machine down, or lock you out.\n" +
				"- Allowlist is stronger: only what you list runs, and everything else is refused.\n" +
				"- One command per line. Just the program (systemctl) means it with any arguments; a longer line (systemctl restart application) means only that.\n\n" +
				"These lists stop a mistake, not a determined route around them: anything that can run a script or open a shell reaches past them. " +
				"What really bounds this tool is the account it signs in as, so give it one you would let run any command at all.",
			Fields: []template.Field{
				{
					Key: "policy.mode", Label: "Rule", Type: template.FieldSelect, Required: true,
					Options: []template.Option{
						template.Choice(string(cmdpolicy.PolicyDenylist), "Every command except the ones I list"),
						template.Choice(string(cmdpolicy.PolicyAllowlist), "Only the commands I list"),
					},
					// A new tool starts as a denylist, with the dangerous commands
					// already in it, because that is the tool people actually want: an
					// assistant that can work on the machine, with the damage it cannot
					// undo taken away. An allowlist is the stronger setting and stays
					// one deliberate choice away, for a server where the assistant
					// should only ever do a named handful of things.
					Default: string(cmdpolicy.PolicyDenylist), Span: 3,
				},
				{
					Key: "policy.sudo", Label: "Running as another user", Type: template.FieldSelect,
					Options: []template.Option{
						template.Choice(cmdpolicy.SudoDeny, "Refused"),
						template.Choice(cmdpolicy.SudoAllow, "Allowed"),
					},
					Default: cmdpolicy.SudoDeny, Span: 3,
				},
				{
					Key: "policy.allowed", Label: "Commands to allow", Type: template.FieldTextarea,
					Help: "Used when the rule is an allowlist. Everything not listed here is refused.",
				},
				{
					Key: "policy.denied", Label: "Commands to refuse", Type: template.FieldTextarea,
					Default: cmdpolicy.DefaultDenied,
					Help: "Used when the rule is a denylist. Listing rm also refuses /bin/rm, sudo rm, and rm used " +
						"inside a longer command. This starts from the commands that destroy data, take the machine " +
						"down, or lock you out of it; add or remove what suits this server.",
				},
			},
		},
	}

	// Where the address is reached FROM, beside where it is. Absent, not
	// disabled, where there is nothing to reach through: a control that can only
	// ever do nothing is worse than no control. Every edition has a link now,
	// including the personal one, so the setting is normally there.
	if t.machines != nil {
		sections[0].Fields = append(sections[0].Fields, template.ReachChatField("server"))
	}
	return sections, nil
}

// SecretPaths are the credential leaves the app seals at rest. All three are
// named whatever the chosen method: an empty one is skipped, and naming them all
// means switching method later cannot leave a secret in the clear.
// Display is what a person sees when they open one of these calls: the command
// and what the server printed.
//
// The rest of the answer exists for the MODEL. It is told whether the session
// is still running, what it is running, the exit code and a note about any of
// it, because it has to decide what to do next; a person reading the
// conversation wants the command and what came back, and `wait: 5` beside them
// is a parameter of the asking rather than part of what happened.
func (sshTemplate) Display() tool.Display {
	return tool.Display{
		Sent:     []tool.Shown{tool.Command("command"), tool.Value("input")},
		Answered: []tool.Shown{tool.Text("output")},
	}
}

func (sshTemplate) SecretPaths(string) []string {
	return []string{"auth.private_key", "auth.passphrase", "auth.password"}
}

// Params are the inputs the tool takes. They are fixed; an administrator may
// refine only their descriptions, to say what this particular server is for.
func (sshTemplate) Params() []template.Param {
	return []template.Param{
		{
			Key: "command", Type: "string", Required: false,
			Description: "The command to run, written exactly as you would type it on the server. One command per " +
				"call, and only the commands this tool permits will run. If it finishes you get its output and exit " +
				"code; if it stops to ask something, or keeps printing, you get what it printed so far and it stays " +
				"running for you to answer or read.",
		},
		{
			Key: "input", Type: "string", Required: false,
			Description: "What to type into the command that is still running, as a person would type it and press " +
				"return. Send it on its own: the running command is already known, so it does not need repeating. " +
				"This is also how you send a verification code when a call came back saying the server is asking for " +
				"one, and that one case does take the command again, so the work happens as soon as the sign-in " +
				"completes. When something asks for what only the person has, a password or a code, ask them and " +
				"send exactly what they tell you; never invent it.",
		},
		{
			Key: "wait", Type: "integer", Required: false,
			Description: fmt.Sprintf("How many seconds to wait for the command to finish, up to %d. It is a ceiling, "+
				"not a delay: a command that finishes sooner comes back sooner. Set it to what you are running. A "+
				"quick check needs a few seconds; a build or an install needs a hundred or more; something you expect "+
				"to stop and ask you a question needs only a few, because you want to see the question. Waiting is "+
				"how you get the whole answer in one call; calling again and again to check is not.",
				int(maxWait.Seconds())),
		},
		{
			Key: "stop", Type: "boolean", Required: false,
			Description: "Ends the command that is still running. Use it for something that will not stop on its own, " +
				"a log you were following or a program you are done answering. Send it on its own.",
		},
	}
}

// DefaultGuide is the guide the form prefills, which an administrator refines
// for their server.
func (sshTemplate) DefaultGuide() string {
	return "Send a command. Read what comes back. If it says running, decide one of three things.\n\n" +
		"Most commands finish, and that is the whole call: you get the output and the exit code.\n\n" +
		"One that does not finish comes back with running: true, the command named in running_command, and whatever " +
		"it printed so far. It is the same answer whether the command stopped to ask you something, keeps printing " +
		"(tail -f), or is a shell you opened. Three calls can follow, and the answer tells you so every time:\n\n" +
		"- it is asking something: send input with what you would type\n" +
		"- it is still working, or you are following it: call again with nothing, and get whatever is new\n" +
		"- you are done with it: call again with stop\n\n" +
		"Only one thing runs at a time, so finish or stop it before sending another command. You never have to " +
		"remember what that is: while something is running, every answer names it.\n\n" +
		"A shell you keep running is also how you keep a working directory: cd in one call and the next call is " +
		"still there.\n\n" +
		"Ask for exactly what you need. Output comes back capped, so grep, tail -n and head give you an answer while " +
		"cat on a large file gives you a truncated one you cannot trust.\n\n" +
		"The commands this tool permits are fixed by its configuration. One that is not permitted is refused, and " +
		"rewording it will not change that: report the refusal.\n\n" +
		"Add what this server is for, which services run on it, and the commands worth reaching for, so the " +
		"agent works on it competently."
}

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)

// Build validates the administrator's input and produces the tool to store: its
// name (ssh_<alias>), what it says about itself, and its call schema. The
// settings become the config JSON, secrets still plaintext for the app to seal.
func (t sshTemplate) Build(in template.Input) (template.Instance, error) {
	alias := strings.ToLower(strings.TrimSpace(in.Alias))
	if !aliasPattern.MatchString(alias) {
		return template.Instance{}, fmt.Errorf("the name must start with a letter and use only lowercase letters, numbers, and underscores")
	}
	if in.Variant != VariantSSH {
		return template.Instance{}, fmt.Errorf("unknown variant %q", in.Variant)
	}

	config, err := t.Config(in.Variant, in.Settings)
	if err != nil {
		return template.Instance{}, err
	}

	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = "Server " + alias
	}
	description := strings.TrimSpace(in.Description)
	if description == "" {
		description = fmt.Sprintf("Work on the %s server: run the commands this tool permits and read back what they printed. "+
			"Say what to do in command, and set operation to choose the kind of work. "+
			"Ask for this tool's guide (tool_guide) for the operations and the commands it allows.", alias)
	}
	guide := strings.TrimSpace(in.Guide)
	if guide == "" {
		guide = t.DefaultGuide()
	}

	return template.Instance{
		Schema: tool.Schema{
			Name:         TemplateName + "_" + alias,
			FriendlyName: display,
			Description:  description,
			InputSchema:  t.inputSchema(in.ParamDescriptions),
			Kind:         tool.KindCustom,
			// A tool that runs commands on a machine is held at the top of the
			// scale: what a command does is the command's business, not ours.
			Risk: tool.RiskDestructiveAction,
		},
		Config: config,
		Guide:  guide,
	}, nil
}

// inputSchema assembles the tool's JSON Schema from its fixed params, taking the
// administrator's description for each where one was given. The identities (key,
// type, required) are always the template's, and operation is constrained to the
// operations this build supports, so the model cannot invent one.
func (t sshTemplate) inputSchema(descriptions map[string]string) json.RawMessage {
	properties := map[string]any{}
	var required []string
	for _, p := range t.Params() {
		desc := p.Description
		if override, ok := descriptions[p.Key]; ok && strings.TrimSpace(override) != "" {
			desc = override
		}
		properties[p.Key] = map[string]any{"type": p.Type, "description": desc}
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

// Config builds the stored config from the flat form settings: dotted keys
// (auth.method, limits.idle_minutes) expand into objects, the variant is
// recorded, and the result must parse as a valid configuration, so a broken one
// is refused at create or test time rather than on first use.
func (sshTemplate) Config(variant string, settings map[string]any) (json.RawMessage, error) {
	if variant != VariantSSH {
		return nil, fmt.Errorf("unknown variant %q", variant)
	}
	raw, err := json.Marshal(nest(settings))
	if err != nil {
		return nil, fmt.Errorf("the settings could not be encoded")
	}
	// Not the strict read: an edit reaches here with its secrets blanked, meaning
	// "leave them as they were", and the app restores them straight after.
	cfg, err := parseSettings(raw)
	if err != nil {
		return nil, err
	}
	// Store what parsed, not what was typed: defaults are recorded, and anything
	// the form sent that is not a setting is dropped rather than kept forever.
	cfg.Driver = VariantSSH
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("the settings could not be encoded")
	}
	return config, nil
}

// Bind builds the live handler for a stored instance, from a config whose
// secrets the app has already opened. The handler is new each turn; the
// connection it works over is the template's and outlives it, and the sessions
// it opens are filed under the agent it was bound for.
func (t *sshTemplate) Bind(config json.RawMessage, owner tool.Owner) (tool.Handler, error) {
	cfg, err := ParseConfig(config)
	if err != nil {
		return nil, err
	}
	if truthy(cfg.ThroughChat) && t.machines == nil {
		return nil, fmt.Errorf("this installation cannot reach a server through the chat application")
	}
	h := &handler{cfg: cfg, pool: t.pool, owner: owner, sessions: t.sessions, machines: t.machines}
	return h.handle, nil
}

// Test signs in to the configured server and opens a channel on it, for the
// administrator's Test button. It runs nothing: proving the credential is
// accepted and that work can be started is the whole question.
func (sshTemplate) Test(ctx context.Context, config json.RawMessage) error {
	cfg, err := ParseConfig(config)
	if err != nil {
		return err
	}
	// A server reached through the chat application cannot be tested from here:
	// the address exists on somebody else's computer, and that person is not the
	// administrator filling in this form. The settings are still checked (they
	// parsed), and the connection is answered on the first call, in words that
	// name the reason.
	if truthy(cfg.ThroughChat) {
		return nil
	}
	client, err := dial(ctx, cfg, refuseVerification, 0)
	if err != nil {
		// A server that stops to ask for a verification code has already accepted
		// the address, proved itself with the host key, and taken the configured
		// credential far enough to ask for more. There is nobody to ask here, and
		// nothing further this can check, so it reports the settings as sound.
		if errors.Is(err, errVerificationNeeded) {
			return nil
		}
		return err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("the server would not open a session: %w", err)
	}
	// Opening it was the question. Closing a channel the server has already torn
	// down reports an end-of-file that says nothing about the configuration.
	_ = session.Close()
	return nil
}

// Documentation is what the model knows about this tool however an
// administrator edits the form: which machine it works on, and the drill-down
// topics for how the server is shared.
func (sshTemplate) Documentation(config json.RawMessage) template.Documentation {
	cfg, err := parseSettings(config)
	if err != nil {
		return template.Documentation{}
	}
	note := fmt.Sprintf("This tool works on the server %s as %s. The address, the sign-in and the permitted "+
		"commands are configured on the tool and cannot be changed by a call, so never ask for or supply a credential.",
		cfg.Host, cfg.Username)

	topics := []tool.Topic{
		{
			ID:    "connection",
			Title: "How this server is reached, and what that means for you",
			Body: fmt.Sprintf("Work runs over one shared connection to %s, with at most %d operations in flight at a time. "+
				"When they are all busy your call comes back saying so; wait a moment and try again rather than giving up. "+
				"The connection closes after %d minutes with nothing running and is reopened on the next call, which you never "+
				"have to manage.", cfg.Host, cfg.Limits.MaxChannels, cfg.Limits.IdleMinutes),
			Edges: []tool.TopicEdge{{To: "limits", Type: tool.EdgeCompanion, When: "before running something slow"}},
		},
		{
			ID:    "limits",
			Title: "Waiting: the one number you set",
			Body: fmt.Sprintf("A call ends on one of two things. The command finished, and you get its output and its "+
				"exit code. Or your wait ran out, and you get what it printed so far, marked running: nothing was "+
				"stopped, it is still going on the server.\n\nSo set wait to what you are running. A status check "+
				"needs a few seconds. An install or a build needs a hundred or more, up to %d. Something you expect "+
				"to stop and ask you needs only a few, because what you want is to see the question.\n\nWaiting is "+
				"how you get the whole answer in one call. Calling again and again to see whether it is done is the "+
				"wrong instinct here: every call is a round trip, and sitting on the open channel costs nothing.\n\n"+
				"Left with no wait at all, a call waits %d seconds.\n\nA command left running holds one of the "+
				"connection's channels, so send stop when you are done with it. One nobody touches is closed for you "+
				"after a while, and one open too long is closed whatever is happening in it.",
				int(maxWait.Seconds()), int(defaultWait.Seconds())),
			Edges: []tool.TopicEdge{{To: "connection", Type: tool.EdgeCompanion, When: "to recall how the server is shared"}},
		},
		commandsTopic(cfg.Policy),
		interactiveTopic(cfg.Policy),
		{
			ID:    "working",
			Title: "Working on this server well",
			Body: "Each command starts fresh: no working directory, no exported variable, no activated environment " +
				"carries from one command to the next. Either say it all in one command (cd /opt/app && ./deploy.sh), or run " +
				"a shell and keep it running, which does keep all of that for as long as you leave it open.\n\n" +
				"Ask the server to narrow the answer rather than narrowing it yourself. grep, tail -n, head, " +
				"systemctl --no-pager, journalctl -n: all of them beat reading something whole and reading past it. " +
				"What comes back is capped, and a result marked output_dropped is missing the part you have not seen, " +
				"so never conclude from one.\n\n" +
				"A command that exits non-zero has answered you. Read exit_code and the output and say what they " +
				"mean; it is not a failure of the tool and it is not something to run again unchanged.\n\n" +
				"A refusal is final. The permitted commands are configuration, so a command that comes back refused " +
				"will be refused however it is reworded, piped, or wrapped. Report it and offer what you can do " +
				"instead.\n\n" +
				"Prefer one good command to several small ones: each call is a round trip and, where this tool asks " +
				"for approval, a question for the person.",
			Edges: []tool.TopicEdge{
				{To: "limits", Type: tool.EdgeCompanion, When: "before running something slow or noisy"},
				{To: "interactive", Type: tool.EdgeAlternative, When: "when a command does not finish, or state has to stick"},
			},
		},
		{
			ID:    "prompts",
			Title: "Commands that stop and ask a question",
			Body: "Nothing here has to be predicted. Run the command. If it stops and asks something, the call comes " +
				"back running with the question it printed, and you answer it with input on the next call. A package " +
				"manager wanting a yes, a tool asking to confirm, a program wanting a passphrase: all the same.\n\n" +
				"That said, prefer the form of the command that does not ask: -y or --yes for a package manager, " +
				"--non-interactive, --force where it genuinely is what you want. One command that finishes is better " +
				"than three calls that get there, and it leaves nothing running.\n\n" +
				"Answer what you can answer yourself. When it asks for something only the person has, a password, a " +
				"licence key, a code, ask them in the conversation and send exactly what they tell you. Never invent " +
				"it, and never send a credential you were not given.",
			Edges: []tool.TopicEdge{
				{To: "interactive", Type: tool.EdgeCompanion, When: "for what running means and what to do about it"},
			},
		},
		{
			ID:    "verification",
			Title: "When the server asks for a verification code",
			Body: "Some servers want a code as well as a credential before they will accept a connection. When that " +
				"happens your call comes back with verification_required and the server's own question. Nothing has " +
				"gone wrong and nothing was run: the server is holding the connection open, waiting.\n\n" +
				"Put the question to the person in your own words and wait for their answer. Then make the same call " +
				"again with the same command, adding input set to the code as they read it out. That call signs in " +
				"and does the work in one go.\n\n" +
				"Only the person has the code. Never invent one, never reuse an old one, and never ask for a " +
				"password or any other credential: everything else the server needs is already configured. A code " +
				"given too late is refused, and sending the command again on its own starts a fresh question.\n\n" +
				"Because the connection is shared, this is asked once for a connection, not once per command: work " +
				"that follows on the same connection just runs.",
			Edges: []tool.TopicEdge{{To: "connection", Type: tool.EdgeCompanion, When: "to recall that the connection is shared and reused"}},
		},
	}
	return template.Documentation{Note: note, Topics: topics}
}

// nest expands the form's dotted keys (auth.method, limits.idle_minutes) into
// the nested object the config is stored as.
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

// commandsTopic tells the model what this server permits, in the same words the
// administrator wrote. Knowing the list up front is the difference between an
// assistant that reaches for a permitted command and one that proposes something
// that will be refused.
func commandsTopic(policy cmdpolicy.Policy) tool.Topic {
	var body string
	switch policy.Mode {
	case cmdpolicy.PolicyAllowlist:
		body = "Only these commands may run here, and nothing else:\n\n" + list(policy.AllowedEntries()) +
			"\n\nA line naming a program permits it with any arguments; a longer line permits only that exact opening. " +
			"Anything not on the list is refused, and rewording it will not help."
	default:
		body = "Most commands may run here, but these are refused:\n\n" + list(policy.DeniedEntries()) +
			"\n\nRunning something as another user is " + map[string]string{cmdpolicy.SudoAllow: "permitted", cmdpolicy.SudoDeny: "refused"}[policy.Sudo] + "."
	}
	body += "\n\nA command is read the way the server would read it, so a refused command cannot be hidden inside a " +
		"pipeline or behind a semicolon. A command whose words are built on the server (from a variable, or from " +
		"another command's output) cannot be checked and is refused: write it out in full."
	return tool.Topic{
		ID:    "commands",
		Title: "Which commands this server permits",
		Body:  body,
		Edges: []tool.TopicEdge{{To: "limits", Type: tool.EdgeCompanion, When: "before running something slow"}},
	}
}

// interactiveTopic teaches the one thing the schema cannot say on its own: what
// a command that has not finished is, and that all three kinds of it are the
// same thing to answer.
func interactiveTopic(policy cmdpolicy.Policy) tool.Topic {
	body := "A command that comes back marked running has not finished. Nothing went wrong: it is still there on the " +
		"server, and you decide what happens next. There are three reasons for it, and they are answered the same " +
		"way.\n\n" +
		"FIRST, it stopped to ask you something. passwd wants the new password twice; an installer wants a choice. " +
		"Read what it printed, and send input with what you would have typed.\n\n" +
		"SECOND, it keeps printing, which is how you follow something as it happens: tail -f " +
		"/var/log/application.log, journalctl -u application -f --no-pager, a build or a migration you want to " +
		"watch. Call again with nothing and you get whatever it has printed since you last looked. That call comes " +
		"back the moment there is something new, so watching this way is how to follow a log; running the same " +
		"command over and over is not.\n\n" +
		"THIRD, and this is the one worth remembering: run a shell (bash) and it keeps running, which means it KEEPS " +
		"ITS STATE. cd, an exported variable, an activated environment all stick for as long as you leave it open, " +
		"and each input is a command in that same place. Work that takes several steps in one directory belongs in " +
		"one shell rather than in commands that each start over.\n\n" +
		"Send stop when you are done with any of them. It holds one of the connection's channels until you do, and " +
		"there are only a few.\n\n" +
		"There is nothing to keep track of and nothing to name: you have one command running on this server at a " +
		"time, and input, reading, and stop all mean that one.\n\n" +
		"It is not the way to read or edit a file, and not the way to run SQL. Reading is a command; SQL is a " +
		"command or another tool.\n\n"

	body += "What may be run this way is what may be run at all: the same permitted commands, checked the same way, " +
		"including what you type into a shell. Being able to answer a program's questions is a way of running it, " +
		"not a separate permission."
	return tool.Topic{
		ID:    "interactive",
		Title: "When a command has not finished",
		Body:  body,
		Edges: []tool.TopicEdge{
			{To: "prompts", Type: tool.EdgeAlternative, When: "when the program has a flag that stops it asking, which is nearly always better"},
			{To: "commands", Type: tool.EdgePrerequisite, When: "to see what this tool permits"},
		},
	}
}

func list(entries [][]string) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, "- "+strings.Join(entry, " "))
	}
	if len(lines) == 0 {
		return "- (none)"
	}
	return strings.Join(lines, "\n")
}
