// Command sag-repo is the public download host.
//
//	sag-repo
//
// It serves one page, the inference node binaries, and two endpoints a machine
// being installed uses to prove it can be reached from the internet.
//
// It is a second binary rather than a mode of `sag` because it shares nothing
// with the gateway: no database, no session, no secret, no configuration beyond
// a directory and a port. Everything it serves is meant to be fetched by a
// stranger with no credentials, from a headless box. Making it a mode would mean
// a public host running a binary that carries the whole product, and a reader
// having to check which half was asleep.
//
// Environment:
//
//	SAG_REPO_ADDR          what to listen on            (default :8080)
//	SAG_REPO_DIST_DIR      the directory of binaries    (required)
//	SAG_REPO_BEHIND_PROXY  an edge proxy is in front    (default true)
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flexie.io/sag/internal/logging"
	"flexie.io/sag/internal/repo"
)

// Set at build time (-X main.version). A host that cannot say which build it is
// is a host nobody can reason about when it starts serving the wrong update.
var version = "0.1.0-dev"

func main() {
	logger := logging.New(env("SAG_REPO_LOG_LEVEL", "info"), env("SAG_REPO_LOG_FORMAT", "json")).
		With().Str("mode", "repo").Str("version", version).Logger()

	dir := os.Getenv("SAG_REPO_DIST_DIR")
	if dir == "" {
		logger.Error().Msg("SAG_REPO_DIST_DIR is required: the directory holding the published binaries")
		os.Exit(1)
	}
	// Refused here rather than per request: a download host that starts and
	// then answers 404 for everything looks like a working deployment.
	srv, err := repo.New(dir, env("SAG_REPO_BEHIND_PROXY", "true") != "false")
	if err != nil {
		logger.Error().Err(err).Msg("cannot serve downloads")
		os.Exit(1)
	}

	addr := env("SAG_REPO_ADDR", ":8080")
	server := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// A download of fifty megabytes over a slow link is normal here, so the
		// write timeout is generous where the read one is not: nothing this
		// serves takes a large request body.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// A download in flight is somebody watching a progress bar, so it is
		// given time to finish rather than cut.
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	logger.Info().Str("addr", addr).Str("dir", dir).Msg("download host listening")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error().Err(err).Msg("stopped")
		os.Exit(1)
	}
	logger.Info().Msg("shutdown complete")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
