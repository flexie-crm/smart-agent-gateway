package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// First-party API auth: a short-lived JWT access token plus a long-lived
// refresh token stored (hashed) as a row, so logout and revocation are
// real. This is separate from the OAuth server, which exists for
// third-party and MCP clients.
const (
	AccessTokenTTL = 15 * time.Minute
	// LinkTokenTTL is how long the chat application's Rust half may hold its
	// credential before asking the page for another. Longer than an access
	// token because renewing it costs a round trip through the window; short
	// enough that a session ended while nobody was looking stops mattering
	// within the hour.
	LinkTokenTTL    = time.Hour
	RefreshTokenTTL = 30 * 24 * time.Hour

	refreshTokenPrefix = "sag_ut_"
	tokenIssuer        = "flexie-sag"
)

var ErrInvalidToken = errors.New("invalid token")

// Claims carries the identity every request is authorized against. It
// deliberately does NOT carry permissions: those are resolved live from
// the database on each request, so a permission change takes effect at
// once instead of at token expiry.
type Claims struct {
	UserID      int64 `json:"uid"`
	WorkspaceID int64 `json:"ws"`
	// SessionID ties the access token to the refresh session it was issued with,
	// so a request can re-check live that the session is still good (not revoked
	// by a logout, password change, or disable) rather than trusting the token for
	// its whole life.
	SessionID int64 `json:"sid"`
	// DeviceID names the installation this token was minted for, and is set
	// only on a link token.
	//
	// A person may be signed in on a laptop and a desktop at once, and a tool
	// that reaches their own network has to run on the one they are SITTING at.
	// Guessing (the most recently connected, say) is a wrong answer half the
	// time and an unpredictable one always; the request knows which application
	// it came from, so the answer travels with it from here to the tool call.
	DeviceID string `json:"did,omitempty"`
	// Audience says what this token is FOR. An access token has none and opens
	// the API; a link token says "link" and opens exactly one socket, so the
	// credential the chat application's Rust half holds cannot be spent on
	// anything else if it leaks off that computer.
	Audience string `json:"aud,omitempty"`
	jwt.RegisteredClaims
}

// AudienceLink marks a token minted for the machine link and nothing else.
const AudienceLink = "link"

type TokenService struct {
	secret []byte
}

func NewTokenService(secret []byte) *TokenService {
	return &TokenService{secret: secret}
}

func (s *TokenService) IssueAccessToken(userID, workspaceID, sessionID int64) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(AccessTokenTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		SessionID:   sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	})
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// IssueLinkToken mints the credential the chat application's Rust half holds:
// the same identity, a longer life than an access token because it is renewed
// by asking the page rather than by a refresh cookie, and an audience that
// opens one socket.
//
// It carries the session id like an access token does, so the link is re-checked
// against a live session every time it is opened or renewed: a person who was
// signed out or disabled loses their machine link within the hour rather than
// whenever their socket happens to break.
func (s *TokenService) IssueLinkToken(userID, workspaceID, sessionID int64, deviceID string) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(LinkTokenTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		SessionID:   sessionID,
		DeviceID:    deviceID,
		Audience:    AudienceLink,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	})
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign link token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseLinkToken verifies a link token and refuses anything that is not one:
// an access token presented here opens nothing, and neither does a link token
// presented to the API.
func (s *TokenService) ParseLinkToken(raw string) (*Claims, error) {
	claims, err := s.parse(raw)
	if err != nil {
		return nil, err
	}
	if claims.Audience != AudienceLink {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// ParseAccessToken verifies the signature, the algorithm, the issuer, and
// the expiry. An unsigned or differently-signed token is always rejected.
//
// And so is a token minted for something else. Audience is checked HERE rather
// than only where a narrower token is expected, because the direction that
// matters is this one: a credential handed to the chat application to open one
// socket must not also open the API, and a check that only ran on the link side
// would leave that true. A test caught exactly that.
func (s *TokenService) ParseAccessToken(raw string) (*Claims, error) {
	claims, err := s.parse(raw)
	if err != nil {
		return nil, err
	}
	if claims.Audience != "" {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// parse verifies a token of any audience: the signature, the algorithm, the
// issuer and the expiry. What it is FOR is the caller's question.
func (s *TokenService) parse(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if claims.UserID == 0 || claims.WorkspaceID == 0 {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// NewRefreshToken returns the token to hand to the client and the hash to
// store. The plain token never touches the database.
func NewRefreshToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	token = refreshTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashRefreshToken(token), nil
}

func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewFamilyID returns a random identifier that ties a rotation chain of refresh
// sessions together, so that reuse of a spent token can revoke the whole family.
func NewFamilyID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
