package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/localdb"
	"flexie.io/sag/internal/supervise"
)

// startLocalDatabase brings up the database this installation carries and hands
// back the DSN to reach it, plus the way to put it down again.
//
// Desktop mode is the one mode that is given no database: every other mode is
// handed a DSN and connects to something somebody else runs. So this happens
// BEFORE anything opens a store, and the returned stop must run AFTER everything
// that uses one has drained (KB/36: shutdown in reverse start order).
func startLocalDatabase(ctx context.Context, cfg *config.Config, logger zerolog.Logger) (string, func(), error) {
	bundle, err := personalBundleDir(cfg)
	if err != nil {
		return "", nil, err
	}
	state, err := personalStateDir(cfg)
	if err != nil {
		return "", nil, err
	}
	logger.Info().Str("bundle", bundle).Str("state", state).Msg("starting the local database")

	db, err := localdb.New(localdb.Config{BundleDir: bundle, StateDir: state})
	if err != nil {
		return "", nil, err
	}
	// Everything that has to be true before the server can be started happens
	// here, so the supervised child only ever has to start it. On a first run
	// this is where the database is created.
	if err := db.Prepare(ctx); err != nil {
		return "", nil, err
	}

	sup := supervise.New(logger, supervise.DefaultIntensity)
	sup.Add(db.Child())

	supCtx, stopSup := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	go func() { done <- sup.Run(supCtx) }()

	// The supervisor does not report readiness, it enforces it: Run has already
	// waited for the child's own probe before the child counts as started. This
	// waits for the same probe from the outside, and watches for the supervisor
	// giving up so a database that cannot start is an error here rather than a
	// timeout somewhere further in.
	if err := awaitDatabase(ctx, db, done); err != nil {
		stopSup()
		<-done
		return "", nil, err
	}

	stop := func() {
		stopSup()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			logger.Error().Err(err).Msg("the local database did not stop cleanly")
		}
	}
	return db.DSN(), stop, nil
}

func awaitDatabase(ctx context.Context, db *localdb.DB, supervisorDone <-chan error) error {
	deadline := time.Now().Add(2 * time.Minute)
	var last error
	for {
		select {
		case err := <-supervisorDone:
			if err == nil {
				err = errors.New("it stopped before it answered")
			}
			return fmt.Errorf("the local database could not be started: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if last = db.Ping(ctx); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the local database never answered: %w", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// personalBundleDir finds the server we ship. Beside the executable by default,
// because that is where an installer puts it and where a signed application
// bundle keeps it.
func personalBundleDir(cfg *config.Config) (string, error) {
	if cfg.PersonalBundleDir != "" {
		return cfg.PersonalBundleDir, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the application: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve the application path: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "mariadb"), nil
}

// personalStateDir is where this installation's data lives: the database, its
// key, its logs.
//
// Never inside the application. On macOS that is a signed bundle and writing to
// it breaks the signature; everywhere it is the difference between data that
// survives an update and data an update replaces.
func personalStateDir(cfg *config.Config) (string, error) {
	if cfg.PersonalStateDir != "" {
		return cfg.PersonalStateDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the application data directory: %w", err)
	}
	dir := filepath.Join(base, personalDataDirName)
	if err := adoptEarlierDataDir(base, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// personalDataDirName is the folder this edition keeps its data in, named for
// the product. The other edition has its own, so both can be installed at once.
const personalDataDirName = "SAG Personal"

// earlierDataDirName is what it used to be called, before the product settled
// on one name.
const earlierDataDirName = "Flexie SAG"

// adoptEarlierDataDir moves an installation that predates the name.
//
// A folder is not a cosmetic detail here: it holds the database, the keys that
// everything sealed was sealed with, and the models. Renaming the constant
// alone would leave an existing installation looking brand new, with its
// conversations, its agents and its connections still on disk under a name
// nothing looks at any more. So the old one is MOVED, once, and only when there
// is no new one to overwrite.
func adoptEarlierDataDir(base, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		// Already here. Whatever is under the old name is somebody's business
		// but not ours: two data directories must never be merged, because the
		// keys in one cannot open what was sealed with the other.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("look for the application data directory: %w", err)
	}
	earlier := filepath.Join(base, earlierDataDirName)
	if _, err := os.Stat(earlier); err != nil {
		// Nothing to adopt: this is a first run, which is the common case.
		return nil
	}
	if err := os.Rename(earlier, dir); err != nil {
		return fmt.Errorf("move this installation's data to %q: %w", dir, err)
	}
	return nil
}
