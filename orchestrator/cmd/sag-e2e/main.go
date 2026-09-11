// Command sag-e2e boots a real orchestrator for the browser end-to-end tests:
// the same server the production binary runs, but with a scripted model in place
// of a real vendor so the flow is deterministic (internal/e2e). It migrates and
// seeds its database on boot, then serves. The make e2e target starts it, points
// the chat UI at it, runs Playwright, and stops it.
//
// It is never the production entrypoint; that is cmd/sag.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"flexie.io/sag/internal/api"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/e2e"
	"flexie.io/sag/internal/logging"
	"flexie.io/sag/internal/migrations"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store/sqlstore"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		boot := logging.New("", "")
		boot.Error().Err(err).Msg("invalid configuration")
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat).With().Str("mode", "e2e").Logger()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := sqlstore.Open(ctx, cfg.DBDSN)
	if err != nil {
		logger.Error().Err(err).Msg("database connection failed")
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// A fresh scratch database, brought fully up to the declared schema, then
	// seeded. The make target drops and recreates the database first, so this is
	// a clean slate every run.
	if err := migrations.Run(ctx, st.DB(), "up"); err != nil {
		logger.Error().Err(err).Msg("migrate failed")
		os.Exit(1)
	}

	a, err := app.New(cfg, logger, st)
	if err != nil {
		logger.Error().Err(err).Msg("app init failed")
		os.Exit(1)
	}

	// The whole point: every model call goes to the scripted provider, so the
	// delegate/park/resume/complete flow is deterministic in the browser.
	a.Gateway.UseProviderBuilder(func(*model.AIVendor, string) (provider.Provider, error) {
		return &e2e.ScriptedProvider{}, nil
	})

	// A real broker and a real worker, in this process. A fleet (Mode B) is the
	// one flow that crosses a process boundary in production, and the bugs it
	// produced lived in exactly the seams a fake queue would have hidden.
	// A FRESH broker every boot, taken away on the way out.
	//
	// The run resets the database, and a broker kept beside it does not reset:
	// its stream still holds the last run's messages and its durable consumers
	// still hold the last run's read positions. A gate whose two halves remember
	// different runs fails on a schedule nobody can see, in whichever spec
	// happens to be running, which is exactly what it did.
	brokerDir, err := os.MkdirTemp("", "sag-e2e-broker-")
	if err != nil {
		logger.Error().Err(err).Msg("broker directory failed")
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(brokerDir) }()

	stopBroker, err := e2e.StartBroker(ctx, a, brokerDir)
	if err != nil {
		logger.Error().Err(err).Msg("broker init failed")
		os.Exit(1)
	}
	defer stopBroker()
	e2e.StartWorker(ctx, a)

	if err := e2e.Seed(ctx, a); err != nil {
		logger.Error().Err(err).Msg("seed failed")
		os.Exit(1)
	}

	if err := run.RecoverOrphans(ctx, st, logger); err != nil {
		logger.Error().Err(err).Msg("recover orphans failed")
		os.Exit(1)
	}
	if _, err := a.RecoverInterruptedDelegations(ctx); err != nil {
		logger.Error().Err(err).Msg("recover delegations failed")
		os.Exit(1)
	}
	defer a.Runs.Shutdown()

	if err := api.Serve(ctx, a); err != nil {
		logger.Error().Err(err).Msg("server exited with error")
		os.Exit(1)
	}
}
