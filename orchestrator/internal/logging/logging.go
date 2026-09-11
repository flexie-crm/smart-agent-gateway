// Package logging builds the process logger: readable in a terminal, structured
// in production, and level-controlled, so a person can watch what the server is
// doing without reading JSON, and a collector can parse it when it needs to.
//
// Logs go to stderr, never a file: a container captures the stream, and where
// it goes from there is the deployment's decision, not the process's.
package logging

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// New builds a logger from a level and a format.
//
// Level, quietest to loudest: error, warn, info (the default), debug, trace. A
// level shows itself and everything more severe, so "warn" shows warnings and
// errors, "debug" shows everything.
//
// Format: "console" (the default) is one readable line per event, coloured when
// stderr is a terminal; "json" is one object per line, for a log collector.
func New(level, format string) zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339
	return zerolog.New(writer(format)).Level(ParseLevel(level)).With().Timestamp().Logger()
}

func writer(format string) io.Writer {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return os.Stderr
	}
	return zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "15:04:05.000",
		// Colour a terminal. A pipe, a file, or a container's log capture is not
		// a terminal, so the escape codes would only be noise there.
		NoColor: !isTerminal(os.Stderr),
	}
}

// ParseLevel maps a level name to its zerolog level, defaulting to info for an
// empty or unrecognized name so a typo makes the logs louder, never silent.
func ParseLevel(level string) zerolog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
