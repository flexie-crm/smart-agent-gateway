package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store/sqlstore"
	"flexie.io/sag/internal/testdb"
)

// The API suite drives the real router against a real database, so routing,
// middleware, permission checks, workspace scoping, and SQL are all
// exercised together. Set SAG_TEST_DSN to a scratch database.

type testEnv struct {
	t      *testing.T
	app    *app.App
	sql    *sqlstore.SQLStore
	router http.Handler
	ws     *model.Workspace
	// userAgent is who the next request claims to be. Empty is the default
	// browser; `as()` makes it somebody else.
	userAgent string
}

// dbSuffix names this package's scratch database. Open makes it, TestMain
// takes it away, and they read it from here so they cannot drift apart.
const dbSuffix = "api"

// testSessionSecret is the HS256 signing secret the suite's app runs with. The
// security tests sign tokens with it (to forge an expiry) or deliberately NOT
// with it (to forge a signature), so it must be the one the app verifies against.
const testSessionSecret = "0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, dbSuffix)) }

func newTestEnv(t *testing.T, opts ...func(*config.Config)) *testEnv {
	t.Helper()
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping API suite")
	}
	st, _ := testdb.Open(t, dsn, dbSuffix)

	cfg := &config.Config{
		BaseURL:                "http://sag.test",
		SessionSecret:          []byte(testSessionSecret),
		EncryptionKeys:         "1:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		EncryptionPrimaryKeyID: "1",
		// Uploaded bytes go somewhere the test framework takes away again, so a
		// suite never writes into the working tree.
		UploadDir: t.TempDir(),
		// The real default, so the suite exercises the behaviour that ships
		// rather than the zero value a struct literal happens to give it.
		RefreshReuseGrace: time.Minute,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	a, err := app.New(cfg, zerolog.Nop(), st)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}

	// The live machinery app.New wires up needs its goroutines running, the same
	// as the server starts them: the hub answers the connected count, the bus
	// fans out events, and the dashboard publisher subscribes and pushes. Without
	// them /stats would block on a hub that is not draining its channels.
	liveCtx, liveCancel := context.WithCancel(context.Background())
	t.Cleanup(liveCancel)
	go a.WS.Run(liveCtx)
	go a.Bus.Run(liveCtx)
	go a.RunLiveDashboard(liveCtx)

	env := &testEnv{t: t, app: a, sql: st, router: newRouter(a)}
	env.ws = &model.Workspace{Slug: "acme", Name: "Acme"}
	if err := st.Workspaces().Create(context.Background(), env.ws); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// The server offers the code's tools to every workspace at boot. The suite
	// starts from the same state a real deployment does, so a test can never
	// pass against a workspace that has tools no deployment would have.
	if err := a.SyncTools(context.Background(), env.ws.ID); err != nil {
		t.Fatalf("sync tools: %v", err)
	}
	return env
}

// createUser inserts a user with the given permissions, granted through a
// dedicated group and role (the real permission path, not a shortcut).
func (e *testEnv) createUser(email, password string, perms ...string) *model.User {
	e.t.Helper()
	ctx := context.Background()

	hash, err := auth.HashPassword(password)
	if err != nil {
		e.t.Fatalf("hash password: %v", err)
	}
	user := &model.User{Email: email, Name: email, PasswordHash: hash}
	if err := e.app.Store.Users().Create(ctx, user); err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	if err := e.app.Store.Workspaces().SetMembers(ctx, user.ID, []int64{e.ws.ID}); err != nil {
		e.t.Fatalf("add member: %v", err)
	}
	if len(perms) == 0 {
		return user
	}

	group := &model.Group{WorkspaceID: e.ws.ID, Name: "group-" + email}
	if err := e.app.Store.Groups().Create(ctx, group); err != nil {
		e.t.Fatalf("create group: %v", err)
	}
	role := &model.Role{WorkspaceID: e.ws.ID, Name: "role-" + email, Permissions: perms}
	if err := e.app.Store.Roles().Create(ctx, role); err != nil {
		e.t.Fatalf("create role: %v", err)
	}
	if err := e.app.Store.Groups().AssignRole(ctx, group.ID, role.ID); err != nil {
		e.t.Fatalf("assign role: %v", err)
	}
	if err := e.app.Store.Groups().AddMember(ctx, group.ID, user.ID); err != nil {
		e.t.Fatalf("add member: %v", err)
	}
	return user
}

func (e *testEnv) do(method, path, token string, body any) *httptest.ResponseRecorder {
	return e.doCookie(method, path, token, body, nil)
}

// doCookie is do with a refresh cookie attached, for the auth endpoints that read
// the refresh token from the cookie rather than a header or a body.
func (e *testEnv) doCookie(method, path, token string, body any, refresh *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if refresh != nil {
		req.AddCookie(refresh)
	}
	if e.userAgent != "" {
		req.Header.Set("User-Agent", e.userAgent)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// as runs the next requests as a DIFFERENT browser. Refresh-token reuse
// detection turns on who is presenting the token, so a test about theft has to
// be able to be somebody else.
func (e *testEnv) as(userAgent string) *testEnv {
	e.userAgent = userAgent
	return e
}

// login performs a real login and returns the access token plus the refresh
// COOKIE the server set. The refresh token never appears in the body.
func (e *testEnv) login(email, password string) (accessToken string, refresh *http.Cookie) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": email, "password": password,
	})
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	e.decode(rec, &resp)
	return resp.AccessToken, e.refreshCookie(rec)
}

// refreshCookie returns the refresh-token cookie a response set, or nil for none.
func (e *testEnv) refreshCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	e.t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == refreshCookie {
			return c
		}
	}
	return nil
}

func (e *testEnv) decode(rec *httptest.ResponseRecorder, dst any) {
	e.t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		e.t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func (e *testEnv) expectStatus(rec *httptest.ResponseRecorder, want int) {
	e.t.Helper()
	if rec.Code != want {
		e.t.Fatalf("expected status %d, got %d: %s", want, rec.Code, rec.Body.String())
	}
}

// expectFields asserts both levels of a refusal: the status, the code, a
// form-level description, and exactly the field messages given, no extras
// and none missing.
func (e *testEnv) expectFields(rec *httptest.ResponseRecorder, status int, code string, want map[string]string) {
	e.t.Helper()
	e.expectStatus(rec, status)
	var body errorBody
	e.decode(rec, &body)
	if body.Error != code {
		e.t.Fatalf("expected code %q, got %q: %s", code, body.Error, rec.Body.String())
	}
	if body.Description == "" {
		e.t.Fatalf("a field-level refusal still needs a form-level description: %s", rec.Body.String())
	}
	if len(body.Fields) != len(want) {
		e.t.Fatalf("expected %d field errors, got %v", len(want), body.Fields)
	}
	for field, message := range want {
		if got := body.Fields[field]; got != message {
			e.t.Fatalf("field %q: expected %q, got %q", field, message, got)
		}
	}
}

// --- auth ---------------------------------------------------------------------

func TestLoginSucceedsAndReturnsUsableToken(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)

	access, refresh := env.login("admin@acme.test", "dev-Passw0rd!")
	if access == "" {
		t.Fatal("login returned no access token")
	}
	// The refresh token is delivered ONLY as a locked-down cookie.
	if refresh == nil || refresh.Value == "" {
		t.Fatal("login set no refresh cookie")
	}
	if !refresh.HttpOnly || refresh.Path != "/v1/auth" {
		t.Fatalf("refresh cookie is not locked down: %+v", refresh)
	}

	rec := env.do(http.MethodGet, "/v1/auth/me", access, nil)
	env.expectStatus(rec, http.StatusOK)
	var me struct {
		User        userBody `json:"user"`
		Permissions []string `json:"permissions"`
	}
	env.decode(rec, &me)
	if me.User.Email != "admin@acme.test" {
		t.Fatalf("wrong identity: %+v", me.User)
	}
	if len(me.Permissions) != 1 || me.Permissions[0] != model.PermSuperuser {
		t.Fatalf("wrong permissions: %+v", me.Permissions)
	}
	// The password hash must never appear in a response body.
	if bytes.Contains(rec.Body.Bytes(), []byte("password")) {
		t.Fatalf("response leaks password material: %s", rec.Body.String())
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("user@acme.test", "dev-Passw0rd!")

	cases := []struct {
		name           string
		email, passwrd string
	}{
		{"wrong password", "user@acme.test", "wrong-password"},
		{"unknown user", "nobody@acme.test", "dev-Passw0rd!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
				"email": tc.email, "password": tc.passwrd,
			})
			env.expectStatus(rec, http.StatusUnauthorized)
			// Every failure must look identical, or the endpoint becomes
			// a user enumeration oracle.
			var body errorBody
			env.decode(rec, &body)
			if body.Error != "invalid_credentials" {
				t.Fatalf("failure reason leaked: %+v", body)
			}
		})
	}
}

// A person with the right password and no workspace is not a failed login: they
// are a real account nobody has placed yet, and telling them to check their
// password would send them to fix the one thing that works.
func TestLoginWithoutAnyWorkspaceIsRefusedPlainly(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("dev-Passw0rd!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	orphan := &model.User{Email: "nowhere@acme.test", Name: "Nowhere", PasswordHash: hash}
	if err := env.app.Store.Users().Create(ctx, orphan); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := env.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": "nowhere@acme.test", "password": "dev-Passw0rd!",
	})
	env.expectStatus(rec, http.StatusForbidden)
	var body errorBody
	env.decode(rec, &body)
	if body.Error != "no_workspace" {
		t.Fatalf("expected no_workspace, got %+v", body)
	}
}

// The switch is an authorization: it re-issues the token against a workspace the
// person belongs to, refuses one they do not, and takes the old session with it.
func TestSwitchWorkspace(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	user := env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)

	second := &model.Workspace{Slug: "second", Name: "Second"}
	if err := env.app.Store.Workspaces().Create(ctx, second); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	stranger := &model.Workspace{Slug: "stranger", Name: "Stranger"}
	if err := env.app.Store.Workspaces().Create(ctx, stranger); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := env.app.Store.Workspaces().SetMembers(ctx, user.ID, []int64{env.ws.ID, second.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}

	access, refresh := env.login("admin@acme.test", "dev-Passw0rd!")

	// The switcher offers exactly the two they belong to, never the third.
	rec := env.do(http.MethodGet, "/v1/auth/workspaces", access, nil)
	env.expectStatus(rec, http.StatusOK)
	var offered []workspaceBody
	env.decode(rec, &offered)
	if len(offered) != 2 {
		t.Fatalf("expected two workspaces, got %+v", offered)
	}

	// A workspace they do not belong to is refused, valid token and all.
	env.expectStatus(env.do(http.MethodPost, "/v1/auth/workspace", access, map[string]any{
		"workspace_id": stranger.ID,
	}), http.StatusForbidden)

	// The one they do belong to hands back a token that says so.
	rec = env.doCookie(http.MethodPost, "/v1/auth/workspace", access, map[string]any{
		"workspace_id": second.ID,
	}, refresh)
	env.expectStatus(rec, http.StatusOK)
	var switched struct {
		AccessToken string        `json:"access_token"`
		Workspace   workspaceBody `json:"workspace"`
	}
	env.decode(rec, &switched)
	if switched.Workspace.ID != second.ID {
		t.Fatalf("switched to the wrong workspace: %+v", switched.Workspace)
	}

	rec = env.do(http.MethodGet, "/v1/auth/me", switched.AccessToken, nil)
	env.expectStatus(rec, http.StatusOK)
	var me struct {
		Workspace workspaceBody `json:"workspace"`
	}
	env.decode(rec, &me)
	if me.Workspace.ID != second.ID {
		t.Fatalf("the new token still speaks for the old workspace: %+v", me.Workspace)
	}

	// The session it replaced is gone: a switch must not leave a refresh token
	// behind that still speaks for the workspace they left.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh), http.StatusUnauthorized)

	// And logging in again lands where they left off: the choice was remembered.
	access, _ = env.login("admin@acme.test", "dev-Passw0rd!")
	rec = env.do(http.MethodGet, "/v1/auth/me", access, nil)
	env.expectStatus(rec, http.StatusOK)
	env.decode(rec, &me)
	if me.Workspace.ID != second.ID {
		t.Fatalf("the last workspace was not remembered: %+v", me.Workspace)
	}
}

// A membership taken away must not survive in a token the person still holds.
func TestRefreshDiesWithTheMembership(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	user := env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)

	other := &model.Workspace{Slug: "other", Name: "Other"}
	if err := env.app.Store.Workspaces().Create(ctx, other); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	_, refresh := env.login("admin@acme.test", "dev-Passw0rd!")

	// They are moved out of the workspace their session was issued for.
	if err := env.app.Store.Workspaces().SetMembers(ctx, user.ID, []int64{other.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh), http.StatusUnauthorized)
}

// Settings are per person: a key one of them sets is not a key the other reads.
func TestUserSettingsAreTheirOwn(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("one@acme.test", "dev-Passw0rd!")
	env.createUser("two@acme.test", "dev-Passw0rd!")
	one, _ := env.login("one@acme.test", "dev-Passw0rd!")
	two, _ := env.login("two@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodPut, "/v1/me/settings/layout", one, map[string]string{
		"value": `{"sidebar":"collapsed"}`,
	}), http.StatusNoContent)

	rec := env.do(http.MethodGet, "/v1/me/settings", one, nil)
	env.expectStatus(rec, http.StatusOK)
	var mine map[string]string
	env.decode(rec, &mine)
	if mine["layout"] != `{"sidebar":"collapsed"}` {
		t.Fatalf("the value came back changed: %+v", mine)
	}

	rec = env.do(http.MethodGet, "/v1/me/settings", two, nil)
	env.expectStatus(rec, http.StatusOK)
	var theirs map[string]string
	env.decode(rec, &theirs)
	if _, found := theirs["layout"]; found {
		t.Fatalf("one person's setting reached another: %+v", theirs)
	}

	env.expectStatus(env.do(http.MethodDelete, "/v1/me/settings/layout", one, nil), http.StatusNoContent)
	rec = env.do(http.MethodGet, "/v1/me/settings", one, nil)
	left := map[string]string{}
	env.decode(rec, &left)
	if _, found := left["layout"]; found {
		t.Fatalf("the setting survived its deletion: %+v", left)
	}
}

func TestLoginRejectsDisabledUser(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("user@acme.test", "dev-Passw0rd!")
	user.Status = model.StatusDisabled
	if err := env.app.Store.Users().Update(context.Background(), user); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	rec := env.do(http.MethodPost, "/v1/auth/login", "", map[string]string{
		"email": "user@acme.test", "password": "dev-Passw0rd!",
	})
	env.expectStatus(rec, http.StatusUnauthorized)
}

func TestRefreshRotatesAndRejectsReplay(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("user@acme.test", "dev-Passw0rd!", model.PermUsersView)
	_, refresh := env.login("user@acme.test", "dev-Passw0rd!")

	rec := env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh)
	env.expectStatus(rec, http.StatusOK)
	rotatedCookie := env.refreshCookie(rec)
	var rotated struct {
		AccessToken string `json:"access_token"`
	}
	env.decode(rec, &rotated)
	if rotatedCookie == nil || rotatedCookie.Value == "" || rotatedCookie.Value == refresh.Value {
		t.Fatalf("refresh token was not rotated: %+v", rotatedCookie)
	}
	// The new access token works.
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", rotated.AccessToken, nil), http.StatusOK)

	// Replaying the consumed token from ANOTHER browser must fail, and takes the
	// family with it. From the same browser inside the grace window it is read as
	// a rotation the client never received; both halves of that distinction are
	// covered in auth_security_test.go.
	env.as("curl/8.0 (somebody else)")
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh), http.StatusUnauthorized)
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("user@acme.test", "dev-Passw0rd!")
	_, refresh := env.login("user@acme.test", "dev-Passw0rd!")

	logoutRec := env.doCookie(http.MethodPost, "/v1/auth/logout", "", nil, refresh)
	env.expectStatus(logoutRec, http.StatusNoContent)
	// Logout clears the cookie, so the browser stops presenting it.
	if cleared := env.refreshCookie(logoutRec); cleared == nil || cleared.Value != "" {
		t.Fatalf("logout did not clear the refresh cookie: %+v", cleared)
	}

	// The revoked token no longer refreshes.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, refresh), http.StatusUnauthorized)

	// Logging out with no cookie is a harmless no-op.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/logout", "", nil, nil), http.StatusNoContent)
	// A forged, unknown token is a no-op too: logout never says whether it was real.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/logout", "", nil,
		&http.Cookie{Name: refreshCookie, Value: "sag_ut_nonexistent"}), http.StatusNoContent)
}

func TestProtectedRoutesRejectMissingOrBadToken(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)

	env.expectStatus(env.do(http.MethodGet, "/v1/users", "", nil), http.StatusUnauthorized)
	env.expectStatus(env.do(http.MethodGet, "/v1/users", "garbage", nil), http.StatusUnauthorized)

	// A token signed with a different secret is not accepted.
	foreign := auth.NewTokenService([]byte("fedcba9876543210fedcba9876543210"))
	token, _, err := foreign.IssueAccessToken(1, env.ws.ID, 1)
	if err != nil {
		t.Fatalf("issue foreign token: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/users", token, nil), http.StatusUnauthorized)
}

// --- permissions -----------------------------------------------------------------

func TestPermissionsAreEnforcedPerRoute(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("viewer@acme.test", "dev-Passw0rd!", model.PermUsersView)
	viewer, _ := env.login("viewer@acme.test", "dev-Passw0rd!")

	// Granted permission works.
	env.expectStatus(env.do(http.MethodGet, "/v1/users", viewer, nil), http.StatusOK)

	// Missing permissions are denied, not silently allowed.
	env.expectStatus(env.do(http.MethodPost, "/v1/users", viewer, map[string]string{
		"email": "new@acme.test", "name": "New", "password": "dev-Passw0rd!",
	}), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodGet, "/v1/groups", viewer, nil), http.StatusForbidden)
	env.expectStatus(env.do(http.MethodGet, "/v1/roles", viewer, nil), http.StatusForbidden)
}

func TestPermissionRevocationTakesEffectImmediately(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("viewer@acme.test", "dev-Passw0rd!", model.PermUsersView)
	token, _ := env.login("viewer@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodGet, "/v1/users", token, nil), http.StatusOK)

	// Remove the user from their group. The access token is still valid and
	// unexpired: authorization must fail anyway, because permissions are
	// resolved live rather than carried in the token.
	ctx := context.Background()
	groups, err := env.app.Store.Groups().ListForUser(ctx, user.ID)
	if err != nil || len(groups) != 1 {
		t.Fatalf("list groups: %v %+v", err, groups)
	}
	if err := env.app.Store.Groups().RemoveMember(ctx, groups[0].ID, user.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	env.expectStatus(env.do(http.MethodGet, "/v1/users", token, nil), http.StatusForbidden)
}

func TestSuperuserSatisfiesEveryPermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	for _, path := range []string{"/v1/users", "/v1/groups", "/v1/roles", "/v1/permissions"} {
		env.expectStatus(env.do(http.MethodGet, path, token, nil), http.StatusOK)
	}
}

// --- users CRUD ---------------------------------------------------------------------

func TestUserCRUD(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/users", token, map[string]string{
		"email": "New@Acme.test", "name": "New User", "password": "another-Passw0rd!",
	})
	env.expectStatus(rec, http.StatusCreated)
	var created userBody
	env.decode(rec, &created)
	if created.Email != "new@acme.test" {
		t.Fatalf("email was not normalized to lower case: %q", created.Email)
	}
	// Naming no workspace means the one the administrator is standing in, so
	// the person they just created can actually sign in.
	if len(created.Workspaces) != 1 || created.Workspaces[0] != env.ws.ID {
		t.Fatalf("user was not placed in the caller's workspace: %+v", created)
	}

	// The created user can log in with the password that was set.
	env.login("new@acme.test", "another-Passw0rd!")

	// Duplicate email is a conflict, not a second user.
	env.expectStatus(env.do(http.MethodPost, "/v1/users", token, map[string]string{
		"email": "new@acme.test", "name": "Dup", "password": "another-Passw0rd!",
	}), http.StatusConflict)

	// Update.
	rec = env.do(http.MethodPut, "/v1/users/"+itoa(created.ID), token, map[string]string{
		"email": "new@acme.test", "name": "Renamed", "status": model.StatusActive,
	})
	env.expectStatus(rec, http.StatusOK)
	var updated userBody
	env.decode(rec, &updated)
	if updated.Name != "Renamed" {
		t.Fatalf("update not applied: %+v", updated)
	}

	// Delete.
	env.expectStatus(env.do(http.MethodDelete, "/v1/users/"+itoa(created.ID), token, nil), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodGet, "/v1/users/"+itoa(created.ID), token, nil), http.StatusNotFound)
}

func TestUserCreateValidation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	cases := []struct {
		name string
		body map[string]string
	}{
		{"missing email", map[string]string{"name": "X", "password": "long-enough-pass"}},
		{"invalid email", map[string]string{"email": "not-an-email", "name": "X", "password": "long-enough-pass"}},
		{"missing name", map[string]string{"email": "a@acme.test", "password": "long-enough-pass"}},
		{"short password", map[string]string{"email": "a@acme.test", "name": "X", "password": "short"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.expectStatus(env.do(http.MethodPost, "/v1/users", token, tc.body), http.StatusBadRequest)
		})
	}

	// An unknown field is rejected rather than silently ignored.
	env.expectStatus(env.do(http.MethodPost, "/v1/users", token, map[string]string{
		"email": "a@acme.test", "name": "X", "password": "long-enough-pass", "role": "admin",
	}), http.StatusBadRequest)
}

func TestIdentityFieldErrors(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// Everything wrong comes back at once, each problem on its field. A form
	// corrected one rejection at a time is a form submitted three times.
	env.expectFields(env.do(http.MethodPost, "/v1/users", token, map[string]any{
		"email": "not-an-email", "name": "", "password": "short",
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"email":    "a valid email is required",
		"name":     "a name is required",
		"password": "password must be at least 10 characters",
	})

	// A duplicate email is a conflict answered on the email field, not a
	// riddle answered on the form.
	env.expectFields(env.do(http.MethodPost, "/v1/users", token, map[string]any{
		"email": "admin@acme.test", "name": "Twin", "password": "long-enough-pass",
	}), http.StatusConflict, "conflict", map[string]string{
		"email": "another account already uses this email",
	})

	// Saving an empty membership list reads like a save and acts like a
	// lockout, so it is refused on the workspaces field.
	rec := env.do(http.MethodPost, "/v1/users", token, map[string]any{
		"email": "bob@acme.test", "name": "Bob", "password": "long-enough-pass",
	})
	env.expectStatus(rec, http.StatusCreated)
	var bob userBody
	env.decode(rec, &bob)
	env.expectFields(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID), token, map[string]any{
		"email": "bob@acme.test", "name": "Bob", "status": model.StatusActive, "workspaces": []int64{},
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"workspaces": "a person has to belong to at least one workspace, or they cannot sign in",
	})

	// A role with a blank name and an invented permission: both fields answer.
	env.expectFields(env.do(http.MethodPost, "/v1/roles", token, map[string]any{
		"name": " ", "permissions": []string{"users:invent"},
	}), http.StatusBadRequest, "invalid_request", map[string]string{
		"name":        "a name is required",
		"permissions": "unknown permission: users:invent",
	})

	// A duplicate group name lands on the name field.
	env.expectStatus(env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": "Support"}),
		http.StatusCreated)
	env.expectFields(env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": "Support"}),
		http.StatusConflict, "conflict", map[string]string{
			"name": "another group already has this name",
		})
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	adminToken, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	victim := env.createUser("victim@acme.test", "dev-Passw0rd!")
	_, victimRefresh := env.login("victim@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(victim.ID)+"/password", adminToken,
		map[string]string{"password": "brand-new-Passw0rd!"}), http.StatusNoContent)

	// The old refresh token must be dead after a password change.
	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, victimRefresh), http.StatusUnauthorized)

	// The new password works.
	env.login("victim@acme.test", "brand-new-Passw0rd!")
}

func TestDisablingUserRevokesSessions(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	adminToken, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	victim := env.createUser("victim@acme.test", "dev-Passw0rd!")
	_, victimRefresh := env.login("victim@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(victim.ID), adminToken, map[string]string{
		"email": "victim@acme.test", "name": "Victim", "status": model.StatusDisabled,
	}), http.StatusOK)

	env.expectStatus(env.doCookie(http.MethodPost, "/v1/auth/refresh", "", nil, victimRefresh), http.StatusUnauthorized)
}

func TestCannotDeleteOwnAccount(t *testing.T) {
	env := newTestEnv(t)
	admin := env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodDelete, "/v1/users/"+itoa(admin.ID), token, nil), http.StatusConflict)
	env.expectStatus(env.do(http.MethodGet, "/v1/auth/me", token, nil), http.StatusOK)
}

// TestWorkspaceIsolation proves a caller cannot read or write another
// workspace's rows even with a valid token and full permissions.
//
// People are deliberately NOT among those rows: a user belongs to the tenant,
// and which workspaces they may act in is a membership. What isolation means
// here is that the workspace's own things (its groups, roles, agents, brains)
// stay behind the token's workspace claim.
func TestWorkspaceIsolation(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	ctx := context.Background()
	other := &model.Workspace{Slug: "globex", Name: "Globex"}
	if err := env.app.Store.Workspaces().Create(ctx, other); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	foreignGroup := &model.Group{WorkspaceID: other.ID, Name: "Foreign Group"}
	if err := env.app.Store.Groups().Create(ctx, foreignGroup); err != nil {
		t.Fatalf("create foreign group: %v", err)
	}

	// Reads of a foreign row report not found, never its contents.
	env.expectStatus(env.do(http.MethodGet, "/v1/groups/"+itoa(foreignGroup.ID), token, nil), http.StatusNotFound)

	// Writes to it are refused.
	env.expectStatus(env.do(http.MethodPut, "/v1/groups/"+itoa(foreignGroup.ID), token, map[string]string{
		"name": "Hijacked",
	}), http.StatusNotFound)
	env.expectStatus(env.do(http.MethodDelete, "/v1/groups/"+itoa(foreignGroup.ID), token, nil), http.StatusNotFound)

	got, err := env.app.Store.Groups().GetByID(ctx, other.ID, foreignGroup.ID)
	if err != nil || got.Name != "Foreign Group" {
		t.Fatalf("foreign row was modified: %v %+v", err, got)
	}

	// Listing never crosses the boundary.
	rec := env.do(http.MethodGet, "/v1/groups", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var groups []groupBody
	env.decode(rec, &groups)
	for _, g := range groups {
		if g.WorkspaceID != env.ws.ID {
			t.Fatalf("listing leaked a foreign group: %+v", g)
		}
	}

	// And the token cannot be pointed at that workspace: the switch checks
	// membership, which possession of a valid token does not confer.
	env.expectStatus(env.do(http.MethodPost, "/v1/auth/workspace", token, map[string]any{
		"workspace_id": other.ID,
	}), http.StatusForbidden)
}

// --- groups and roles -------------------------------------------------------------------

func TestGroupAndRoleCRUDGrantsPermissions(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A user with no permissions at all.
	member := env.createUser("member@acme.test", "dev-Passw0rd!")
	memberToken, _ := env.login("member@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodGet, "/v1/users", memberToken, nil), http.StatusForbidden)

	// Build the grant chain through the API: role, group, assignment,
	// membership.
	rec := env.do(http.MethodPost, "/v1/roles", token, map[string]any{
		"name": "Viewer", "permissions": []string{model.PermUsersView},
	})
	env.expectStatus(rec, http.StatusCreated)
	var role roleBody
	env.decode(rec, &role)

	rec = env.do(http.MethodPost, "/v1/groups", token, map[string]string{"name": "Viewers"})
	env.expectStatus(rec, http.StatusCreated)
	var group groupBody
	env.decode(rec, &group)

	env.expectStatus(env.do(http.MethodPut,
		"/v1/groups/"+itoa(group.ID)+"/roles/"+itoa(role.ID), token, nil), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodPut,
		"/v1/groups/"+itoa(group.ID)+"/members/"+itoa(member.ID), token, nil), http.StatusNoContent)

	// The member now passes the permission check with their existing token.
	env.expectStatus(env.do(http.MethodGet, "/v1/users", memberToken, nil), http.StatusOK)

	// Removing the role from the group revokes it again.
	env.expectStatus(env.do(http.MethodDelete,
		"/v1/groups/"+itoa(group.ID)+"/roles/"+itoa(role.ID), token, nil), http.StatusNoContent)
	env.expectStatus(env.do(http.MethodGet, "/v1/users", memberToken, nil), http.StatusForbidden)
}

func TestRoleRejectsUnknownPermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A typo must be an error: a role holding a permission nothing checks
	// is a silent security hole.
	env.expectStatus(env.do(http.MethodPost, "/v1/roles", token, map[string]any{
		"name": "Typo", "permissions": []string{"users:veiw"},
	}), http.StatusBadRequest)
}

// A person belongs to one or many groups, and the user form says which in one
// save: the list becomes exactly what was sent, within the caller's workspace.
func TestUserGroupsAreSetWholesale(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/users", token, map[string]any{
		"email": "bob@acme.test", "name": "Bob", "password": "long-enough-pass",
	})
	env.expectStatus(rec, http.StatusCreated)
	var bob userBody
	env.decode(rec, &bob)

	groupID := func(name string) int64 {
		rec := env.do(http.MethodPost, "/v1/groups", token, map[string]any{"name": name})
		env.expectStatus(rec, http.StatusCreated)
		var g groupBody
		env.decode(rec, &g)
		return g.ID
	}
	sales, support := groupID("Sales"), groupID("Support")

	memberships := func() []int64 {
		rec := env.do(http.MethodGet, "/v1/users/"+itoa(bob.ID)+"/groups", token, nil)
		env.expectStatus(rec, http.StatusOK)
		var groups []groupBody
		env.decode(rec, &groups)
		ids := make([]int64, 0, len(groups))
		for _, g := range groups {
			ids = append(ids, g.ID)
		}
		return ids
	}

	// One save, many groups.
	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID)+"/groups", token,
		map[string]any{"groups": []int64{sales, support}}), http.StatusNoContent)
	if got := memberships(); len(got) != 2 {
		t.Fatalf("expected both groups, got %v", got)
	}

	// A shorter list removes what it no longer names.
	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID)+"/groups", token,
		map[string]any{"groups": []int64{support}}), http.StatusNoContent)
	if got := memberships(); len(got) != 1 || got[0] != support {
		t.Fatalf("expected only support, got %v", got)
	}

	// The empty list is a valid answer: a person in no group at all.
	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID)+"/groups", token,
		map[string]any{"groups": []int64{}}), http.StatusNoContent)
	if got := memberships(); len(got) != 0 {
		t.Fatalf("expected no groups, got %v", got)
	}

	// A group that does not exist in this workspace refuses the save.
	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID)+"/groups", token,
		map[string]any{"groups": []int64{999999}}), http.StatusNotFound)

	// Rewriting memberships is the group editor's power, not the user viewer's.
	env.createUser("viewer@acme.test", "dev-Passw0rd!", model.PermUsersView)
	viewer, _ := env.login("viewer@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodPut, "/v1/users/"+itoa(bob.ID)+"/groups", viewer,
		map[string]any{"groups": []int64{sales}}), http.StatusForbidden)
}

func TestDeletingRoleRevokesItsGrants(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	env.createUser("member@acme.test", "dev-Passw0rd!", model.PermUsersView)
	memberToken, _ := env.login("member@acme.test", "dev-Passw0rd!")
	env.expectStatus(env.do(http.MethodGet, "/v1/users", memberToken, nil), http.StatusOK)

	rec := env.do(http.MethodGet, "/v1/roles", token, nil)
	env.expectStatus(rec, http.StatusOK)
	// The roles screen answers with the roles AND the permission catalogue a
	// role can hold, because a screen asks one question (KB/19).
	var listed rolesBody
	env.decode(rec, &listed)
	if len(listed.Permissions) == 0 {
		t.Fatal("the roles answer carries no permission catalogue")
	}

	var target int64
	for _, r := range listed.Roles {
		if r.Name == "role-member@acme.test" {
			target = r.ID
		}
	}
	if target == 0 {
		t.Fatalf("role not found in %+v", listed.Roles)
	}
	env.expectStatus(env.do(http.MethodDelete, "/v1/roles/"+itoa(target), token, nil), http.StatusNoContent)

	// The grant is gone immediately.
	env.expectStatus(env.do(http.MethodGet, "/v1/users", memberToken, nil), http.StatusForbidden)
}

func TestInvalidIdsAreRejected(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	env.expectStatus(env.do(http.MethodGet, "/v1/users/999999", token, nil), http.StatusNotFound)
	env.expectStatus(env.do(http.MethodGet, "/v1/users/not-a-number", token, nil), http.StatusBadRequest)
	env.expectStatus(env.do(http.MethodGet, "/v1/groups/0", token, nil), http.StatusBadRequest)
	env.expectStatus(env.do(http.MethodGet, "/v1/groups/-1", token, nil), http.StatusBadRequest)
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
