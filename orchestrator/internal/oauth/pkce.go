package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// verifyPKCES256 checks code_verifier against the stored S256 challenge.
// Only S256 exists in SAG ("plain" is rejected at the authorize endpoint,
// per OAuth 2.1). Comparison is constant-time.
func verifyPKCES256(verifier, challenge string) bool {
	if !validVerifier(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// validVerifier enforces RFC 7636 §4.1: 43–128 chars of [A-Za-z0-9-._~].
func validVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.' || c == '_' || c == '~':
		default:
			return false
		}
	}
	return true
}
