package api

import (
	"context"
	"net/http"
	"strings"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
)

type contextKey int

const claimsKey contextKey = iota

// requireAuth validates the bearer access token and pins the caller's
// identity into the request context. Every /v1 route below it is
// authenticated.
func requireAuth(a *app.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			claims, err := a.Tokens.ParseAccessToken(raw)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
				return
			}
			// Token possession is never sufficient: re-check live that the session
			// behind the token is still good (not revoked, the user active, the
			// membership intact), so a logout, disable, password change, or removal
			// takes effect on the next request instead of at token expiry.
			ok, err := a.Store.Sessions().ValidateAccess(r.Context(), claims.SessionID, claims.UserID, claims.WorkspaceID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "could not verify the session")
				return
			}
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "this session is no longer valid")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
		})
	}
}

// requirePermission resolves the caller's permissions live from the store on
// every request: a revoked role must take effect immediately, never at
// token expiry.
func requirePermission(a *app.App, permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := claimsFrom(r)
			if claims == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			allowed, err := a.Authorize(r.Context(), claims.UserID, permission)
			if err != nil {
				a.Log.Error().Err(err).Msg("authorize")
				writeError(w, http.StatusInternalServerError, "server_error", "temporary failure")
				return
			}
			if !allowed {
				writeError(w, http.StatusForbidden, "forbidden", "permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func claimsFrom(r *http.Request) *auth.Claims {
	claims, _ := r.Context().Value(claimsKey).(*auth.Claims)
	return claims
}
