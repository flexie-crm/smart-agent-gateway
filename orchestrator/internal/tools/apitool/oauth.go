package apitool

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/useragent"
)

// AuthClientCredentials is the OAuth 2.1 client-credentials grant: the tool
// proves itself and nobody signs in.
//
// TWO THINGS THE SPECIFICATION SETTLES, quoted because both have been argued
// about here and neither is a matter of taste.
//
// There is no authorization endpoint in this grant. RFC 6749 section 4.4:
// "Since the client authentication is used as the authorization grant, no
// additional authorization request is needed." So a form asking for an
// authorization address would be asking for something that is never sent
// anywhere, and the token address is the only one this grant has.
//
// And a refresh token is discouraged rather than forbidden. RFC 6749 section
// 4.4.3: "A refresh token SHOULD NOT be included." Some servers send one
// regardless, and fetch() below reads only the access token, so any refresh
// token is dropped. That is deliberate: this tool holds the client ID and
// secret, so another access token is one request away, and keeping a refresh
// token would be a second credential to seal and rotate for no capability. It
// is also why this driver persists nothing and needs no state across a restart.
//
//nolint:gosec // G101: the NAME of a grant, stored as auth.kind, not a credential
const AuthClientCredentials = "oauth_client_credentials"

// AuthAuthorizationCode is the OAuth 2.1 authorization-code grant: a PERSON
// signs in at the service and consents, and the tool then acts within what
// THEY were allowed.
//
// The reason to want it and the reason it cannot be automated are the same one.
// The other grants prove the TOOL; this proves a person, so what the assistant
// may do at the service is exactly what the administrator who consented may do,
// and the service's own audit log carries their name. That needs a browser,
// which is why this one has a Connect button and the others do not.
//
//nolint:gosec // G101: the NAME of a grant, stored as auth.kind, not a credential
const AuthAuthorizationCode = "oauth_authorization_code"

// Where the client's own credentials go on the token request. Both are in the
// specification and services genuinely differ, so it is asked rather than
// guessed: sending the wrong one is a 401 with no explanation.
const (
	CredentialsInHeader = "basic"
	CredentialsInBody   = "body"
)

// early is how long before expiry a token is replaced. A token that expires
// while in flight is a failed call somebody has to understand, and a minute of
// a one-hour token is nothing.
const early = time.Minute

// tokens is the process's cache of access tokens, shared by every tool that
// uses this grant.
//
// Keyed by the token request's own identity rather than by a tool id, so two
// tools configured with the same credentials share one token, which is correct:
// it is the same client asking the same authorization server for the same
// scope.
type tokens struct {
	client *http.Client
	mu     sync.Mutex
	held   map[string]*entry
}

// entry is one cached token, with a lock of its OWN.
//
// The reason it is per entry and not one lock over the map: a token request is a
// network call, and holding a single lock across it would make every API tool in
// the process wait for one authorization server. Per entry, callers for the same
// token queue behind one fetch (which is the point, so twenty concurrent calls
// do not make twenty token requests) while different APIs proceed in parallel.
type entry struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

func newTokens(client *http.Client) *tokens {
	if client == nil {
		client = newClient()
	}
	return &tokens{client: client, held: map[string]*entry{}}
}

func (t *tokens) entryFor(key string) *entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.held[key]
	if !ok {
		e = &entry{}
		t.held[key] = e
	}
	return e
}

// For answers with a usable access token, fetching one only when what is held
// is missing or about to expire.
//
// via is the network this token is being obtained over, and it is BOTH the
// client to fetch with and part of the cache's identity. Nil and empty mean the
// ordinary case: this server's own network, one token per credential.
//
// The identity half is the part worth writing down. A tool that reaches its API
// through somebody's own computer reaches a token endpoint there too, and two
// people's networks answer for the same host name while being different
// servers. Keyed on the credential alone, the first person's token would be
// handed to the second. It is the same hazard the database tool names on its
// own connection cache, and the same answer.
func (t *tokens) For(ctx context.Context, a Auth, via *http.Client, from string) (string, error) {
	e := t.entryFor(keyOf(a, from))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.value != "" && time.Until(e.expires) > early {
		return e.value, nil
	}
	client := t.client
	if via != nil {
		client = via
	}
	value, expires, err := fetch(ctx, client, a)
	if err != nil {
		return "", err
	}
	e.value, e.expires = value, expires
	return value, nil
}

// forget drops a held token, so a 401 from the API can be answered by asking
// for a new one rather than by failing for an hour.
func (t *tokens) forget(a Auth, from string) {
	e := t.entryFor(keyOf(a, from))
	e.mu.Lock()
	defer e.mu.Unlock()
	e.value, e.expires = "", time.Time{}
}

// keyOf identifies a token request without keeping the secret in a map key.
//
// The secret is part of the identity (a rotated one must not hand back the old
// token) but a credential does not belong in a key that could reach a log, so
// it is hashed.
func keyOf(a Auth, from string) string {
	// Everything that decides WHICH token comes back, including the assertion's
	// identity and the key it is signed with: a rotated key must not hand back
	// the token the old one obtained, and two tools acting as different
	// subjects of one issuer must not share one.
	sum := sha256.Sum256([]byte(strings.Join([]string{
		a.Kind, a.TokenURL, a.ClientID, a.ClientSecret, a.Scope,
		a.Issuer, a.Subject, a.Audience, a.Algorithm, a.PrivateKey,
		// And WHOSE NETWORK it was obtained over, when that is not ours.
		from,
	}, "\x00")))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// excerptLimit bounds what a third party's error body contributes to a message
// somebody reads. Long enough for an OAuth error or a sentence of prose, short
// enough that an HTML page does not become the message.
const excerptLimit = 200

// excerpt turns whatever a server sent into one readable line.
//
// Whitespace collapsed, because an error page is mostly newlines and indentation
// and a message with those in it is unreadable in a toast; truncated with an
// ellipsis, so it is obvious that there was more.
func excerpt(raw []byte) string {
	text := strings.Join(strings.Fields(string(raw)), " ")
	if len(text) <= excerptLimit {
		return text
	}
	return text[:excerptLimit] + "…"
}

// tokenForm is the client-credentials token request.
func tokenForm(a Auth) (url.Values, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if a.Scope != "" {
		form.Set("scope", a.Scope)
	}
	if a.Credentials == CredentialsInBody {
		form.Set("client_id", a.ClientID)
		form.Set("client_secret", a.ClientSecret)
	}
	return form, nil
}

// fetch asks the authorization server for a token.
func fetch(ctx context.Context, client *http.Client, a Auth) (string, time.Time, error) {
	// A JWT tool exchanges nothing: the token it signs IS the credential, so
	// there is no request to make and no service to be down. Minted through the
	// same cache as every other token, so it is renewed before it expires
	// rather than signed afresh on every call.
	if a.Kind == AuthJWTBearer {
		now := time.Now().UTC()
		signed, signErr := signAssertion(a, now)
		if signErr != nil {
			return "", time.Time{}, signErr
		}
		return signed, now.Add(lifeOf(a)), nil
	}

	form, err := tokenForm(a)
	if err != nil {
		return "", time.Time{}, err
	}

	// A token endpoint may not wander either. The credential is in this body,
	// so a redirect to another host is the worst of the three places one can
	// happen, and there is no legitimate reason for a token route to move.
	runCtx, cancel := context.WithTimeout(withinBase(ctx, a.TokenURL), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the token request could not be built: %w", err)
	}
	useragent.Set(req.Header)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The assertion IS the credential for a JWT grant, so there is nothing to
	// put in a header and a service reading an empty one answers 401.
	if a.Kind != AuthJWTBearer && a.Credentials != CredentialsInBody {
		req.SetBasicAuth(url.QueryEscape(a.ClientID), url.QueryEscape(a.ClientSecret))
	}

	res, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the authorization server could not be reached: %s", scrub(err.Error(), a))
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var answer struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &answer)

	if res.StatusCode < 200 || res.StatusCode > 299 {
		// The server's OWN words, which are what an administrator needs: "this
		// client is not allowed that scope" is the answer, and a generic
		// failure invented over it is not.
		if reason := strings.TrimSpace(answer.Description); reason != "" {
			return "", time.Time{}, fmt.Errorf("the authorization server refused: %s", reason)
		}
		if answer.Error != "" {
			return "", time.Time{}, fmt.Errorf("the authorization server refused: %s", answer.Error)
		}
		// Nothing we recognise came back, so hand over what DID. A bare
		// "answered 400" is where a diagnosis stops: the reason is almost
		// always in the body, and the body is discarded exactly when it is the
		// only thing left. It can be an OAuth error in some other shape, an
		// HTML page because the address points at a login screen rather than a
		// token endpoint, or a proxy's own words.
		//
		// Trimmed and scrubbed: it is a third party's text going into a message
		// an administrator reads, and this tool's own credentials appear in
		// enough error bodies to be worth removing from all of them.
		if body := excerpt(raw); body != "" {
			return "", time.Time{}, fmt.Errorf("the authorization server answered %d: %s",
				res.StatusCode, scrub(body, a))
		}
		return "", time.Time{}, fmt.Errorf("the authorization server answered %d with an empty body, "+
			"which usually means the token address is not a token endpoint", res.StatusCode)
	}
	if answer.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("the authorization server answered without a token")
	}
	// A type other than bearer is a different protocol (a signed request), not
	// a header we can guess at, so it is refused rather than half-supported.
	if kind := strings.ToLower(strings.TrimSpace(answer.TokenType)); kind != "" && kind != "bearer" {
		return "", time.Time{}, fmt.Errorf("this tool sends bearer tokens, and the authorization server issued a %q token", answer.TokenType)
	}
	// No expiry given means the server does not say; hold it briefly rather
	// than for ever, so a revoked token is not used all day.
	life := time.Duration(answer.ExpiresIn) * time.Second
	if answer.ExpiresIn <= 0 {
		life = 5 * time.Minute
	}
	return answer.AccessToken, time.Now().Add(life), nil
}
