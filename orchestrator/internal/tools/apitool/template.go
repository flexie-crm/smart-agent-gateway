package apitool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/useragent"
)

// TemplateName is the template's id and the prefix of every tool it makes:
// api_billing, api_warehouse.
const TemplateName = "api"

type apiTemplate struct {
	client *http.Client
	// tokens is the process's access-token cache, shared by every tool using
	// the client-credentials grant. On the TEMPLATE because it is process
	// state: two tools with the same credentials are the same client asking
	// the same authorization server, and should not each hold a token.
	tokens *tokens
	// machines is how a call reaches an API this server cannot (KB/39).
	machines template.Machines
}

// New makes the template. The client is shared, so connections are pooled
// across every API tool in the process, which is what a pool is for.
//
// machines is the chat applications connected right now, for an API that
// exists only on somebody's own network. Nil on an installation that does not
// offer that, and a tool configured to use it is then refused at Bind rather
// than at its first call.
func New(machines template.Machines) template.Template {
	client := newClient()
	return apiTemplate{client: client, tokens: newTokens(client), machines: machines}
}

func (apiTemplate) Name() string  { return TemplateName }
func (apiTemplate) Title() string { return "HTTP API" }
func (apiTemplate) Description() string {
	return "Connect the assistant to one API you already use, which is how most integrations are " +
		"built. Its credentials are stored encrypted on the tool and attached to every call, so the " +
		"assistant uses the service without ever seeing them, and they are removed from anything it " +
		"reports back. It can reach only the address you give it, using only the verbs you allow."
}

// Variants are the ways an API is proved to, because each needs different
// fields on the form. Every one here is STATIC: a value an administrator types
// once that does not change by itself. OAuth is a later phase, and the reason
// it is not simply another variant is that its token rotates, which needs
// somewhere to write it back to.
func (apiTemplate) Variants() []template.Variant {
	return []template.Variant{
		{Key: AuthBearer, Label: "Bearer token"},
		{Key: AuthAPIKey, Label: "API key"},
		{Key: AuthBasic, Label: "Username and password"},
		{Key: AuthClientCredentials, Label: "OAuth2, acting as the application"},
		{Key: AuthAuthorizationCode, Label: "OAuth2, acting as a person who signs in"},
		{Key: AuthJWTBearer, Label: "JWT"},
		{Key: AuthNone, Label: "None (a public API)"},
	}
}

func (apiTemplate) Fields(variant string) ([]template.Section, error) {
	address := template.Section{
		Title: "The API",
		Hint: "The address every call is made under. A path the assistant asks for is resolved UNDER this and " +
			"cannot leave it, so this is what bounds the tool: point it at one API, not at a whole host you " +
			"also keep other things on.\n\n" +
			"Keep the API's own root in it if it has one (https://api.example.com/v1), and the assistant then " +
			"asks for /invoices rather than /v1/invoices.",
		Fields: []template.Field{
			{Key: "base_url", Label: "Base address", Type: template.FieldText, Required: true, Span: 4,
				Help: "https://api.example.com/v1"},
			{Key: "timeout_seconds", Label: "Timeout (seconds)", Type: template.FieldNumber, Span: 2,
				Default: "30", Help: "How long one call may take before it is given up on."},
			// NOT secret, and that is a decision rather than an oversight.
			//
			// These are a version, an account id, a tenant: the ordinary
			// furniture of an API call, and there is a place for a credential
			// already, on the Authentication tab. Marking them secret cost
			// more than it bought: the form BLANKS a secret, so an
			// administrator with a header configured could not see it, could
			// not amend it without retyping it, and reported it as having been
			// removed. Sealing them bought nothing either, since what they
			// hold is not a secret to begin with.
			{
				Key: "extra_headers", Label: "Additional parameters in headers", Type: template.FieldPairs, Span: 6,
				Help: "Sent as headers on every call: a version, an account id, a tenant. Not for a key or a token: " +
					"those go on the Authentication tab, where they are encrypted and hidden from the assistant.",
			},
			{
				Key: "extra_query", Label: "Additional parameters in the query string", Type: template.FieldPairs, Span: 6,
				Help: "Sent in the query string on every call.",
			},
			// The same checkbox the database and server tools offer, because it
			// is the same situation: an API that exists only on an office
			// network, and a chat application installed on a machine that can
			// see it.
			template.ReachChatField("API"),
		},
	}

	auth, err := authSection(variant)
	if err != nil {
		return nil, err
	}

	policy := template.Section{
		Title: "Policy",
		Hint: "Which HTTP verbs this tool may use. The verb is the whole rule, deliberately: what a path means " +
			"is the API's own business and cannot be known from here, while what a verb DOES is the same " +
			"everywhere, and the line worth drawing is between reading and changing.\n\n" +
			"- Denylist uses anything except what you list. It starts with the four that change things, which " +
			"makes an assistant that can read the API without altering it.\n" +
			"- Allowlist is stronger: only what you list is used.\n" +
			"- One verb per line, from GET, HEAD, OPTIONS, POST, PUT, PATCH, DELETE.\n\n" +
			"Whether a call stops to ask a person is the agent's setting, not this one.",
		Fields: []template.Field{
			{
				Key: "policy.mode", Label: "Rule", Type: template.FieldSelect, Required: true, Span: 6,
				Options: []template.Option{
					template.Choice(Denylist, "Every verb except the ones I list"),
					template.Choice(Allowlist, "Only the verbs I list"),
				},
				Default: Denylist,
			},
			{
				Key: "policy.verbs", Label: "Verbs", Type: template.FieldTextarea, Span: 6,
				Default: strings.Join(DefaultDeniedVerbs(), "\n"),
				Help:    "One per line.",
			},
		},
	}
	return []template.Section{address, auth, policy}, nil
}

// authSection is the one part of the form that changes with the variant.
func authSection(variant string) (template.Section, error) {
	switch variant {
	case AuthBearer:
		return template.Section{
			Title:  "Authentication",
			Hint:   "Sent as `Authorization: Bearer <token>`, which is what most modern APIs expect.",
			Fields: []template.Field{{Key: "auth.token", Label: "Token", Type: template.FieldPassword, Required: true, Secret: true, Span: 6}},
		}, nil
	case AuthBasic:
		return template.Section{
			Title: "Authentication",
			Hint:  "Sent as `Authorization: Basic`, with the two joined and encoded the way the standard says.",
			Fields: []template.Field{
				{Key: "auth.user", Label: "Username", Type: template.FieldText, Required: true, Span: 3},
				{Key: "auth.password", Label: "Password", Type: template.FieldPassword, Secret: true, Span: 3},
			},
		}, nil
	case AuthAPIKey:
		return template.Section{
			Title: "Authentication",
			Hint: "An API key is not one thing, so say where this service wants it. X-API-Key in a header, " +
				"`Authorization: Token abc`, and ?api_key=abc are all in use, and a tool that guessed would " +
				"work on some services and not others.",
			Fields: []template.Field{
				{Key: "auth.key", Label: "Key", Type: template.FieldPassword, Required: true, Secret: true, Span: 6},
				{
					Key: "auth.placement", Label: "Sent in", Type: template.FieldSelect, Required: true, Span: 2,
					Options: []template.Option{
						template.Choice(InHeader, "A header"),
						template.Choice(InQuery, "The query string"),
					},
					Default: InHeader,
				},
				{Key: "auth.name", Label: "Under the name", Type: template.FieldText, Required: true, Span: 4,
					Default: "X-API-Key",
					Help: "X-API-Key, Authorization, api_key. The key is sent exactly as you typed it, so " +
						"a service wanting \"Token abc\" takes that as the key."},
			},
		}, nil
	case AuthClientCredentials:
		return template.Section{
			Title: "Authentication",
			Hint: "The tool holds its own access to the service. It sends the client ID and secret, " +
				"gets an access token back, and asks for another when that one expires. Nobody signs " +
				"in, and the access is the same for everyone who uses the tool.",
			Fields: []template.Field{
				{
					Key: "callback", Label: "Callback URL", Type: template.FieldCallback, Span: 6,
					Help: "Copy this into the application you create at the service. It is matched " +
						"character for character, so paste it rather than typing it.",
				},
				{Key: "auth.token_url", Label: "Token URL", Type: template.FieldText, Required: true, Span: 6,
					Help: "https://login.example.com/oauth2/token"},
				{Key: "auth.client_id", Label: "Client ID", Type: template.FieldText, Required: true, Span: 3},
				{Key: "auth.client_secret", Label: "Client secret", Type: template.FieldPassword, Required: true, Secret: true, Span: 3},
				{Key: "auth.scope", Label: "Scope", Type: template.FieldText, Span: 4,
					Help: "Space separated, if the service wants them. Leave empty otherwise."},
				{
					Key: "auth.credentials", Label: "Client sent in", Type: template.FieldSelect, Span: 2,
					Options: []template.Option{
						template.Choice(CredentialsInHeader, "The Authorization header"),
						template.Choice(CredentialsInBody, "The request body"),
					},
					Default: CredentialsInHeader,
				},
			},
		}, nil
	case AuthAuthorizationCode:
		return template.Section{
			Title: "Authentication",
			Hint: "A person signs in at the service and approves access, and the assistant then acts " +
				"as them: it sees exactly what they can see, and the service's audit log carries " +
				"their name. The approval is given once and kept.",
			Fields: []template.Field{
				{
					Key: "callback", Label: "Callback URL", Type: template.FieldCallback, Span: 6,
					Help: "Copy this into the application you create at the service. It is matched " +
						"character for character, so paste it rather than typing it.",
				},
				{Key: "auth.client_id", Label: "Client ID", Type: template.FieldText, Required: true, Span: 3},
				{Key: "auth.client_secret", Label: "Client secret", Type: template.FieldPassword, Secret: true, Span: 3},
				{Key: "auth.scope", Label: "Scope", Type: template.FieldText, Span: 4,
					Help: "Space separated, if the service wants them. Leave empty otherwise."},
				{
					Key: "auth.credentials", Label: "Client sent in", Type: template.FieldSelect, Span: 2,
					Options: []template.Option{
						template.Choice(CredentialsInHeader, "The Authorization header"),
						template.Choice(CredentialsInBody, "The request body"),
					},
					Default: CredentialsInHeader,
					Help:    "Change this only if the service's documentation says so.",
				},
				{Key: "auth.authorize_url", Label: "Sign-in URL", Type: template.FieldText, Span: 3,
					Help: "https://example.com/oauth2/authorize"},
				{Key: "auth.token_url", Label: "Token URL", Type: template.FieldText, Span: 3,
					Help: "https://example.com/oauth2/token"},
				{
					Key: "auth.authorize_params", Label: "Extra sign-in parameters",
					Type: template.FieldPairs, Span: 6,
					Help: "Added to the sign-in URL. Whatever the service requires beyond the " +
						"protocol: access_type = offline for Google, which issues no renewable " +
						"access without it; audience for Auth0; prompt where a service wants one.",
				},
			},
		}, nil
	case AuthJWTBearer:
		return template.Section{
			Title: "Authentication",
			Hint: "The service gives you a key and a secret. The tool generates a token from them, " +
				"signs it with the secret, and sends it with every call, generating another before " +
				"that one expires. The secret itself is never sent.",
			Fields: []template.Field{
				// The four claims the format defines, each on the form under
				// the name its documentation uses, because that is what
				// somebody is reading while they fill this in.
				{Key: "auth.issuer", Label: "Issuer (iss)", Type: template.FieldText, Required: true, Span: 6,
					Help: "The key the service issued."},
				{
					Key: "auth.private_key", Label: "Secret", Type: template.FieldTextarea,
					Required: true, Secret: true, Span: 6,
					Help: "The secret the service issued. For RS256 or ES256 it is a PEM private " +
						"key instead, pasted whole.",
				},
				{Key: "auth.subject", Label: "Subject (sub)", Type: template.FieldText, Span: 3,
					Help: "Who the token is about. Only where the service asks for one."},
				{Key: "auth.audience", Label: "Audience (aud)", Type: template.FieldText, Span: 3,
					Help: "Who the token is for. Only where the service asks for one."},
				{Key: "auth.lifetime_minutes", Label: "Expires after (exp), minutes",
					Type: template.FieldNumber, Span: 2, Default: "30",
					Help: "Services cap this, often at 60."},
				{
					Key: "auth.algorithm", Label: "Signed with", Type: template.FieldSelect, Span: 2,
					Options: []template.Option{
						template.Choice(AlgHS256, "HS256"),
						template.Choice(AlgHS384, "HS384"),
						template.Choice(AlgHS512, "HS512"),
						template.Choice(AlgRS256, "RS256"),
						template.Choice(AlgES256, "ES256"),
					},
					Default: AlgHS256,
				},
				{Key: "auth.name", Label: "Sent under the name", Type: template.FieldText, Span: 2,
					Default: HeaderAuthorization,
					Help:    "The header the token travels in."},
				{Key: "auth.scope", Label: "Scope", Type: template.FieldText, Span: 6,
					Help: "Only where the service wants one, carried inside the token."},
				{
					Key: "auth.claims", Label: "Extra claims", Type: template.FieldPairs, Span: 6,
					Help: "Anything a service asks for beyond the four above.",
				},
			},
		}, nil
	case AuthNone:
		return template.Section{
			Title:  "Authentication",
			Hint:   "None: every call goes out unauthenticated. Right for a public API and nothing else.",
			Fields: nil,
		}, nil
	default:
		return template.Section{}, fmt.Errorf("unknown authentication %q", variant)
	}
}

// SecretPaths are what the app seals at rest and blanks when the form is
// reopened. Listed per variant so a field that is not on the form is not
// claimed to be a secret of it.
func (apiTemplate) SecretPaths(variant string) []string {
	// THE CREDENTIAL, and nothing else.
	//
	// The API's own extras used to be in here too, on the reasoning that one of
	// them was "as likely to be a shared secret as the key is". That was wrong
	// twice. They are a version, an account id, a tenant, and there is a place
	// for a credential already, one tab away. And sealing them made them
	// unreadable to the person who typed them: the form blanks a secret, so a
	// configured header could not be seen or amended without retyping it, and
	// it read as having been deleted.
	//
	// A value sealed under the old rule still OPENS, because opening walks the
	// whole config on the marker rather than following this list. It is stored
	// in the clear the next time the tool is saved.
	switch variant {
	case AuthBearer:
		return []string{"auth.token"}
	case AuthBasic:
		return []string{"auth.password"}
	case AuthAPIKey:
		return []string{"auth.key"}
	case AuthClientCredentials, AuthAuthorizationCode:
		return []string{"auth.client_secret"}
	case AuthJWTBearer:
		// The key is the whole credential for this grant, and the only one:
		// nothing else here is secret, because nothing else is shared.
		return []string{"auth.private_key"}
	default:
		return nil
	}
}

// Params are what the model sends. FIXED, and small on purpose: the API's own
// operations are not here and never will be. A specification is hundreds of
// operations, and an enum of those would be sent with every turn of every
// conversation whether or not this tool was touched. The endpoints belong in a
// skill, which is searched and read on demand.
func (apiTemplate) Params() []template.Param {
	return []template.Param{
		{
			Key: "method", Type: "string", Required: true,
			Description: "The HTTP verb: GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS. Only the verbs this tool's policy permits will run.",
		},
		{
			Key: "path", Type: "string", Required: false,
			Description: "The path under the API's base address, for example /invoices or /invoices/in_123/lines. " +
				"Leave it out or send an empty string when the base address is the whole endpoint, which is how a " +
				"single webhook or listener address is configured. A whole address is refused: this tool reaches " +
				"one API and nothing else.",
		},
		{
			Key: "query", Type: "object", Required: false,
			Description: "Query string parameters as a flat object, including the API's own paging parameters. " +
				"Values may be strings, numbers or booleans, or a LIST where the API takes more than one of " +
				"something: {\"id\": [1, 2]} is sent as id=1&id=2. Parameters the tool is configured with are " +
				"added for you and cannot be overridden here.",
		},
		{
			Key: "body", Type: "object", Required: false,
			Description: "The request body. For POST, PUT and PATCH. An object by default, sent as JSON; " +
				"for the other encodings see body_type.",
		},
		{
			Key: "body_type", Type: "string", Required: false,
			Description: "How to encode the body, when the endpoint does not take JSON: " +
				"`form` sends an object as application/x-www-form-urlencoded (which many token and webhook " +
				"routes require), `xml` and `text` send a single string as its own characters. " +
				"Omit it for JSON, which is the default.",
		},
	}
}

func (apiTemplate) DefaultDescription() string {
	return "Make requests to this API: you give the HTTP verb and the path, and its credentials " +
		"are attached for you. Read this tool's guide before the first call, because which paths " +
		"exist is documented there."
}

func (apiTemplate) DefaultGuide() string {
	return strings.TrimSpace(`
This tool is one API, already authenticated. You supply a verb, a path under its
base address, and parameters; the credentials are held by the tool and are never
shown to you.

THE PATH MAY BE LEFT OUT, and for some tools it must be. Where the base address
is a single endpoint rather than an API with many paths (a webhook or a listener
address is the usual case), there is nothing to append: send no path, or an
empty one, and the request goes to the base address exactly as configured.
Sending "/" instead is a DIFFERENT address and such a service will not answer
it.

HOW THE BODY IS ENCODED is the endpoint's business, not this tool's. JSON is the
default and is what most take. Where one wants a form, say body_type "form" and
pass an object; where it wants a document, say "xml" or "text" and pass a single
string. One API commonly takes JSON on its resources and a form on its token
route, which is why this is per call.

MORE THAN ONE OF SOMETHING is a list, in the query and in a form body alike:
{"id": [1, 2]} is sent as id=1&id=2. A list is never sent as text with brackets
in it.

WHAT THE TOOL ADDS ITSELF you cannot change and do not need to: its credential,
and any headers and query parameters it is configured with. They are applied
after everything you ask for, so naming one of them has no effect. The call
reports every header it sent, with the configured values shown as
"****secret****": that is a value you are not given, and asking for it another
way will not produce it.

Never put a key, a token or a password in anything you send. The tool attaches
its own, and a credential you invent will only be wrong.

WHICH PATHS EXIST IS NOT IN THIS GUIDE. An API has far too many endpoints to
recite here. If this guide ends with a list of written procedures, that is the
specification somebody wrote for this API: open one and read it before guessing
at a path. If it lists none, try the path the API's own conventions suggest and
read the answer, because a path that does not exist comes back as the API's own
404, which is an answer and not a fault.

WHAT THIS TOOL MAY DO is decided per verb by an administrator, and a verb that
is not permitted is refused here before anything is sent. That is somebody's
decision, not a fault and not an obstacle to route around: do not retry it, and
do not go looking for a different path that would have the same effect. Say
plainly what you were not able to do.

Reading a response:
- status and ok say what happened. A 4xx or 5xx is the API answering, not the
  tool breaking: read the body, it usually says which field was wrong.
- 429 means slow down; the Retry-After and rate-limit headers come back for
  exactly that.
- Paging is the API's own: pass its paging parameters in query, and follow the
  Link header or the next-page field the API gives you.
- A response longer than the ceiling is marked truncated. Ask for less rather
  than for the same thing again.`)
}

var aliasOK = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)

func (t apiTemplate) Config(variant string, settings map[string]any) (json.RawMessage, error) {
	cfg := nest(settings)
	// The form does not own the grant, and this is what makes that true rather
	// than merely intended: nest() builds whatever keys it is handed, so a
	// request carrying "grant.access_token" would write a sign-in nobody gave.
	// Deleted here, where every path into a config passes, instead of trusted
	// not to arrive.
	delete(cfg, GrantPath)
	auth, _ := cfg["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
		cfg["auth"] = auth
	}
	auth["kind"] = variant
	// And where the app recovers it when the form is reopened, which is a
	// top-level "driver" (see Settings.Driver). Written from the same value as
	// the kind above, one line apart, so the two cannot disagree.
	cfg["driver"] = variant
	// The textarea is a list by the time it is stored, so the rule is one shape
	// wherever it is read.
	if policy, ok := cfg["policy"].(map[string]any); ok {
		policy["verbs"] = lines(policy["verbs"])
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("the settings could not be encoded")
	}
	if _, err := Parse(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (t apiTemplate) Build(in template.Input) (template.Instance, error) {
	alias := strings.TrimSpace(in.Alias)
	if !aliasOK.MatchString(alias) {
		return template.Instance{}, fmt.Errorf("the name must start with a letter and use only lowercase letters, numbers and underscores")
	}
	config, err := t.Config(in.Variant, in.Settings)
	if err != nil {
		return template.Instance{}, err
	}
	settings, err := Parse(config)
	if err != nil {
		return template.Instance{}, err
	}

	name := TemplateName + "_" + alias
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = "Call " + alias
	}
	description := strings.TrimSpace(in.Description)
	if description == "" {
		description = fmt.Sprintf("Make requests to the %s API at %s.", alias, settings.BaseURL)
	}

	return template.Instance{
		Schema: tool.Schema{
			Name:         name,
			FriendlyName: display,
			Description:  description,
			InputSchema:  template.InputSchema(t.Params(), in.ParamDescriptions),
			Kind:         tool.KindCustom,
			// The same level as http_request and the browser, and for the same
			// reason: what this tool IS is a thing that reaches a place outside
			// the workspace. It is not raised when writes are permitted,
			// because "this deletes something and cannot be undone" is a claim
			// about a remote service we are not entitled to make, and the
			// approval card is keyed to this.
			Risk: tool.RiskExternalCommunication,
		},
		Config: config,
		Guide:  strings.TrimSpace(in.Guide),
	}, nil
}

func (t apiTemplate) Bind(config json.RawMessage, _ tool.Owner) (tool.Handler, error) {
	settings, err := Parse(config)
	if err != nil {
		return nil, err
	}
	if err := t.canReach(settings); err != nil {
		return nil, err
	}
	// The owner is taken and unused: this tool keeps nothing between calls that
	// belongs to one agent. A sign-in is the TOOL's, not an agent's, and comes
	// through BindGranted below.
	return Handler(settings, t.client, t.tokens, nil, t.machines), nil
}

// canReach refuses at BIND a tool asking for something this installation does
// not have, rather than at its first call.
//
// The difference matters: a tool refused here is absent from the loadout and
// the model never sees it, where one that fails on its first call is a tool the
// model believes in and keeps trying.
func (t apiTemplate) canReach(settings Settings) error {
	if settings.ThroughChat && t.machines == nil {
		return fmt.Errorf("this installation cannot reach an API through the chat application")
	}
	return nil
}

// BindGranted is the same handler with a way to renew a person's sign-in.
//
// Separate from Bind because only one variant has one, and because renewing is
// the app's: this template knows how to attach a bearer token, and getting a
// fresh one needs the store, the keyring and a conversation with an
// authorization server. A grant is handed in rather than reached for.
func (t apiTemplate) BindGranted(config json.RawMessage, _ tool.Owner, grant template.Grant) (tool.Handler, error) {
	settings, err := Parse(config)
	if err != nil {
		return nil, err
	}
	if err := t.canReach(settings); err != nil {
		return nil, err
	}
	return Handler(settings, t.client, t.tokens, grant, t.machines), nil
}

// Connectable says this tool is the kind a person signs in to.
//
// True only for the authorization-code grant. A key is typed and client
// credentials prove the tool itself; neither has anybody to send anywhere, and
// a Connect button beside them would be a button that does nothing.
func (apiTemplate) Connectable(config json.RawMessage) bool {
	settings, err := Parse(config)
	if err != nil {
		return false
	}
	return settings.Auth.Kind == AuthAuthorizationCode
}

// Test proves the configuration before it is saved, so approval equals success.
//
// It asks the API a question the policy always permits and that changes
// nothing, and reads the ANSWER rather than requiring a 200: a 401 means the
// credential is wrong, which is the thing worth catching, while a 404 means the
// address is reachable and authenticated and simply has nothing at its root,
// which is true of most APIs and is not a fault.
func (t apiTemplate) Test(ctx context.Context, config json.RawMessage) error {
	settings, err := Parse(config)
	if err != nil {
		return err
	}
	// A tool that signs a PERSON in has no token until somebody has signed in,
	// so testing it cannot mean making an authenticated call. What can be
	// checked is what an administrator just typed: that the addresses parse
	// (Parse above did that) and that the service answers at all. Once it IS
	// connected there is a token, and the ordinary call below proves it.
	if settings.Auth.Kind == AuthAuthorizationCode && !settings.Grant.Held() {
		return t.reachable(ctx, settings)
	}

	verb := "GET"
	if ok, _ := settings.Policy.Permits(verb); !ok {
		if ok, _ := settings.Policy.Permits("HEAD"); !ok {
			return fmt.Errorf("this policy permits neither GET nor HEAD, so the connection cannot be tested")
		}
		verb = "HEAD"
	}
	res, err := Handler(settings, t.client, t.tokens, heldGrant(settings.Grant), t.machines)(ctx, tool.Call{
		Args: json.RawMessage(`{"method":"` + verb + `","path":"/"}`),
	})
	if err != nil {
		return err
	}
	if res.Failed() {
		return fmt.Errorf("%s", firstLine(res.Content))
	}
	var out struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(res.Content, &out)
	switch out.Status {
	case 401, 403:
		return fmt.Errorf("the API answered %d, which means it did not accept these credentials", out.Status)
	case 0:
		return fmt.Errorf("the API did not answer")
	}
	return nil
}

// reachable asks the base address whether anything is there, with no
// credentials at all.
//
// ANY answer passes, a 401 included: without a sign-in that is the correct
// answer and the most likely one, and treating it as a failure would mean an
// administrator could never save a tool before connecting it. What this catches
// is the address being wrong, which is the mistake worth catching at this
// moment, because the alternative is finding out halfway through a browser
// redirect.
func (t apiTemplate) reachable(ctx context.Context, settings Settings) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, settings.BaseURL, nil)
	if err != nil {
		return fmt.Errorf("that address could not be used: %w", err)
	}
	useragent.Set(req.Header)
	res, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("that address could not be reached: %w", err)
	}
	_ = res.Body.Close()
	return nil
}

// heldGrant is the sign-in a tool already has, as a grant that cannot renew
// itself. Nil when there is none, so a handler built from it behaves exactly as
// one built for a tool nobody has connected.
//
// It exists for Test, which is the one caller with a config and no app behind
// it. Testing a connected tool has to make a REAL authenticated call, because
// that is the only thing that proves the sign-in works; renewing on the way is
// the running tool's job and needs the store and the keyring, neither of which
// a template has.
func heldGrant(g Grant) template.Grant {
	if !g.Held() {
		return nil
	}
	return storedToken(g.AccessToken)
}

type storedToken string

// Token answers with what is stored and never renews. A token that has expired
// since it was kept comes back as the service's own 401, which is the right
// answer to "do these settings work": it does not, and reconnecting is what
// fixes it.
func (s storedToken) Token(context.Context) (string, error) { return string(s), nil }

// Renew offers nothing better, for the same reason: a test proves the stored
// sign-in as it is.
func (s storedToken) Renew(context.Context, string) (string, error) { return string(s), nil }

// Documentation is the part of the tool's description the form cannot remove.
func (apiTemplate) Documentation(config json.RawMessage) template.Documentation {
	settings, err := Parse(config)
	if err != nil {
		return template.Documentation{}
	}
	note := fmt.Sprintf(" Calls go to %s. ", settings.BaseURL)
	switch settings.Policy.Mode {
	case Allowlist:
		note += "It may only use " + strings.Join(settings.Policy.Verbs, ", ") + "."
	default:
		if len(settings.Policy.Verbs) == 0 {
			note += "It may use any HTTP verb."
		} else {
			note += "It may not use " + strings.Join(settings.Policy.Verbs, ", ") + "."
		}
	}
	return template.Documentation{Note: note}
}

// Display is what a person sees when they open one of these calls: the request
// they made, and what came back. The credential is in neither.
//
// The BODY and the QUERY are here because they were missing, and their absence
// read as a defect in the tool: a call that posted an invoice showed the verb
// and the path and nothing else, so the one thing somebody debugging needs to
// see was the one thing not shown. They are the model's OWN arguments, which
// is what makes them safe to show: `Sent` is read off the arguments and a
// credential is never in them (the key is attached to the request afterwards,
// and a key placed in the query goes on the URL, not on this).
func (apiTemplate) Display() tool.Display {
	return tool.Display{
		Where: []string{"request_url"},
		Sent: []tool.Shown{
			tool.Text("method"), tool.Text("path"),
			tool.Payload("query"), tool.Payload("body"),
		},
		// What the tool put on the call itself. The values that came from the
		// configuration are masked and their names are not, so an
		// administrator can see that their own additional headers went.
		Attached: []tool.Shown{tool.Payload("request_headers")},
		Answered: []tool.Shown{tool.Text("status"), tool.Body("body", "content_type")},
	}
}

// nest expands dotted form keys (auth.token, policy.mode) into the nested
// object the config is read as. The same helper the query template has, and
// for the same reason: the form is flat and the config is not.
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

// lines turns a textarea into a list, and leaves a list alone, so a config
// written by the form and one written by hand read the same.
func lines(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, line := range strings.Split(t, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func firstLine(raw []byte) string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Error != "" {
		return out.Error
	}
	return string(raw)
}
