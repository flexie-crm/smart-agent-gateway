package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
)

// How the application asks the gateway to close.
//
// # Why a file and not a signal
//
// On macOS the shell sends SIGTERM to the gateway's process group and everything
// below works: the gateway quiesces, drains and settles (KB/27), then asks its
// database to stop, then exits. Windows has no SIGTERM. What it has instead is
// console control events, and those cannot be delivered here: the sender must be
// attached to the target's console, and the shell is a windowed application with
// no console at all, so its child has none either. Giving the child one means a
// console window flashing up behind the application.
//
// What is left is `taskkill`. Without /F it asks a window to close, which a
// gateway does not have, and it fails outright: BOTH the gateway and the
// database survived it, so closing the application left the whole stack running
// with no window to stop it from. With /F it is a hard kill, which skips the
// drain entirely and cuts the database off mid-write, and that is the thing the
// supervision tree exists to avoid.
//
// So the request arrives the same way the database's does: over a channel that
// already exists. The shell resolves this installation's state directory and
// tells the gateway where it is, so both sides already agree on one directory,
// and it already holds the things that matter (the database password, the record
// of which port the gateway is on). Writing a file into it is not a new trust
// boundary: anybody who can write there already owns the installation.
//
// Nothing about shutting down changes. The watcher cancels exactly the context a
// signal would have cancelled, so there is ONE shutdown path and this is only a
// second doorbell on it.
const stopFileName = "stop"

// stopPollEvery is how often the doorbell is looked at. Short enough that
// closing the window feels immediate, and a stat of one path costs nothing.
const stopPollEvery = 200 * time.Millisecond

// clearStopRequest removes a request left over from a previous run.
//
// It must happen before the watcher starts, and it is not housekeeping: the file
// outlives the process it was meant for, so without this the next launch would
// read somebody else's old request and shut down the moment it finished starting.
// That is a first run that creates a database, applies every migration, and then
// quits, which reads as a crash.
func clearStopRequest(state string) error {
	err := os.Remove(filepath.Join(state, stopFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear a previous stop request: %w", err)
	}
	return nil
}

// watchForStopRequest ends the run when the application asks it to.
//
// stop is the cancel from signal.NotifyContext, so a request that arrives here
// is indistinguishable downstream from a SIGTERM. It returns when the context
// ends, which includes the case where it was the one that ended it.
func watchForStopRequest(ctx context.Context, state string, logger zerolog.Logger, stop func()) {
	path := filepath.Join(state, stopFileName)
	ticker := time.NewTicker(stopPollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(path); err != nil {
				continue
			}
			// Taken away as it is acted on, so the request is spent exactly once
			// and a gateway started next in this directory does not read it
			// again.
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Warn().Err(err).Msg("could not clear the stop request")
			}
			logger.Info().Msg("the application asked this installation to close")
			stop()
			return
		}
	}
}
