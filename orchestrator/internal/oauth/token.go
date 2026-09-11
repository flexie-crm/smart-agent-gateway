// Package oauth is SAG's OAuth 2.1 authorization server core, a faithful
// Go port of the Flexie CRM OauthBundle logic (CRM KB/32): opaque tokens
// stored only as SHA-256 hashes, bcrypt client secrets, PKCE S256
// mandatory, refresh rotation with family-wide reuse revocation. Serves the
// MCP surface first; the REST API can adopt it later. Full design +
// divergences: KB/07-oauth-service.md.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Token prefixes make leaked credentials identifiable in logs and secret
// scanners without revealing anything about their contents.
const (
	prefixAccessToken  = "sag_at_"
	prefixRefreshToken = "sag_rt_"
	prefixAuthCode     = "sag_ac_"
	prefixClientID     = "sag_ci_"
	prefixClientSecret = "sag_cs_"
)

// newSecret returns prefix + base64url(32 random bytes), 256 bits of
// entropy from crypto/rand.
func newSecret(prefix string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashToken is the only form a token is ever persisted in.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newFamilyID mints a refresh-family lineage id (128-bit hex).
func newFamilyID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
