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
	// Allowed is what an administrator ticked under "What it may do", by
	// capability key. Absent means off, which is what every tool made before a
	// capability existed says, and is the safe direction.
	Allowed map[string]bool
}

// May reports whether a capability was turned on for this tool.
func (s Settings) May(capability string) bool { return s.Allowed[capability] }

// ParseConfig reads an instance's stored config.
func ParseConfig(config json.RawMessage) (Settings, error) {
	var wire struct {
		datasource.Config
		Access Access          `json:"access"`
		Policy sqlguard.Policy `json:"policy"`
		// A declared setting arrives as a string, whatever its type: the form is
		// one shape for all of them (template.FieldCheckbox).
		ThroughChat string `json:"reach.chat"`
		// The checkboxes, as the form stores them: one object, a key per
		// capability. It is read as a map rather than as named fields because
		// which capabilities exist is the DRIVER's answer, and a list here would
		// go stale the day a database is added.
		Allow map[string]any `json:"allow"`
	}
	if err := json.Unmarshal(config, &wire); err != nil {
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
	// What the driver offers is the only list consulted: a stored setting for a
	// capability this engine does not have is ignored rather than honoured, so
	// moving a tool between drivers cannot quietly grant something.
	allowed := map[string]bool{}
	if d, ok := datasource.Get(wire.Driver); ok {
		for _, c := range d.Capabilities() {
			if v, ok := wire.Allow[c.Key].(string); ok && truthy(v) {
				allowed[c.Key] = true
			}
		}
	}

	return Settings{
		Connection:  wire.Config,
		Access:      wire.Access,
		Policy:      wire.Policy,
		ThroughChat: truthy(wire.ThroughChat),
		Allowed:     allowed,
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
