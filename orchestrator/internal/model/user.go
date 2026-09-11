// Package model holds the shared domain structs: one model package consumed
// by api, app, and store.
package model

import (
	"strings"
	"time"
)

// ValidEmail is what this system will accept as a person's address.
//
// It lives here rather than beside the endpoint that rejects a bad one because
// the endpoint is not the only thing that has to agree with it: the personal
// edition SEEDS an address, and an address that is seeded but would be refused
// on the way back in is a person who cannot rename themselves. That happened,
// with "owner@localhost", and the failure surfaced as "a valid email is
// required" on a form nobody had typed an address into.
//
// Deliberately shallow: enough to catch what a person got wrong, and no attempt
// at RFC 5322, which accepts more than any of this is prepared to handle and
// would still not tell you the address exists.
func ValidEmail(email string) bool {
	email = strings.TrimSpace(email)
	at := strings.Index(email, "@")
	return at > 0 && at < len(email)-1 && !strings.ContainsAny(email, " \t\r\n") &&
		strings.Contains(email[at+1:], ".")
}

type Workspace struct {
	ID          int64
	Slug        string
	Name        string
	Description string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// User is a person in the tenant, not in a workspace. Which workspaces they may
// act in is a membership (WorkspaceStore.ListForUser), and which one they are
// acting in right now is a claim on their token.
type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserSetting is one preference of one person: a key, and whatever the caller
// wants to keep under it. The value is text, so it can be a word or a JSON
// object without the table needing to know which.
type UserSetting struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// SettingWorkspace is the workspace a person last switched to. The console opens
// there instead of wherever the list happens to start.
const SettingWorkspace = "workspace"

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	// StatusSuspended is a workspace's off switch: membership of a suspended
	// workspace grants nothing, and the switcher does not offer it.
	StatusSuspended = "suspended"
)
