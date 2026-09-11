package sshtool

import (
	"encoding/json"
	"strings"
	"testing"

	"flexie.io/sag/internal/cmdpolicy"
)

// The configuration is the whole security surface of this tool: where it
// connects, how it proves itself, and how it verifies the machine it reached.
// Every one of those has to be refused when it is missing, at create time, not
// on the first call.

func TestParseConfigRejectsIncompleteSettings(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"no host", `{"username":"deploy","host_key":"ssh-ed25519 AAAA","auth":{"private_key":"x"}}`, "a host is required"},
		{"no username", `{"host":"h","host_key":"ssh-ed25519 AAAA","auth":{"private_key":"x"}}`, "a username is required"},
		{"port out of range", `{"host":"h","username":"deploy","port":70000,"host_key":"k","auth":{"private_key":"x"}}`, "port must be between"},
		{"no credential at all", `{"host":"h","username":"deploy","host_key":"k","auth":{},"policy":{"mode":"allowlist","allowed":"uptime"}}`, "password or a private key is required"},
		{"too many jobs at once", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"allowlist","allowed":"uptime"},"limits":{"max_channels":500}}`, "jobs at once must be between"},
		{"too many jobs on one connection", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"allowlist","allowed":"uptime"},"limits":{"jobs_per_connection":500}}`, "jobs per connection must be between"},
		{"idle too long", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"allowlist","allowed":"uptime"},"limits":{"idle_minutes":10000}}`, "idle timeout must be between"},
		{"connect too short", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"allowlist","allowed":"uptime"},"limits":{"connect_seconds":1}}`, "connect timeout must be between"},
		{"allowlist with nothing on it", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"}}`, "list the commands"},
		{"unknown policy", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"anything"}}`, "must be allowlist or denylist"},
		{"unknown sudo setting", `{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"denylist","sudo":"maybe"}}`, "sudo must be deny or allow"},
		{"not json", `nonsense`, "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig(json.RawMessage(tc.config))
			if err == nil {
				t.Fatalf("expected %q to be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseConfigFillsDefaults(t *testing.T) {
	cfg, err := ParseConfig(json.RawMessage(
		`{"host":"h","username":"deploy","host_key":"k","auth":{"password":"p"},"policy":{"mode":"allowlist","allowed":"uptime"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Fatalf("port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.Limits.MaxChannels != defaultMaxChannels {
		t.Fatalf("max channels = %d, want %d", cfg.Limits.MaxChannels, defaultMaxChannels)
	}
	if cfg.Limits.IdleMinutes != defaultIdleMinutes {
		t.Fatalf("idle minutes = %d, want %d", cfg.Limits.IdleMinutes, defaultIdleMinutes)
	}
	if cfg.Limits.ConnectSeconds != defaultConnectSeconds {
		t.Fatalf("connect seconds = %d, want %d", cfg.Limits.ConnectSeconds, defaultConnectSeconds)
	}
	// A setting left blank settles on the safer reading, never the permissive one.
	if cfg.Policy.Sudo != cmdpolicy.SudoDeny {
		t.Fatalf("sudo = %q, want %q", cfg.Policy.Sudo, cmdpolicy.SudoDeny)
	}
}

// An edit arrives with its secrets blanked, meaning "leave them as they were",
// and is validated BEFORE the app restores them. So the settings read has to
// accept a blank credential where the full read insists on one: without this,
// every edit of an SSH tool would be refused.
func TestSettingsReadAcceptsABlankCredentialButTheFullReadDoesNot(t *testing.T) {
	blank := json.RawMessage(
		`{"host":"h","username":"deploy","host_key":"k","auth":{"password":"","private_key":""},` +
			`"policy":{"mode":"allowlist","allowed":"uptime"}}`)

	if _, err := parseSettings(blank); err != nil {
		t.Fatalf("an edit with its secrets blanked was refused: %v", err)
	}
	if _, err := ParseConfig(blank); err == nil {
		t.Fatal("a configuration with nothing to sign in with was accepted for use")
	}
}

func TestParseConfigKeepsExplicitLimits(t *testing.T) {
	cfg, err := ParseConfig(json.RawMessage(
		`{"host":"h","port":2222,"username":"deploy","host_key":"k","auth":{"password":"p"},` +
			`"policy":{"mode":"denylist","denied":"reboot","sudo":"allow"},` +
			`"limits":{"max_channels":8,"idle_minutes":5,"connect_seconds":15}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Port != 2222 || cfg.Limits.MaxChannels != 8 || cfg.Limits.IdleMinutes != 5 || cfg.Limits.ConnectSeconds != 15 {
		t.Fatalf("settings were not kept: %+v", cfg)
	}
}
