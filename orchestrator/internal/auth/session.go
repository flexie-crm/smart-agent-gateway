package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session is the browser session used by the OAuth authorize flow (login +
// consent). It is a compact HMAC-signed value: payload.signature, both
// base64url. Stateless by design; server restarts only invalidate it when
// the secret rotates.
type Session struct {
	UserID      int64 `json:"uid"`
	WorkspaceID int64 `json:"ws"`
	ExpiresAt   int64 `json:"exp"`
}

type SessionManager struct {
	secret []byte
}

func NewSessionManager(secret []byte) *SessionManager {
	return &SessionManager{secret: secret}
}

func (m *SessionManager) Issue(userID, workspaceID int64, ttl time.Duration) (string, error) {
	payload, err := json.Marshal(Session{
		UserID:      userID,
		WorkspaceID: workspaceID,
		ExpiresAt:   time.Now().UTC().Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal session: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + m.sign(encoded), nil
}

var errInvalidSession = errors.New("invalid session")

func (m *SessionManager) Verify(value string) (*Session, error) {
	encoded, sig, found := strings.Cut(value, ".")
	if !found || !hmac.Equal([]byte(m.sign(encoded)), []byte(sig)) {
		return nil, errInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errInvalidSession
	}
	s := &Session{}
	if err := json.Unmarshal(payload, s); err != nil {
		return nil, errInvalidSession
	}
	if time.Now().UTC().Unix() >= s.ExpiresAt {
		return nil, errInvalidSession
	}
	return s, nil
}

func (m *SessionManager) sign(encoded string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
