// Package sshtool is the native custom tool template for working on a remote
// server over SSH. One instance is one configured server: an administrator
// fills in where it is and how to sign in, and the tool that comes out lets an
// agent do permitted work there without ever seeing a credential.
//
// The template has a single variant, because there is only one kind of SSH.
// Several servers means several tools, each with its own name and description,
// which is how the agent tells them apart.
package sshtool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"flexie.io/sag/internal/cmdpolicy"
)

const (
	// TemplateName is the template's id and the prefix of every tool it makes
	// ("ssh" -> "ssh_<alias>").
	TemplateName = "ssh"
	// VariantSSH is the template's only variant. A variant is a template's
	// sub-kind with its own settings form (for the query template, the database
	// driver); SSH has exactly one.
	VariantSSH = "ssh"
)

// Auth is the credential the tool signs in with: a password, a private key, or
// both offered in turn. There is no method to choose, the same way the database
// tools' bastion settings have none: whichever is filled in is what is used.
type Auth struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// empty reports that no credential at all was configured, which is the one thing
// a sign-in cannot recover from.
func (a Auth) empty() bool {
	return strings.TrimSpace(a.PrivateKey) == "" && a.Password == ""
}

// Config is a configured server, as stored on the tool's row. The secret leaves
// (the private key, its passphrase, the password) are sealed at rest and opened
// by the app just before the config reaches this package.
//
// Driver carries the template's variant. The name is the shared one: the app
// layer reads every custom tool's variant from this key.
type Config struct {
	Driver   string `json:"driver"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Auth     Auth   `json:"auth"`
	// HostKey is the server's public key in authorized_keys form. When it is
	// given it is verified on every connection; when it is blank the server is
	// not verified, which is weaker and worth filling in for anything that
	// matters. Same choice as the bastion settings the database tools collect.
	HostKey string           `json:"host_key"`
	Limits  Limits           `json:"limits"`
	Policy  cmdpolicy.Policy `json:"policy"`
	// ThroughChat carries the connection through the chat application of
	// whoever is using the tool, for a server this gateway cannot reach. It is a
	// declared setting, so it arrives as a string like all of them.
	ThroughChat string `json:"reach.chat"`

	// reach and reachKey are the caller's, filled in per call and never stored:
	// unexported, so they cannot reach the database or a fingerprint by
	// accident. reachKey is what keeps two PEOPLE apart in the connection pool,
	// which matters here more than anywhere else: a pooled connection is shared
	// by identical configurations, and two people using one chat-reached tool
	// have identical configurations and completely different computers.
	reach    func(ctx context.Context) (net.Conn, error)
	reachKey string
}

// Limits bound what one tool instance may hold open.
//
// MaxChannels is the one an administrator thinks about: how many jobs may run
// at once. JobsPerConnection is the server's own limit, not ours, and it is
// here because we cannot read it: sshd will carry only so many jobs on a single
// sign-in (MaxSessions, ten by default) and refuses the rest. Knowing it lets
// the tool sign in again instead of being refused, so MaxChannels can be a
// number about the work rather than a number about the protocol.
type Limits struct {
	MaxChannels       int `json:"max_channels"`
	JobsPerConnection int `json:"jobs_per_connection"`
	IdleMinutes       int `json:"idle_minutes"`
	ConnectSeconds    int `json:"connect_seconds"`
}

// Limit defaults and ceilings. The ceilings exist because these are settings an
// administrator types: a connection that never idles out, or a hundred channels
// on one server, is a mistake we should refuse rather than honour.
const (
	defaultMaxChannels = 4
	maxMaxChannels     = 100
	// The default matches sshd's own MaxSessions default, so a server nobody has
	// touched is described correctly without anybody having to look.
	defaultJobsPerConnection = 10
	maxJobsPerConnection     = 100
	defaultIdleMinutes       = 30
	maxIdleMinutes           = 240
	defaultConnectSeconds    = 10
	maxConnectSeconds        = 60
	defaultPort              = 22
)

// ParseConfig reads a configuration that is about to be used, and insists it is
// complete: without a credential there is nothing to sign in with. Bind and Test
// use it, so a tool that cannot work never pretends it can.
func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := parseSettings(raw)
	if err != nil {
		return Config{}, err
	}
	if cfg.Auth.empty() {
		return Config{}, fmt.Errorf("a password or a private key is required to sign in")
	}
	return cfg, nil
}

// parseSettings reads and defaults everything that is not a credential.
//
// It exists apart from ParseConfig because of the order an edit happens in: the
// settings are validated BEFORE the secrets an administrator did not retype are
// restored, so a form arriving with a blank password (meaning "leave it as it
// was") must be allowed through here. What is stored is checked in full every
// time it is used, which is where an incomplete one is actually caught.
func parseSettings(raw json.RawMessage) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("the tool's configuration could not be read")
	}

	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.HostKey = strings.TrimSpace(cfg.HostKey)

	if cfg.Host == "" {
		return Config{}, fmt.Errorf("a host is required")
	}
	if cfg.Username == "" {
		return Config{}, fmt.Errorf("a username is required")
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("the port must be between 1 and 65535")
	}
	cfg.Limits = cfg.Limits.withDefaults()
	if err := cfg.Limits.validate(); err != nil {
		return Config{}, err
	}

	cfg.Policy = cfg.Policy.WithDefaults()
	if err := cfg.Policy.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (l Limits) withDefaults() Limits {
	if l.MaxChannels == 0 {
		l.MaxChannels = defaultMaxChannels
	}
	if l.JobsPerConnection == 0 {
		l.JobsPerConnection = defaultJobsPerConnection
	}
	if l.IdleMinutes == 0 {
		l.IdleMinutes = defaultIdleMinutes
	}
	if l.ConnectSeconds == 0 {
		l.ConnectSeconds = defaultConnectSeconds
	}
	return l
}

func (l Limits) validate() error {
	if l.MaxChannels < 1 || l.MaxChannels > maxMaxChannels {
		return fmt.Errorf("the number of jobs at once must be between 1 and %d", maxMaxChannels)
	}
	if l.JobsPerConnection < 1 || l.JobsPerConnection > maxJobsPerConnection {
		return fmt.Errorf("the jobs per connection must be between 1 and %d", maxJobsPerConnection)
	}
	if l.IdleMinutes < 1 || l.IdleMinutes > maxIdleMinutes {
		return fmt.Errorf("the idle timeout must be between 1 and %d minutes", maxIdleMinutes)
	}
	if l.ConnectSeconds < 3 || l.ConnectSeconds > maxConnectSeconds {
		return fmt.Errorf("the connect timeout must be between 3 and %d seconds", maxConnectSeconds)
	}
	return nil
}

// truthy reads a checkbox's stored value. Anything a form can send for "on" is
// on: a setting that decides where a connection goes must not depend on which
// of "true" or "1" a client happened to send.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
