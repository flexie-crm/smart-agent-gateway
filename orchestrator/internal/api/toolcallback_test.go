package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// The one field a provider's registration form demands that nobody can work
// out from the far end's documentation, because it is OURS.
//
// It is shown on the OAuth sections and nowhere else, and it carries the exact
// string this deployment uses. Every provider's form asks for a callback URL
// when an application is registered, whichever grant is going to be used, so
// "this grant never redirects" is not a reason to withhold it: somebody still
// has to put something in that field.
func TestTheOAuthSectionsCarryThisDeploymentsCallbackURL(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	want := env.app.ToolRedirectURI()
	if want == "" || !strings.HasSuffix(want, "/connect/tool/callback") {
		t.Fatalf("this deployment has no usable callback address: %q", want)
	}

	// An OAuth driver shows it, filled in.
	rec := env.do(http.MethodGet,
		"/v1/tools/templates/api/fields?variant=oauth_client_credentials", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var form newToolFormBody
	env.decode(rec, &form)

	shown := ""
	for _, section := range form.Sections {
		for _, f := range section.Fields {
			if f.Type != tool.FieldCallback {
				continue
			}
			if section.Title != "Authentication" {
				t.Fatalf("the callback address is on the %q section, not where the client is entered", section.Title)
			}
			shown = f.Default
		}
	}
	if shown == "" {
		t.Fatal("an OAuth driver's form offers no callback address, so it has to be guessed")
	}
	if shown != want {
		t.Fatalf("the form offers %q to register, but this deployment uses %q", shown, want)
	}

	// A driver that authenticates with a typed credential redirects nobody, and
	// a callback address beside it would be a field that means nothing.
	rec = env.do(http.MethodGet, "/v1/tools/templates/api/fields?variant=bearer", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var plain newToolFormBody
	env.decode(rec, &plain)
	for _, section := range plain.Sections {
		for _, f := range section.Fields {
			if f.Type == tool.FieldCallback {
				t.Fatalf("a bearer-token tool was shown a callback address on %q", section.Title)
			}
		}
	}
}

// createSignInTool makes an API tool of the kind a person signs in to.
func (e *testEnv) createSignInTool(token, base string) int64 {
	e.t.Helper()
	return e.createAPITool(token, "signin", map[string]any{
		"base_url": base, "timeout_seconds": float64(10),
		"auth.authorize_url": "https://service.test/oauth2/authorize",
		"auth.token_url":     "https://service.test/oauth2/token",
		"auth.client_id":     "a-client", "auth.client_secret": "a-secret",
		"policy.mode": "denylist", "policy.verbs": "DELETE",
	}, "oauth_authorization_code")
}

// createKeyedTool makes one with a typed credential, which signs nobody in.
func (e *testEnv) createKeyedTool(token, base string) int64 {
	e.t.Helper()
	return e.createAPITool(token, "keyed", map[string]any{
		"base_url": base, "timeout_seconds": float64(10),
		"auth.token":  "a-token",
		"policy.mode": "denylist", "policy.verbs": "DELETE",
	}, "bearer")
}

func (e *testEnv) createAPITool(token, alias string, settings map[string]any, variant string) int64 {
	e.t.Helper()
	body := map[string]any{
		"template": "api", "variant": variant,
		"display_name": "Tool " + alias, "settings": settings,
	}
	rec := e.do(http.MethodPost, "/v1/tools/custom", token, body)
	e.expectStatus(rec, http.StatusCreated)
	var made struct {
		ID int64 `json:"id"`
	}
	e.decode(rec, &made)
	return made.ID
}

// A tool that needs a sign-in says so when it is OPENED.
//
// This is the defect it closes: the Connect button arrived only as a side
// effect of a successful connection test, so somebody opening a tool that had
// never been signed in to saw a complete-looking form, no sign-in, and no
// reason to go looking for one. Reported as "there is no sign-in on the edit
// modal", and it was right.
//
// Absent for every other kind of tool, which is the other half: a form that
// said "not connected" about a database tool would be inventing a state it
// does not have.
func TestTheEditFormSaysWhetherASignInIsNeededAndDone(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer service.Close()

	created := env.createSignInTool(token, service.URL)

	var detail toolDetailBody
	rec := env.do(http.MethodGet, "/v1/tools/"+itoa(created), token, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &detail)

	if detail.Connection == nil {
		t.Fatal("a tool that signs in to a service says nothing about it when opened, " +
			"so there is no sign-in on the form and no reason to look for one")
	}
	if detail.Connection.Connected {
		t.Fatalf("a tool nobody has signed in to reads as connected: %+v", detail.Connection)
	}

	// A tool that needs none is not told it is unconnected.
	plain := env.createKeyedTool(token, service.URL)
	rec = env.do(http.MethodGet, "/v1/tools/"+itoa(plain), token, nil)
	env.expectStatus(rec, http.StatusOK)

	var other toolDetailBody
	env.decode(rec, &other)
	if other.Connection != nil {
		t.Fatalf("a tool with a typed credential was given a sign-in state: %+v", other.Connection)
	}
}

// The page at the end of a sign-in is the whole of what somebody sees, so it
// has to say they are finished and that the tab can be shut.
//
// Reported as "a blank page that says almost nothing": a heading on white with
// one line under it reads like a page that failed to load, at the exact moment
// somebody is wondering whether the thing worked.
func TestTheCallbackPageSaysItIsDoneAndCanBeClosed(t *testing.T) {
	env := newTestEnv(t)

	// No state and no code: the refusal page, which must ALSO say what to do.
	rec := env.do(http.MethodGet, "/connect/tool/callback", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an address visited with nothing on it answered %d", rec.Code)
	}
	refused := rec.Body.String()
	if !strings.Contains(refused, "Close this tab") {
		t.Fatalf("the refusal page does not say what to do:\n%s", refused)
	}

	// And the page a browser actually renders carries what it needs to: a
	// viewport, so it is not a desktop page on a phone, and the message back to
	// the window that opened it, which is what stops the form saying "not
	// connected" until somebody reloads.
	for _, want := range []string{"viewport", "window.opener", "postMessage", "tool-connect"} {
		if !strings.Contains(refused, want) {
			t.Fatalf("the page carries no %q:\n%s", want, refused)
		}
	}
	// The message is posted to our OWN origin and nowhere else.
	if !strings.Contains(refused, "window.origin") {
		t.Fatalf("the page posts its message to any origin, not just ours:\n%s", refused)
	}
}

// What a refused sign-in says, and how it says it.
//
// This is the defect it closes, reported from a screenshot of the page. A
// service refusing the exchange produced:
//
//	the service refused the token request: the service answered 400:
//	Authorization code is invalid, expired, or already used.
//
// Three clauses, two of them ours, saying one thing twice before the sentence
// somebody needed, run together as body prose in the same voice as our own
// words. The page now owns ONE sentence and quotes the service apart from it.
func TestARefusedSignInQuotesTheServiceOnceAndApartFromUs(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	const said = "Authorization code is invalid, expired, or already used."
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + said + `"}`))
	}))
	defer service.Close()

	created := env.createAPITool(token, "refused", map[string]any{
		"base_url": service.URL, "timeout_seconds": float64(10),
		"auth.authorize_url": service.URL + "/oauth2/authorize",
		"auth.token_url":     service.URL + "/oauth2/token",
		"auth.client_id":     "a-client", "auth.client_secret": "a-secret",
		"policy.mode": "denylist", "policy.verbs": "DELETE",
	}, "oauth_authorization_code")

	// A real round trip: the state on the page is the one we minted, sealed,
	// so this drives the whole exchange rather than a hand-built request.
	rec := env.do(http.MethodPost, "/v1/tools/"+itoa(created)+"/connection", token, map[string]any{})
	env.expectStatus(rec, http.StatusOK)
	var begun struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	env.decode(rec, &begun)
	sent, err := url.Parse(begun.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	state := sent.Query().Get("state")
	if state == "" {
		t.Fatal("the sign-in carried no state, so there is no round trip to finish")
	}

	rec = env.do(http.MethodGet, "/connect/tool/callback?state="+url.QueryEscape(state)+
		"&code=a-spent-code", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a refused exchange answered %d", rec.Code)
	}
	page := rec.Body.String()

	// The service is quoted once.
	if n := strings.Count(page, said); n != 1 {
		t.Fatalf("the service's words appear %d times, not once:\n%s", n, page)
	}
	// And our own wrapping of it is not on the page at all. Both clauses: one
	// is the error type's, one is the reader of the body.
	for _, ours := range []string{"refused the token request", "the sign-in could not be completed"} {
		if strings.Contains(page, ours) {
			t.Fatalf("the page carries our wrapping %q over the service's own words:\n%s", ours, page)
		}
	}
	// Set apart from our prose, not run into it: inside the block, and not in
	// a paragraph of ours.
	block := between(page, `<div class="said">`, "</div>")
	if !strings.Contains(block, said) {
		t.Fatalf("the service's words are not in the block that sets them apart:\n%s", page)
	}
	// The status is the one prefix worth keeping, and it is the only one left.
	if !strings.HasPrefix(block, "the service answered 400: ") {
		t.Fatalf("the quoted words do not say what the service answered: %q", block)
	}
	// One sentence of ours, above it.
	if !strings.Contains(page, "<p>The sign-in did not finish.</p>") {
		t.Fatalf("the page does not say in our own words what happened:\n%s", page)
	}
}

// Both callback pages are the same page.
//
// There were two, and only one of them was ever improved: the tool's became a
// card and the MCP connection's stayed a heading on white with an empty
// paragraph under it, which is the exact thing that was reported as "a blank
// page that says almost nothing". A shared page cannot drift like that.
func TestBothCallbackPagesAreTheOneCard(t *testing.T) {
	env := newTestEnv(t)

	pages := map[string]string{}
	for _, at := range []string{"/connect/tool/callback", "/connect/mcp/callback"} {
		rec := env.do(http.MethodGet, at, "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s visited with nothing on it answered %d", at, rec.Code)
		}
		pages[at] = rec.Body.String()
	}

	for at, page := range pages {
		for _, want := range []string{
			`class="card"`,           // a card, not a heading on white
			`class="badge"`,          // the mark beside the word, not above it
			`viewBox="0 0 20 20"`,    // drawn, not a font glyph
			"prefers-color-scheme",   // follows the reader's own setting
			`class="close"`,          // and says what to do next
			"Nothing to finish here", // an address visited on its own says so
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s is not the shared card, it carries no %q:\n%s", at, want, page)
			}
		}
	}
}

// between returns what lies between two markers, or "".
func between(text, from, to string) string {
	start := strings.Index(text, from)
	if start < 0 {
		return ""
	}
	rest := text[start+len(from):]
	end := strings.Index(rest, to)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// Nothing to say means no element, rather than an empty one holding space.
//
// Driven straight at the page, because no outcome in the product carries an
// empty detail today: asserting it on a real callback would be an assertion
// that cannot fail, which is how the rule would quietly stop holding.
func TestAnOutcomeWithNothingToAddRendersNoParagraph(t *testing.T) {
	rec := httptest.NewRecorder()
	writeConnectPage(rec, http.StatusOK, connectOutcome{
		ok: true, title: "Connected", back: "the tool", kind: "tool-connect",
	})
	page := rec.Body.String()

	if strings.Contains(page, "<p></p>") {
		t.Fatalf("an outcome with no detail renders an empty paragraph:\n%s", page)
	}
	if strings.Contains(page, `<div class="said"></div>`) {
		t.Fatalf("an outcome with nothing reported renders an empty block:\n%s", page)
	}
	// The line that says what to do is not optional, and neither is the word.
	for _, want := range []string{"<h1>Connected</h1>", "This tab can be closed safely."} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page carries no %q:\n%s", want, page)
		}
	}
}
