package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// ErrInvalidCredentials is returned for every failed login reason (unknown
// user, disabled user, wrong password) so the API cannot be used to probe
// which of them is the case.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrNoWorkspace is what a person hits when their password is right and they
// belong to nowhere. It is deliberately NOT ErrInvalidCredentials: telling them
// to check their password would send them somewhere there is nothing to fix.
var ErrNoWorkspace = errors.New("no workspace")

// ErrNotMember is a switch to a workspace the person does not belong to. The
// token they hold is real, which is exactly why this has to be checked.
var ErrNotMember = errors.New("not a member of that workspace")

// ErrInvalidRefresh covers unknown, expired, revoked, and already-rotated
// refresh tokens.
var ErrInvalidRefresh = errors.New("invalid refresh token")

type LoginResult struct {
	User      *model.User
	Workspace *model.Workspace
	// Workspaces is every workspace this person may enter, resolved as the
	// tokens are issued so the client learns the choice without a second
	// request. The current one (Workspace) is the token's claim; this is what a
	// switcher offers.
	Workspaces            []*model.Workspace
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

type LoginRequest struct {
	Email     string
	Password  string
	UserAgent string
	IP        string
}

// Login identifies the person, then decides where they are standing.
//
// The workspace is not asked for, because a person is not asked to remember one:
// they are a member of some, and the one they used last is the one they meant.
// If that membership is gone, or they never chose, the first workspace they
// belong to answers, and the sidebar lets them move.

func (a *App) Login(ctx context.Context, req LoginRequest) (*LoginResult, error) {
	user, err := a.Authenticate(ctx, req.Email, req.Password)
	if err != nil {
		return nil, err
	}
	ws, err := a.ActiveWorkspace(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	return a.issueTokens(ctx, user, ws, req.UserAgent, req.IP, "")
}

// Authenticate is the password check on its own, shared by the API login and
// the consent screen the OAuth flow puts in front of a browser.
func (a *App) Authenticate(ctx context.Context, email, password string) (*model.User, error) {
	user, err := a.Store.Users().GetByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		// Verify against nothing anyway: an unknown email must cost the same
		// as a known one, or the timing answers the question we refused to.
		auth.VerifyPassword("", password)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user.Status != model.StatusActive || !auth.VerifyPassword(user.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	return user, nil
}

// ActiveWorkspace resolves where a person is standing: the one they last
// switched to if they are still a member of it, otherwise the first they belong
// to. A stale preference is not an error, it is simply out of date.
func (a *App) ActiveWorkspace(ctx context.Context, userID int64) (*model.Workspace, error) {
	memberships, err := a.Store.Workspaces().ListForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	if len(memberships) == 0 {
		return nil, ErrNoWorkspace
	}

	last, err := a.Store.Settings().Get(ctx, userID, model.SettingWorkspace)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read workspace setting: %w", err)
	}
	if id, convErr := strconv.ParseInt(last, 10, 64); convErr == nil {
		for _, ws := range memberships {
			if ws.ID == id {
				return ws, nil
			}
		}
	}
	return memberships[0], nil
}

// SwitchWorkspace moves a person to another workspace they belong to.
//
// It is an authorization, not a preference: the membership is checked here, and
// what comes back is a NEW token pair carrying the new workspace. The old
// refresh token is revoked with it, so a switch leaves nothing behind that could
// still speak for the workspace they left.
func (a *App) SwitchWorkspace(ctx context.Context, userID, workspaceID int64, refreshToken, userAgent, ip string) (*LoginResult, error) {
	member, err := a.Store.Workspaces().IsMember(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotMember
	}
	ws, err := a.Store.Workspaces().GetByID(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	user, err := a.Store.Users().GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if err := a.Store.Settings().Set(ctx, userID, model.SettingWorkspace, strconv.FormatInt(workspaceID, 10)); err != nil {
		return nil, fmt.Errorf("remember workspace: %w", err)
	}
	// Issue the new pair FIRST, then revoke the old session. If issuing fails, the
	// old session is untouched, so a failed switch leaves the person working
	// rather than stranded (approval must equal success). Revoking after is
	// best-effort: if it fails, an extra session lingers until it expires, which
	// nobody notices, and the new session is already the live one.
	result, err := a.issueTokens(ctx, user, ws, userAgent, ip, "")
	if err != nil {
		return nil, err
	}
	if refreshToken != "" {
		if err := a.Logout(ctx, refreshToken); err != nil {
			a.Log.Warn().Err(err).Int64("user_id", userID).Msg("could not revoke the previous session after a workspace switch")
		}
	}
	return result, nil
}

// Refresh rotates the presented token: the old one is consumed and a new
// pair is issued. Replaying a consumed token fails, and the session it
// came from is already marked used, so a stolen token has a single-use
// window at most.
func (a *App) Refresh(ctx context.Context, refreshToken, userAgent, ip string) (*LoginResult, error) {
	session, err := a.Store.Sessions().GetByHash(ctx, auth.HashRefreshToken(refreshToken))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidRefresh
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if session.Revoked || time.Now().UTC().After(session.ExpiresAt) {
		return nil, ErrInvalidRefresh
	}
	if session.Used {
		// The token was already rotated and is being presented again. That is
		// USUALLY theft, and the answer is to revoke the whole family, severing
		// the attacker's chain and the victim's alike (OAuth 2.0 BCP reuse
		// detection). The concurrent double-refresh is a different path: there
		// both racers read used=0 and the atomic MarkUsed below rejects the
		// loser, so this fires only on a later replay.
		//
		// But it is not ALWAYS theft, and the exception has teeth: we can spend
		// a token and the client can never receive its replacement. The response
		// dies in flight, the process is killed mid-write, a phone loses signal.
		// The browser still holds the old cookie, presents it, and is treated as
		// an attacker for a failure that was ours — permanently, because the
		// family is gone by the time anybody notices.
		//
		// So a replay moments after the rotation, FROM THE SAME BROWSER, is read
		// as the retry it almost certainly is: the successor nobody received is
		// revoked, and a fresh token takes its place in the same family. Outside
		// that window, or from anywhere else, it is theft and treated as theft.
		if a.refreshRetry(session, userAgent) {
			// One recovery per spent token, claimed atomically. Without this the
			// grace is unbounded: eight tabs racing one cookie would each read
			// "used, and recently" and each be handed a session.
			claimed, err := a.Store.Sessions().ClaimReuseGrace(ctx, session.ID,
				time.Now().UTC().Add(-a.Config.RefreshReuseGrace))
			if err != nil {
				return nil, err
			}
			if !claimed {
				return nil, ErrInvalidRefresh
			}
			a.Log.Warn().Int64("user_id", session.UserID).Str("family", session.FamilyID).
				Msg("refresh token replayed within the grace window; treating as a lost rotation")
			if err := a.Store.Sessions().RevokeUnusedInFamily(ctx, session.FamilyID); err != nil {
				return nil, fmt.Errorf("revoke orphaned successor: %w", err)
			}
			user, err := a.Store.Users().GetByID(ctx, session.UserID)
			if err != nil || user.Status != model.StatusActive {
				return nil, ErrInvalidRefresh
			}
			member, err := a.Store.Workspaces().IsMember(ctx, session.WorkspaceID, user.ID)
			if err != nil || !member {
				return nil, ErrInvalidRefresh
			}
			ws, err := a.Store.Workspaces().GetByID(ctx, session.WorkspaceID)
			if err != nil {
				return nil, fmt.Errorf("load workspace: %w", err)
			}
			return a.issueTokens(ctx, user, ws, userAgent, ip, session.FamilyID)
		}
		if err := a.Store.Sessions().RevokeFamily(ctx, session.FamilyID); err != nil {
			return nil, fmt.Errorf("revoke reused session family: %w", err)
		}
		return nil, ErrInvalidRefresh
	}

	user, err := a.Store.Users().GetByID(ctx, session.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidRefresh
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	// A disabled or deleted user cannot refresh their way back in.
	if user.Status != model.StatusActive {
		return nil, ErrInvalidRefresh
	}

	// The session belongs to a workspace, and a refresh must not move anyone.
	// If the membership behind it is gone, so is the session: they log in again
	// and land somewhere they are still allowed to be.
	member, err := a.Store.Workspaces().IsMember(ctx, session.WorkspaceID, user.ID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrInvalidRefresh
	}
	ws, err := a.Store.Workspaces().GetByID(ctx, session.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}

	// Consume the token atomically. This, not the session.Used read above, is the
	// authoritative single-use gate: if a concurrent refresh already consumed the
	// same token, MarkUsed reports ErrNotFound and we refuse, so one refresh token
	// can never be redeemed into two sessions (the earlier check is only a fast
	// path that spares the user and membership lookups for an obviously dead token).
	if err := a.Store.Sessions().MarkUsed(ctx, session.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("consume session: %w", err)
	}
	return a.issueTokens(ctx, user, ws, userAgent, ip, session.FamilyID)
}

// refreshRetry reports whether a replayed token looks like a client that never
// received its replacement rather than an attacker holding a stolen one.
//
// Two conditions, and both matter. WITHIN THE WINDOW, because a lost rotation is
// noticed on the very next request and a stolen token is used whenever the thief
// gets round to it. FROM THE SAME BROWSER, because the user agent is recorded
// when the token is issued, and a thief replaying from their own machine does
// not match it. Neither is proof; together they are the difference between an
// accident we caused and an attack, and the window is short enough that the
// worst case is a few seconds in which a stolen token also works.
//
// A zero window turns this off and restores strict reuse detection.
func (a *App) refreshRetry(session *model.UserSession, userAgent string) bool {
	grace := a.Config.RefreshReuseGrace
	if grace <= 0 || session.UsedAt == nil {
		return false
	}
	if time.Since(*session.UsedAt) > grace {
		return false
	}
	return session.UserAgent == userAgent
}

// Logout revokes the session behind the presented refresh token. Unknown
// tokens are a no-op: logout never reveals whether a token was real.
func (a *App) Logout(ctx context.Context, refreshToken string) error {
	session, err := a.Store.Sessions().GetByHash(ctx, auth.HashRefreshToken(refreshToken))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	return a.Store.Sessions().Revoke(ctx, session.ID)
}

// refreshTTL is how long a session survives without being used.
//
// Thirty days on a deployment, because a laptop left on a train should
// eventually stop being a way in.
//
// On a personal installation it is effectively forever, and that is the honest
// setting rather than a lax one. There is nothing an expiry protects here: the
// credential is possession of this computer's own files, which an expiry does
// not change, and the sign-in it would force asks for nothing. What it WOULD do
// is meet somebody a month after they installed the thing with a login box, for
// an account they never made and a password that does not exist.
func (a *App) refreshTTL() time.Duration {
	if a.Config.Personal {
		return personalSessionTTL
	}
	return auth.RefreshTokenTTL
}

// A century. Not "no expiry" as a special case: an absent or zero expiry would
// have to be understood by every piece of code that reads one, and a date
// nobody alive will see does the same job without inventing a rule.
const personalSessionTTL = 100 * 365 * 24 * time.Hour

func (a *App) issueTokens(ctx context.Context, user *model.User, ws *model.Workspace, userAgent, ip, familyID string) (*LoginResult, error) {
	refreshToken, refreshHash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, err
	}
	// A login or switch (empty familyID) starts a new rotation family; a refresh
	// passes the consumed session's family, so the whole chain stays linked for
	// reuse detection.
	if familyID == "" {
		familyID, err = auth.NewFamilyID()
		if err != nil {
			return nil, err
		}
	}
	refreshExpiry := time.Now().UTC().Add(a.refreshTTL())
	// The session is created first, so the access token can carry its id and a
	// request can re-check live that the session is still good (not revoked by a
	// logout, password change, or disable).
	session := &model.UserSession{
		TokenHash:   refreshHash,
		FamilyID:    familyID,
		UserID:      user.ID,
		WorkspaceID: ws.ID,
		UserAgent:   userAgent,
		IP:          ip,
		ExpiresAt:   refreshExpiry,
	}
	if err := a.Store.Sessions().Create(ctx, session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	accessToken, accessExpiry, err := a.Tokens.IssueAccessToken(user.ID, ws.ID, session.ID)
	if err != nil {
		return nil, err
	}
	workspaces, err := a.Store.Workspaces().ListForUser(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return &LoginResult{
		User:                  user,
		Workspace:             ws,
		Workspaces:            workspaces,
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiry,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiry,
	}, nil
}

// Authorize resolves the caller's live permissions and reports whether they
// satisfy the requirement. Permissions are never read from a token: a
// revoked role takes effect on the next request, not at token expiry.
func (a *App) Authorize(ctx context.Context, userID int64, required string) (bool, error) {
	perms, err := a.Store.Users().EffectivePermissions(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("resolve permissions: %w", err)
	}
	return model.HasPermission(perms, required), nil
}
