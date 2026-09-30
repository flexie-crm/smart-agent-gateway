// Package apitool is the HTTP API template: one configured API is one tool.
//
// An administrator points it at a base URL, gives it the credentials once, and
// says which HTTP verbs it may use. The model then calls it with a method, a
// path and parameters, and gets the response back. It never sees the
// credentials, which are sealed at rest and injected here.
//
// WHAT IS DELIBERATELY NOT HERE: the API's own documentation. A real
// specification is hundreds of operations, and an enum of those would be sent
// to the model on every turn of every conversation whether or not the tool was
// touched. So the endpoints live in a SKILL instead, which is searched and
// drilled into on demand (KB/42), and this tool stays a transport with a fixed,
// tiny schema that does not grow with the API.
package apitool

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/template"
)

// Auth kinds. Each is a variant of the template, because each needs different
// fields on the form.
const (
	AuthNone   = "none"
	AuthAPIKey = "api_key"
	AuthBearer = "bearer"
	AuthBasic  = "basic"
)

// Where an API key travels. Two placements cover every API worth the name, and
// the name and prefix are the administrator's because there is no convention:
// X-API-Key, Authorization: Token abc, and ?api_key=abc are all in the wild.
const (
	InHeader = "header"
	InQuery  = "query"
)

// Settings is one configured API.
type Settings struct {
	// Driver is which variant this tool was made with, recorded where the app
	// looks for it (driverName reads a top-level "driver", and the other two
	// templates carry one for the same reason).
	//
	// It is not read HERE: what the handler acts on is Auth.Kind, and both are
	// written from one value in Config, so they cannot drift. Without it an
	// edit recovers no variant, and the form is rebuilt as though the tool had
	// no authentication at all: found by a test asking why a Connect button
	// was missing, which is a much smaller symptom than the cause.
	Driver  string `json:"driver,omitempty"`
	BaseURL string `json:"base_url"`
	// Timeout bounds one call. Zero takes the default.
	Timeout time.Duration `json:"-"`
	Seconds int           `json:"timeout_seconds,omitempty"`
	Auth    Auth          `json:"auth"`
	Policy  Policy        `json:"policy"`
	// Grant is what a PERSON gave this tool by signing in, and it is the one
	// part of a custom tool's configuration the FORM does not own.
	//
	// Everything else here was typed by an administrator: authored, sealed on
	// write, blanked when the form reopens. This arrives from a third party
	// mid-flight and rotates without anybody pressing anything, so the two must
	// not compete for the same space. They share a config because a custom
	// tool's config IS its per-instance payload, and they are kept apart inside
	// it: nothing the form sends writes under `grant`, and an edit carries the
	// whole subtree forward untouched (App.carryGrant).
	Grant Grant `json:"grant,omitempty"`
	// ExtraHeaders and ExtraQuery are further name/value pairs sent with every
	// call, one per line as "name: value".
	//
	// On the API rather than on the credential, because that is what they are:
	// a version header, an account id, a tenant. They are sent whatever the
	// authentication is, including none, so an administrator configuring a
	// public API still has them.
	//
	// Two fields rather than one because WHERE a parameter goes is a property
	// of the parameter: a service can want an account id in a header and a
	// version in the query at once, and a single list could only do one.
	//
	// Strings rather than lists, and not out of laziness: the app seals a
	// secret by path and only ever seals a string leaf (sealConfigSecrets uses
	// leafString), so a list here would be SKIPPED and every value in it
	// written to the database in plaintext.
	ExtraHeaders string `json:"extra_headers,omitempty"`
	ExtraQuery   string `json:"extra_query,omitempty"`
	// Read from those two on Parse. Never stored.
	HeaderPairs []tool.Pair `json:"-"`
	QueryPairs  []tool.Pair `json:"-"`
	// ThroughChat carries every call through the chat application of whoever is
	// using the tool, for an API this server cannot reach at all.
	//
	// Read from the stored configuration rather than from a field of its own,
	// because the form's dotted key is stored NESTED and a field named for the
	// dotted spelling matches nothing (see template.ReachChat, and the defect
	// it records). Nothing here marshals Settings back into a stored config,
	// so unlike the server tool there is nothing to carry.
	ThroughChat bool `json:"-"`
}

// Grant is the sign-in a person gave this tool: a usable token, the refresh
// token that renews it, and who consented.
//
// The tokens are sealed by hand where they are WRITTEN rather than through
// SecretPaths, because sealing by path runs on save and this is not saved by a
// form: a value already sealed would be sealed a second time. Opening needs no
// arrangement at all, because that walks the whole config and works on the
// marker (App.openConfigSecrets).
type Grant struct {
	AccessToken  string     `json:"access_token,omitempty"`
	RefreshToken string     `json:"refresh_token,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	// Who consented. The name is frozen beside the id for the reason every
	// authored row freezes one: the id goes to NULL when somebody is deleted,
	// and "connected by" has to survive them.
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	ConnectedBy     int64      `json:"connected_by,omitempty"`
	ConnectedByName string     `json:"connected_by_name,omitempty"`
}

// GrantPath is where the grant lives in the config JSON. Named once, so the
// carry-forward and the form's own refusal cannot mean different keys.
const GrantPath = "grant"

// Held reports that somebody has signed in and there is a token to use.
func (g Grant) Held() bool { return strings.TrimSpace(g.AccessToken) != "" }

// Auth is how this API is proved to, with the secret still in it: the app seals
// these paths at rest (SecretPaths) and opens them before Bind.
type Auth struct {
	Kind string `json:"kind"`
	// API key.
	Key       string `json:"key,omitempty"`
	Placement string `json:"placement,omitempty"`
	Name      string `json:"name,omitempty"`
	// Bearer.
	Token string `json:"token,omitempty"`
	// Basic.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// The client-credentials grant. No refresh token and no person, so the
	// access token is re-obtainable from these at any moment and is held in
	// memory rather than written anywhere (see oauth.go).
	TokenURL string `json:"token_url,omitempty"`
	// AuthorizeURL is where the PERSON is sent to consent, used by the
	// authorization-code grant only. Typed, because an ordinary service
	// documents its two addresses in prose rather than publishing the metadata
	// document an MCP server does.
	AuthorizeURL string `json:"authorize_url,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Credentials  string `json:"credentials,omitempty"`
	// AuthorizeParams are further name/value pairs added to the sign-in
	// address, one per line as "name: value".
	//
	// Generic on purpose, because the alternative is a field per vendor. What
	// goes here is a service's own requirement rather than anything OAuth
	// defines, and the requirements are not optional: Google issues NO refresh
	// token unless the sign-in carries access_type=offline, so a connection
	// made without it works for an hour and then stops for good. Auth0 wants an
	// audience, Microsoft sometimes wants a prompt.
	//
	// Kept out of Scope, which is a defined parameter with its own field, and
	// out of the API's extra query parameters, which are sent on every CALL
	// rather than once at the sign-in.
	AuthorizeParams string `json:"authorize_params,omitempty"`
	// AuthorizePairs is AuthorizeParams read into pairs. Not stored.
	AuthorizePairs []tool.Pair `json:"-"`
	// The JWT authorization grant (RFC 7523), where the tool signs a JWT with a
	// private key instead of sending a shared secret. Nothing here is a
	// credential except the key.
	//
	// Issuer, Subject and Audience are claims the specification requires of
	// the assertion, asked for rather than derived because services disagree
	// about what belongs in them: one service wants an account address in iss
	// and the same again or a delegated person in sub, another wants a consumer
	// key and a username, and the audience is the token endpoint for some and a
	// login host for others.
	Issuer   string `json:"issuer,omitempty"`
	Subject  string `json:"subject,omitempty"`
	Audience string `json:"audience,omitempty"`
	// PrivateKey is the PEM the assertion is signed with, sealed at rest like
	// every other secret. It never leaves this installation: what travels is a
	// signature over a few claims, which is the point of the grant.
	PrivateKey string `json:"private_key,omitempty"`
	Algorithm  string `json:"algorithm,omitempty"`
	// Lifetime is how many minutes a minted token lasts. Zero takes a default.
	// An administrator's choice because the ceiling is the service's: some
	// refuse anything over an hour, and a token that outlives what they allow
	// is refused on every call with nothing said about why.
	Lifetime int `json:"lifetime_minutes,omitempty"`
	// Claims are further name/value pairs put in the token, one per line as
	// "name: value". Generic, so a service asking for something beyond the
	// four the format defines needs no field of its own.
	Claims      string      `json:"claims,omitempty"`
	ClaimsPairs []tool.Pair `json:"-"`
}

// DefaultTimeout bounds a call that set none. An API that has not answered in
// half a minute is not going to.
const DefaultTimeout = 30 * time.Second

// Parse reads a stored config and refuses one that could not work.
//
// Refusing here rather than at call time is the pre-park contract in KB/02: an
// administrator who was told the tool was saved has been told it will run.
func Parse(raw json.RawMessage) (Settings, error) {
	var s Settings
	if len(raw) == 0 {
		return Settings{}, fmt.Errorf("this tool has no settings saved")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("this tool's saved settings could not be read: %w", err)
	}
	base, err := BaseURL(s.BaseURL)
	if err != nil {
		return Settings{}, err
	}
	s.BaseURL = base
	s.Timeout = DefaultTimeout
	if s.Seconds > 0 {
		s.Timeout = time.Duration(s.Seconds) * time.Second
	}
	s.ThroughChat = template.ReachChat(raw)
	if err := s.Auth.check(); err != nil {
		return Settings{}, err
	}
	if err := s.Policy.check(); err != nil {
		return Settings{}, err
	}
	// The shared reader for this field type, so every template that declares
	// FieldPairs reads one the same way.
	headers, err := tool.ReadPairs(s.ExtraHeaders)
	if err != nil {
		return Settings{}, fmt.Errorf("the additional headers are not right: %w", err)
	}
	query, err := tool.ReadPairs(s.ExtraQuery)
	if err != nil {
		return Settings{}, fmt.Errorf("the additional query parameters are not right: %w", err)
	}
	s.HeaderPairs, s.QueryPairs = headers, query

	// The sign-in's own extra parameters, read by the same rule and refused at
	// save time by the same error, so one format is learned once.
	signIn, err := tool.ReadPairs(s.Auth.AuthorizeParams)
	if err != nil {
		return Settings{}, fmt.Errorf("the extra sign-in parameters are not usable: %w", err)
	}
	s.Auth.AuthorizePairs = signIn

	claims, err := tool.ReadPairs(s.Auth.Claims)
	if err != nil {
		return Settings{}, fmt.Errorf("the extra claims are not usable: %w", err)
	}
	s.Auth.ClaimsPairs = claims
	return s, nil
}

// BaseURL normalises the address every call is made under, and is where the
// confinement starts: a path is later joined UNDER this, so what this accepts
// decides what the tool can ever reach.
//
// Its own path is kept, and so is a TRAILING SLASH, because a slash is part of
// an address rather than noise on it. It used to be trimmed here "so joining is
// one rule rather than two", and that reasoning holds only while a base is a
// PREFIX. Where the base is the whole endpoint, the slash is the address: a
// service that appends one (Django's APPEND_SLASH is the common case, and a
// listener issued with one is another) answers a redirect or a 404 without it,
// and trimming made the right address unreachable with no way to get it back.
// Joining stays one rule because `under` compares without it, which is where a
// trailing slash never mattered anyway.
//
// A QUERY STRING is kept too, and is not the same as a parameter on a call:
// plenty of vendors issue an endpoint with an identifier already in it. It used
// to be refused on the grounds that it "would be sent on every call or silently
// dropped", which was half right and half wrong. It IS sent on every call,
// because that is what putting it in the address means. What it was never safe
// from is being displaced, so the handler re-applies it LAST, the way the
// credential and the administrator's own extras are applied: a model naming the
// same parameter can no longer overwrite part of the address.
func BaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("the base address is required: the address every call is made under, such as https://api.example.com/v1")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the base address is not a valid address: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("the base address must begin with http:// or https://, and this one begins with %q", u.Scheme+"://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("the base address has no host in it, so there is nothing to call")
	}
	// A FRAGMENT is still refused, and unlike a query it is not a judgement
	// call: a fragment is never sent to a server at all, so one here is a
	// misunderstanding of the field and not a requirement of any API.
	if u.Fragment != "" {
		return "", fmt.Errorf("the base address must not carry a #fragment: a fragment is never sent to the server, " +
			"so it can have no effect on a call")
	}
	return u.String(), nil
}

func (a *Auth) check() error {
	a.Kind = strings.TrimSpace(a.Kind)
	if a.Kind == "" {
		a.Kind = AuthNone
	}
	switch a.Kind {
	case AuthNone:
		return nil
	case AuthAPIKey:
		if strings.TrimSpace(a.Key) == "" {
			return fmt.Errorf("the API key is required")
		}
		a.Placement = strings.TrimSpace(a.Placement)
		if a.Placement == "" {
			a.Placement = InHeader
		}
		if a.Placement != InHeader && a.Placement != InQuery {
			return fmt.Errorf("choose whether the API key is sent in a header or in the query string; %q is neither", a.Placement)
		}
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("the API key needs the name this service expects it under, such as X-API-Key or Authorization")
		}
		return nil
	case AuthBearer:
		if strings.TrimSpace(a.Token) == "" {
			return fmt.Errorf("the bearer token is required")
		}
		return nil
	case AuthBasic:
		if strings.TrimSpace(a.User) == "" {
			return fmt.Errorf("the username is required")
		}
		return nil
	case AuthAuthorizationCode:
		if strings.TrimSpace(a.ClientID) == "" || strings.TrimSpace(a.ClientSecret) == "" {
			return fmt.Errorf("the client ID and the client secret are both required")
		}
		// The two addresses are OPTIONAL because they are looked up from the
		// service's own metadata when it publishes any, which most services
		// that do OAuth at all now do. What cannot be looked up is the client:
		// no vendor registers one for us, so somebody creates the application
		// there and pastes it back. Optional does not mean unchecked, so a
		// typed address is still an address.
		if strings.TrimSpace(a.AuthorizeURL) != "" {
			authorize, err := BaseURL(a.AuthorizeURL)
			if err != nil {
				return fmt.Errorf("the sign-in address is not usable: %w", err)
			}
			a.AuthorizeURL = authorize
		}
		if strings.TrimSpace(a.TokenURL) != "" {
			token, err := BaseURL(a.TokenURL)
			if err != nil {
				return fmt.Errorf("the token address is not usable: %w", err)
			}
			a.TokenURL = token
		}
		return nil
	case AuthJWTBearer:
		// The key and the issuer are what cannot be defaulted.
		if strings.TrimSpace(a.Issuer) == "" {
			return fmt.Errorf("the issuer is required: it is the identity the service knows this key by")
		}
		if strings.TrimSpace(a.PrivateKey) == "" {
			return fmt.Errorf("the signing key is required: this tool signs the token rather than sending a secret")
		}
		alg := strings.TrimSpace(a.Algorithm)
		switch alg {
		case "", AlgHS256, AlgHS384, AlgHS512, AlgRS256, AlgES256:
		default:
			return fmt.Errorf("%q is not a signing algorithm this tool knows", a.Algorithm)
		}
		// A key that cannot sign the chosen way is caught now rather than on
		// the first call: an asymmetric algorithm needs a readable PEM, and a
		// shared secret is any text at all.
		if !symmetric(alg) && alg != "" {
			if _, err := readPrivateKey(a.PrivateKey); err != nil {
				return err
			}
		}
		if a.Lifetime < 0 {
			return fmt.Errorf("the token lifetime cannot be negative")
		}
		return nil
	case AuthClientCredentials:
		if strings.TrimSpace(a.ClientID) == "" || strings.TrimSpace(a.ClientSecret) == "" {
			return fmt.Errorf("the client ID and the client secret are both required")
		}
		// The token address is checked the same way the base address is: it is
		// reached with the client's own credentials on it, so it must be one
		// somebody chose on purpose.
		token, err := BaseURL(a.TokenURL)
		if err != nil {
			return fmt.Errorf("the token address is not usable: %w", err)
		}
		a.TokenURL = token
		a.Credentials = strings.TrimSpace(a.Credentials)
		if a.Credentials == "" {
			a.Credentials = CredentialsInHeader
		}
		if a.Credentials != CredentialsInHeader && a.Credentials != CredentialsInBody {
			return fmt.Errorf("choose whether the client ID and secret are sent in the Authorization header or in the request body; %q is neither", a.Credentials)
		}
		return nil
	default:
		return fmt.Errorf("unknown authentication %q", a.Kind)
	}
}
