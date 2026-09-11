package model

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Public identifiers are opaque and unguessable.
//
// A row id is a fine primary key and a terrible public identifier: it says how
// many of a thing exist and invites walking the range. Authorization stops a
// stranger reading someone else's conversation; an opaque id stops them asking
// the question at all.

// Prefixes make an id self-describing in a log or a bug report, which is worth
// more than the few bytes they cost.
const (
	ChatUIDPrefix       = "ch_"
	RunUIDPrefix        = "run_"
	AttachmentUIDPrefix = "at_"
)

// NewChatUID mints a conversation's public identifier.
func NewChatUID() (string, error) { return newUID(ChatUIDPrefix) }

// NewRunUID mints the identifier a client holds to reattach to a turn.
func NewRunUID() (string, error) { return newUID(RunUIDPrefix) }

// NewAttachmentUID mints an uploaded file's public identifier. It is also the
// name the bytes are stored under, which is the point: nothing a person typed
// ever reaches a path.
func NewAttachmentUID() (string, error) { return newUID(AttachmentUIDPrefix) }

// newUID is 128 bits of randomness, which is not a number anybody guesses.
func newUID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("model: read random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
