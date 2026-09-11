package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const (
	keyHexA = "0101010101010101010101010101010101010101010101010101010101010101"
	keyHexB = "0202020202020202020202020202020202020202020202020202020202020202"
)

func mustKeyring(t *testing.T, spec, primary string) *Keyring {
	t.Helper()
	k, err := ParseKeyring(spec, primary)
	if err != nil {
		t.Fatalf("parse keyring: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	secret := []byte("sk-ant-super-secret-api-key")

	sealed, err := k.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("sealed value contains the plaintext")
	}

	opened, err := k.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("round trip mismatch: %q", opened)
	}
}

// Sealing the same secret twice must produce different ciphertexts, or an
// observer could tell that two workspaces use the same API key.
func TestSealIsNonDeterministic(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	secret := []byte("same-secret")

	first, err := k.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	second, err := k.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two seals of the same secret are identical")
	}

	// Both still open to the same plaintext.
	for _, sealed := range [][]byte{first, second} {
		opened, err := k.Open(sealed)
		if err != nil || !bytes.Equal(opened, secret) {
			t.Fatalf("open: %v %q", err, opened)
		}
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	sealed, err := k.Seal([]byte("secret-value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Flipping any single byte must break authentication, not silently
	// decrypt to something else.
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if _, err := k.Open(tampered); err == nil {
			t.Fatalf("tampering at byte %d was accepted", i)
		}
	}
}

func TestOpenRejectsTruncation(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	sealed, err := k.Seal([]byte("secret-value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for cut := 0; cut < len(sealed); cut++ {
		if _, err := k.Open(sealed[:cut]); err == nil {
			t.Fatalf("truncated value of length %d was accepted", cut)
		}
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	sealer := mustKeyring(t, "1:"+keyHexA, "1")
	sealed, err := sealer.Seal([]byte("secret-value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Same key id, different key material: the wrap must fail to open.
	impostor := mustKeyring(t, "1:"+keyHexB, "1")
	if _, err := impostor.Open(sealed); err == nil {
		t.Fatal("a different key with the same id opened the value")
	}

	// Key id absent from the keyring.
	other := mustKeyring(t, "9:"+keyHexB, "9")
	if _, err := other.Open(sealed); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey, got %v", err)
	}
}

func TestOpenRejectsBadVersion(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	sealed, err := k.Seal([]byte("secret-value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	sealed[0] = 0xFF
	if _, err := k.Open(sealed); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("expected ErrBadVersion, got %v", err)
	}
}

// Rotation: a value sealed with the old key still opens after the primary
// key changes, and is reported as needing a rewrap.
func TestKeyRotation(t *testing.T) {
	old := mustKeyring(t, "1:"+keyHexA, "1")
	secret := []byte("secret-value")
	sealed, err := old.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	rotated := mustKeyring(t, "1:"+keyHexA+",2:"+keyHexB, "2")

	opened, err := rotated.Open(sealed)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("value sealed before rotation must still open: %v %q", err, opened)
	}
	if !rotated.NeedsRewrap(sealed) {
		t.Fatal("value sealed with a retired key must be reported for rewrap")
	}

	resealed, err := rotated.Seal(secret)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if rotated.NeedsRewrap(resealed) {
		t.Fatal("value sealed with the primary key must not need a rewrap")
	}
	// And the old keyring can no longer open what the new primary sealed.
	if _, err := old.Open(resealed); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("expected ErrUnknownKey from the old keyring, got %v", err)
	}
}

func TestKeyringValidation(t *testing.T) {
	shortKey := hex.EncodeToString(make([]byte, 16))

	cases := []struct {
		name    string
		spec    string
		primary string
	}{
		{"empty spec", "", "1"},
		{"no primary id", "1:" + keyHexA, ""},
		{"primary not in keyring", "1:" + keyHexA, "2"},
		{"key too short", "1:" + shortKey, "1"},
		{"not hex", "1:zzzz", "1"},
		{"missing separator", keyHexA, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseKeyring(tc.spec, tc.primary); err == nil {
				t.Fatal("invalid key configuration was accepted")
			}
		})
	}
}

func TestParseKeyringAcceptsMultipleKeys(t *testing.T) {
	k := mustKeyring(t, " 1:"+keyHexA+" , 2:"+keyHexB+" ", "2")
	if k.PrimaryID() != "2" {
		t.Fatalf("wrong primary: %q", k.PrimaryID())
	}
	sealed, err := k.Seal([]byte("x"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	id, err := sealedKeyID(sealed)
	if err != nil || id != "2" {
		t.Fatalf("sealed with the wrong key: %q %v", id, err)
	}
}

// A long secret must survive the round trip: the format carries the
// ciphertext length implicitly, so an off-by-one in the layout would show
// up here.
func TestSealHandlesLargeSecret(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	secret := []byte(strings.Repeat("abcdefgh", 512))
	sealed, err := k.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := k.Open(sealed)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("large secret round trip failed: %v", err)
	}
}

func TestSealHandlesEmptySecret(t *testing.T) {
	k := mustKeyring(t, "1:"+keyHexA, "1")
	sealed, err := k.Seal(nil)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := k.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(opened) != 0 {
		t.Fatalf("expected empty plaintext, got %q", opened)
	}
}
