package query

import (
	"encoding/json"
	"fmt"
	"strings"

	"flexie.io/sag/internal/datasource"
	"flexie.io/sag/internal/sqlguard"
)

// Settings is one query tool instance, read from its stored configuration:
// where the database is, what the tool may do to it, and what it may see of it.
//
// The connection is the shared shape, so a workflow reads it the same way. The
// access mode and the policy are this tool's own, because both are about what an
// assistant may do rather than about the database: one decides whether a
// statement may change anything, the other which tables and fields it may reach
// at all.
type Settings struct {
	Connection datasource.Config
	Access     Access
	Policy     sqlguard.Policy
	// ThroughChat carries the connection through the chat application of
	// whoever is using the tool, for a database this server cannot reach at all.
	ThroughChat bool
}

// ParseConfig reads an instance's stored config.
func ParseConfig(raw json.RawMessage) (Settings, error) {
	var wire struct {
		datasource.Config
		Access Access          `json:"access"`
		Policy sqlguard.Policy `json:"policy"`
		// A declared setting arrives as a string, whatever its type: the form is
		// one shape for all of them (template.FieldCheckbox).
		ThroughChat string `json:"reach.chat"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Settings{}, fmt.Errorf("the tool's configuration could not be read")
	}
	wire.Driver = strings.ToLower(strings.TrimSpace(wire.Driver))
	if _, ok := datasource.Get(wire.Driver); !ok {
		return Settings{}, fmt.Errorf("unsupported database driver %q", wire.Driver)
	}
	if !wire.Access.Valid() {
		return Settings{}, fmt.Errorf("access must be read, write, or both")
	}
	if strings.TrimSpace(wire.Host) == "" || strings.TrimSpace(wire.Database) == "" {
		return Settings{}, fmt.Errorf("a host and a database are required")
	}
	if wire.TLS.Mode == "" {
		wire.TLS.Mode = "disable"
	}
	if err := wire.Policy.Validate(); err != nil {
		return Settings{}, err
	}
	// A policy is refused rather than stored unenforced: an administrator who
	// fills this in has said what the tool may see, and a tool that kept the
	// answer without acting on it would be the worst of both.
	if wire.Policy.Active() && !sqlguard.Supports(wire.Driver) {
		return Settings{}, fmt.Errorf("a table and field policy cannot be enforced on this kind of database yet, so it cannot be set here")
	}
	return Settings{
		Connection:  wire.Config,
		Access:      wire.Access,
		Policy:      wire.Policy,
		ThroughChat: truthy(wire.ThroughChat),
	}, nil
}

// truthy reads a checkbox's stored value. Anything a form can send for "on" is
// on, and everything else is off: a setting that decides where a connection
// goes must not depend on which of "true" or "1" a client happened to send.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
