// Package crypto seals secrets that must live in the database: vendor API
// keys, MCP server credentials, and anything else a workspace entrusts to
// us.
//
// Envelope encryption: every secret gets a fresh random data key (DEK),
// which encrypts the plaintext; the DEK itself is wrapped with a key
// encryption key (KEK) held outside the database. Rotating a KEK therefore
// means rewrapping small DEKs, never re-encrypting every secret, and a
// stolen database is useless without the KEK.
//
// Both layers are AES-256-GCM, which authenticates as well as encrypts: a
// tampered ciphertext fails to open rather than decrypting to garbage.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// sealVersion prefixes every sealed blob so the format can change
	// without guessing at what old rows contain.
	sealVersion = 1

	keySize   = 32 // AES-256
	nonceSize = 12 // GCM standard nonce
)

var (
	ErrNoPrimaryKey  = errors.New("crypto: no primary key configured")
	ErrUnknownKey    = errors.New("crypto: sealed with an unknown key id")
	ErrMalformedSeal = errors.New("crypto: malformed sealed value")
	ErrBadVersion    = errors.New("crypto: unsupported seal version")
)

// Keyring holds the key encryption keys. Several may be present at once:
// the primary seals new secrets, the others exist so secrets sealed before
// a rotation can still be opened.
type Keyring struct {
	primaryID string
	keys      map[string][]byte
}

// NewKeyring builds a keyring from key material. Key ids are opaque labels
// (for example "1", "2"), carried inside each sealed value so the right key
// is chosen at open time without a database column to keep in sync.
func NewKeyring(primaryID string, keys map[string][]byte) (*Keyring, error) {
	if primaryID == "" {
		return nil, ErrNoPrimaryKey
	}
	if len(keys) == 0 {
		return nil, ErrNoPrimaryKey
	}
	for id, key := range keys {
		if id == "" {
			return nil, errors.New("crypto: empty key id")
		}
		if strings.Contains(id, ":") || strings.Contains(id, ",") {
			return nil, fmt.Errorf("crypto: key id %q must not contain ':' or ','", id)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("crypto: key %q must be %d bytes, got %d", id, keySize, len(key))
		}
	}
	if _, ok := keys[primaryID]; !ok {
		return nil, fmt.Errorf("crypto: primary key id %q is not in the keyring", primaryID)
	}
	return &Keyring{primaryID: primaryID, keys: keys}, nil
}

// ParseKeyring reads "id:hexkey,id:hexkey" plus the id of the primary key.
// This is how the deployment supplies key material (env, secrets manager,
// mounted file), never the database.
func ParseKeyring(spec, primaryID string) (*Keyring, error) {
	keys := make(map[string][]byte)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, hexKey, found := strings.Cut(entry, ":")
		if !found {
			return nil, errors.New("crypto: key spec entries must look like id:hexkey")
		}
		key, err := hex.DecodeString(strings.TrimSpace(hexKey))
		if err != nil {
			return nil, fmt.Errorf("crypto: key %q is not valid hex: %w", id, err)
		}
		keys[strings.TrimSpace(id)] = key
	}
	return NewKeyring(primaryID, keys)
}

// PrimaryID reports which key seals new secrets.
func (k *Keyring) PrimaryID() string { return k.primaryID }

// Seal encrypts plaintext under a fresh data key wrapped by the primary
// key. The layout is:
//
//	version | idLen | keyID | kekNonce | wrappedDEK | dataNonce | ciphertext
//
// The key id travels with the value, so a rotation never has to find and
// rewrite every row before old secrets can be read.
func (k *Keyring) Seal(plaintext []byte) ([]byte, error) {
	kek, ok := k.keys[k.primaryID]
	if !ok {
		return nil, ErrNoPrimaryKey
	}

	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: generate data key: %w", err)
	}

	wrappedDEK, kekNonce, err := encryptWith(kek, dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap data key: %w", err)
	}
	ciphertext, dataNonce, err := encryptWith(dek, plaintext)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt secret: %w", err)
	}

	out := make([]byte, 0, 2+len(k.primaryID)+len(kekNonce)+len(wrappedDEK)+len(dataNonce)+len(ciphertext))
	out = append(out, sealVersion, byte(len(k.primaryID)))
	out = append(out, k.primaryID...)
	out = append(out, kekNonce...)
	out = append(out, wrappedDEK...)
	out = append(out, dataNonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// Open reverses Seal. A tampered blob, a wrong key, or a truncated value
// all fail here rather than yielding a plausible-looking secret.
func (k *Keyring) Open(sealed []byte) ([]byte, error) {
	// version + idLen + at least one id byte
	if len(sealed) < 3 {
		return nil, ErrMalformedSeal
	}
	if sealed[0] != sealVersion {
		return nil, ErrBadVersion
	}
	idLen := int(sealed[1])
	rest := sealed[2:]
	if idLen == 0 || len(rest) < idLen {
		return nil, ErrMalformedSeal
	}
	kek, ok := k.keys[string(rest[:idLen])]
	if !ok {
		return nil, ErrUnknownKey
	}
	rest = rest[idLen:]

	// wrapped DEK is the key plus the GCM tag.
	wrappedLen := keySize + wrapOverhead()
	if len(rest) < nonceSize+wrappedLen+nonceSize {
		return nil, ErrMalformedSeal
	}
	kekNonce := rest[:nonceSize]
	wrappedDEK := rest[nonceSize : nonceSize+wrappedLen]
	rest = rest[nonceSize+wrappedLen:]
	dataNonce := rest[:nonceSize]
	ciphertext := rest[nonceSize:]

	dek, err := decryptWith(kek, kekNonce, wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap data key: %w", err)
	}
	plaintext, err := decryptWith(dek, dataNonce, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt secret: %w", err)
	}
	return plaintext, nil
}

// NeedsRewrap reports whether a sealed value was produced by a key other
// than the primary one, so a rotation job can find what to rewrap.
func (k *Keyring) NeedsRewrap(sealed []byte) bool {
	id, err := sealedKeyID(sealed)
	if err != nil {
		return false
	}
	return id != k.primaryID
}

func sealedKeyID(sealed []byte) (string, error) {
	if len(sealed) < 3 || sealed[0] != sealVersion {
		return "", ErrMalformedSeal
	}
	idLen := int(sealed[1])
	if idLen == 0 || len(sealed) < 2+idLen {
		return "", ErrMalformedSeal
	}
	return string(sealed[2 : 2+idLen]), nil
}

func encryptWith(key, plaintext []byte) (ciphertext, nonce []byte, err error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func decryptWith(key, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Never say why: a padding-oracle style distinction between
		// "wrong key" and "tampered" helps an attacker, not us.
		return nil, errors.New("authentication failed")
	}
	return plaintext, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return aead, nil
}

func wrapOverhead() int {
	aead, err := newAEAD(make([]byte, keySize))
	if err != nil {
		// A zero key of the right length always builds an AES-GCM AEAD;
		// reaching here would mean the standard library changed.
		panic("crypto: cannot construct AES-GCM: " + err.Error())
	}
	return aead.Overhead()
}
