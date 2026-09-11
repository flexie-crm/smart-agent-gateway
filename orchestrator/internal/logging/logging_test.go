package logging

import (
	"testing"

	"github.com/rs/zerolog"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]zerolog.Level{
		"trace":   zerolog.TraceLevel,
		"debug":   zerolog.DebugLevel,
		"info":    zerolog.InfoLevel,
		"warn":    zerolog.WarnLevel,
		"warning": zerolog.WarnLevel,
		"ERROR":   zerolog.ErrorLevel,
		" info ":  zerolog.InfoLevel,
		// An empty or unknown level defaults to info, so a typo makes the logs
		// louder, never silent.
		"":        zerolog.InfoLevel,
		"verbose": zerolog.InfoLevel,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
