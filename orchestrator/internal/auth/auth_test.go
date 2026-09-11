package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password stored in clear")
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword(hash, "") {
		t.Fatal("empty password accepted")
	}
	if VerifyPassword("not-a-bcrypt-hash", "anything") {
		t.Fatal("garbage hash verified")
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	h1, err := HashPassword("same input")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h2, err := HashPassword("same input")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical, salting is broken")
	}
}

func TestSessionIssueAndVerify(t *testing.T) {
	m := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"))
	value, err := m.Issue(42, 7, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s, err := m.Verify(value)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if s.UserID != 42 || s.WorkspaceID != 7 {
		t.Fatalf("wrong identity: %+v", s)
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	m := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"))
	value, err := m.Issue(1, 1, -time.Second)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := m.Verify(value); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	m := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"))
	value, err := m.Issue(1, 1, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	payload, sig, _ := strings.Cut(value, ".")

	// Payload swapped for a forged identity keeps the old signature.
	forged, err := m.Issue(999, 999, time.Hour)
	if err != nil {
		t.Fatalf("issue forged: %v", err)
	}
	forgedPayload, _, _ := strings.Cut(forged, ".")
	if _, err := m.Verify(forgedPayload + "." + sig); err == nil {
		t.Fatal("payload swap accepted")
	}

	// Flipped signature byte.
	if _, err := m.Verify(payload + "." + sig[:len(sig)-2] + "xx"); err == nil {
		t.Fatal("tampered signature accepted")
	}
	// Structural garbage.
	for _, bad := range []string{"", ".", payload, "a.b.c", payload + "."} {
		if _, err := m.Verify(bad); err == nil {
			t.Fatalf("malformed session %q accepted", bad)
		}
	}
}

func TestSessionRejectsForeignSecret(t *testing.T) {
	m1 := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"))
	m2 := NewSessionManager([]byte("fedcba9876543210fedcba9876543210"))
	value, err := m1.Issue(1, 1, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := m2.Verify(value); err == nil {
		t.Fatal("session signed with a different secret accepted")
	}
}
