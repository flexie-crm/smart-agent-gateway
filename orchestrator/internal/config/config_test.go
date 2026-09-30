package config

import (
	"reflect"
	"testing"
)

func TestParseOrigins(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty means same-origin only", raw: "", want: nil},
		{name: "blank means same-origin only", raw: "   ", want: nil},
		{
			name: "a list, with the spaces people put after commas",
			raw:  "http://localhost:5174, http://127.0.0.1:5174",
			want: []string{"http://localhost:5174", "http://127.0.0.1:5174"},
		},
		{
			name: "a trailing comma is not an entry",
			raw:  "https://console.example.com,",
			want: []string{"https://console.example.com"},
		},
		{
			// A browser sends "http://localhost:5174", so an entry without a
			// scheme could never match anything. Refusing it at boot beats
			// silently allowing nothing.
			name:    "an entry without a scheme is a boot error",
			raw:     "localhost:5174",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOrigins(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseOrigins(%q) accepted a non-origin", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOrigins(%q): %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseOrigins(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// Where the chat starts showing how full a conversation is: the default when
// nothing is set, the number when one is, and a refusal at boot for anything
// that is not a percentage, rather than a meter that never shows or always does.
func TestTheContextWarningIsAPercentage(t *testing.T) {
	t.Setenv("SAG_CONTEXT_WARN_PERCENT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The line appears from 80 percent: close enough to the trim to matter,
	// early enough to compact before anything is dropped.
	if DefaultContextWarnPercent != 80 {
		t.Fatalf("the default threshold is %d, want 80", DefaultContextWarnPercent)
	}
	if cfg.ContextWarnPercent != DefaultContextWarnPercent {
		t.Fatalf("unset came out %d, want %d", cfg.ContextWarnPercent, DefaultContextWarnPercent)
	}

	t.Setenv("SAG_CONTEXT_WARN_PERCENT", "30")
	if cfg, err = Load(); err != nil || cfg.ContextWarnPercent != 30 {
		t.Fatalf("30 came out %d (%v)", cfg.ContextWarnPercent, err)
	}

	for _, bad := range []string{"0", "101", "-5", "half", "50%"} {
		t.Setenv("SAG_CONTEXT_WARN_PERCENT", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
