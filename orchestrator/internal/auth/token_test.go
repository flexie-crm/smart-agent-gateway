package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestAccessTokenRoundTrip(t *testing.T) {
	svc := NewTokenService(testSecret)
	token, expiresAt, err := svc.IssueAccessToken(42, 7, 99)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !expiresAt.After(time.Now().UTC()) {
		t.Fatalf("expiry is not in the future: %v", expiresAt)
	}

	claims, err := svc.ParseAccessToken(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.UserID != 42 || claims.WorkspaceID != 7 {
		t.Fatalf("wrong claims: %+v", claims)
	}
}

func TestAccessTokenRejectsForeignSecret(t *testing.T) {
	token, _, err := NewTokenService(testSecret).IssueAccessToken(1, 1, 1)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	other := NewTokenService([]byte("fedcba9876543210fedcba9876543210"))
	if _, err := other.ParseAccessToken(token); err == nil {
		t.Fatal("token signed with a different secret accepted")
	}
}

func TestAccessTokenRejectsExpired(t *testing.T) {
	svc := NewTokenService(testSecret)
	// Sign an already-expired token directly: the service never issues one,
	// but a client can present one.
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	})
	raw, err := expired.SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := svc.ParseAccessToken(raw); err == nil {
		t.Fatal("expired token accepted")
	}
}

// A token with alg=none must never be trusted: this is the classic JWT
// downgrade attack.
func TestAccessTokenRejectsNoneAlgorithm(t *testing.T) {
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := NewTokenService(testSecret).ParseAccessToken(raw); err == nil {
		t.Fatal("alg=none token accepted")
	}
}

func TestAccessTokenRejectsMissingExpiryAndIssuer(t *testing.T) {
	svc := NewTokenService(testSecret)

	noExpiry := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: tokenIssuer},
	})
	raw, err := noExpiry.SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := svc.ParseAccessToken(raw); err == nil {
		t.Fatal("token without expiry accepted")
	}

	foreignIssuer := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: 1, WorkspaceID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "someone-else",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	if raw, err = foreignIssuer.SignedString(testSecret); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := svc.ParseAccessToken(raw); err == nil {
		t.Fatal("token from a foreign issuer accepted")
	}
}

func TestAccessTokenRejectsMissingIdentity(t *testing.T) {
	svc := NewTokenService(testSecret)
	empty := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	raw, err := empty.SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := svc.ParseAccessToken(raw); err == nil {
		t.Fatal("token without an identity accepted")
	}
}

func TestAccessTokenRejectsGarbage(t *testing.T) {
	svc := NewTokenService(testSecret)
	for _, raw := range []string{"", "not-a-token", "a.b.c", strings.Repeat("x", 200)} {
		if _, err := svc.ParseAccessToken(raw); err == nil {
			t.Fatalf("garbage token %q accepted", raw)
		}
	}
}

func TestRefreshTokenIsHashedAndUnique(t *testing.T) {
	token1, hash1, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	token2, hash2, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}

	if !strings.HasPrefix(token1, refreshTokenPrefix) {
		t.Fatalf("missing product prefix: %q", token1)
	}
	if token1 == token2 || hash1 == hash2 {
		t.Fatal("refresh tokens are not unique")
	}
	if strings.Contains(hash1, token1) || hash1 == token1 {
		t.Fatal("stored hash reveals the token")
	}
	if HashRefreshToken(token1) != hash1 {
		t.Fatal("hash is not deterministic")
	}
	if HashRefreshToken(token2) == hash1 {
		t.Fatal("hash collision between distinct tokens")
	}
}
