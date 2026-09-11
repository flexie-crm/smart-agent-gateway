// Command sag is the single entry point for the SAG orchestrator.
// The same binary runs every deployment role; the subcommand picks the
// starting angle:
//
//	sag server    web mode: HTTP API, SSE/WebSocket streaming, MCP surface
//	sag worker    jobs mode: queue consumers, scheduler, model runtime supervisor
//	sag personal  one machine, one person: the web stack and the jobs stack in
//	              this process, over a database this process owns and no broker
//	sag migrate   schema migrations (up|down|status|version)
//	sag schema    sync the database with the declared schema (--dump-sql | --update)
//	sag version   print the build version
//
// Both modes share the same internal packages (app, store, provider, tool,
// queue); they differ only in what they wire up at boot.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/api"
	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/logging"
	"flexie.io/sag/internal/migrations"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/store/sqlstore"
	"flexie.io/sag/internal/worker"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]

	// `sag dev` IS `sag server`, with the two things a working copy needs: a
	// sign-in nobody has to remember a password for, and a server that admits
	// to being a working copy so both pages can say so.
	//
	// It rewrites the mode rather than becoming a third one on purpose. Every
	// `mode == "server"` decision below (orphan recovery, the queue, the web
	// stack) must hold here exactly as it does in production, and a new mode
	// string would mean finding all of them and hoping. What differs is a flag,
	// so what differs is only what reads that flag.
	dev := mode == "dev"
	if dev {
		mode = "server"
	}

	if mode == "version" {
		fmt.Println(version)
		return
	}

	// A plain logger covers the moment before the config that configures logging
	// has been read; a bad config is reported with it, then the real logger
	// takes over at the level and format the config asked for.
	cfg, err := config.Load()
	if err != nil {
		boot := logging.New("", "")
		boot.Error().Err(err).Msg("invalid configuration")
		os.Exit(1)
	}
	// The version rides on every line, because the first question about any
	// log is which build produced it, and `sag version` cannot be run against a
	// container that has already gone.
	logger := logging.New(cfg.LogLevel, cfg.LogFormat).With().
		Str("mode", mode).Str("version", version).Logger()

	// Refused rather than started, because the alternative is a server that
	// offers a button and fails when it is pressed. The same rule the rest of
	// the product runs on: what is offered is what works.
	if dev {
		cfg.Dev = true
		// The callback a service sends somebody back to has to be THIS server,
		// on the port it is actually listening on. A default of :8080 held while
		// nothing moved, and a working copy started anywhere else would send
		// people to a port with nothing behind it. Derived the way the personal
		// edition derives it, and still overridable by anybody who means
		// something different.
		cfg.BaseURL = personalBaseURL(os.Getenv("SAG_BASE_URL"), cfg.HTTPAddr)
		if cfg.DevSignInEmail == "" || cfg.DevSignInPassword == "" {
			logger.Error().Msg("dev mode needs SAG_DEV_SIGN_IN_EMAIL and SAG_DEV_SIGN_IN_PASSWORD: " +
				"the sign-in presents a real credential to the ordinary login, so there has to be one")
			os.Exit(1)
		}
		logger = logger.With().Bool("dev", true).Logger()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Desktop mode is the one mode nobody gives a database to, so it starts the
	// one it carries before anything tries to connect. The stop is deferred
	// FIRST and therefore runs LAST, after the store and everything above it
	// have closed: the gateway finishes draining before its storage goes away.
	if mode == "personal" {
		cfg.Personal = true
		// Unset means we choose, and we choose the only address that is safe
		// here. Set means somebody meant it, so it is checked rather than
		// overridden: a desktop signs in without a password, and on an address
		// other machines can reach that is an open administrative console.
		if os.Getenv("SAG_HTTP_ADDR") == "" {
			cfg.HTTPAddr = "127.0.0.1:8080"
		} else if addrErr := requireLoopback(cfg.HTTPAddr); addrErr != nil {
			logger.Error().Err(addrErr).Msg("refusing to start")
			os.Exit(1)
		}
		// And the address it calls itself by, which is the one it just bound
		// rather than a deployment's default. See personalBaseURL: this is what
		// an OAuth redirect is built from, and a port chosen at startup cannot
		// be guessed by a constant.
		cfg.BaseURL = personalBaseURL(os.Getenv("SAG_BASE_URL"), cfg.HTTPAddr)

		// Keys that survive a restart, made before anything is sealed with
		// them. Without this the second launch cannot read what the first one
		// stored.
		state, stateErr := personalStateDir(cfg)
		if stateErr != nil {
			logger.Error().Err(stateErr).Msg("cannot locate the application data directory")
			os.Exit(1)
		}
		if err := os.MkdirAll(state, 0o700); err != nil {
			logger.Error().Err(err).Msg("cannot create the application data directory")
			os.Exit(1)
		}
		// Everything this installation writes goes under its own state
		// directory. The default is relative ("./uploads"), and an application
		// launched from an icon has no useful working directory: it resolved to
		// /uploads and the first start died on a read-only file system. The same
		// mistake as leaving a database path to a compiled-in default, in a
		// different place.
		cfg.UploadDir = filepath.Join(state, "uploads")

		// The other way to be asked to close, for the platform that has no
		// signal to ask with. Cleared before the watcher starts, so a request
		// meant for the previous run cannot stop this one the moment it is
		// ready. See personal_stop.go.
		if err := clearStopRequest(state); err != nil {
			logger.Error().Err(err).Msg("this installation could not be set up")
			os.Exit(1)
		}
		go watchForStopRequest(ctx, state, logger, stop)

		// A first run that was interrupted leaves a database that can never
		// finish migrating, because MySQL cannot roll back a half-applied
		// ALTER TABLE. Start it again rather than resuming it.
		if err := discardUnfinishedFirstRun(state, logger); err != nil {
			logger.Error().Err(err).Msg("this installation could not be set up")
			os.Exit(1)
		}
		if err := loadPersonalSecrets(cfg, state); err != nil {
			logger.Error().Err(err).Msg("this installation could not be set up")
			os.Exit(1)
		}

		dsn, stopDB, dbErr := startLocalDatabase(ctx, cfg, logger)
		if dbErr != nil {
			logger.Error().Err(dbErr).Msg("the local database could not be started")
			os.Exit(1)
		}
		defer stopDB()
		cfg.DBDSN = dsn
	}

	st, err := sqlstore.Open(ctx, cfg.DBDSN)
	if err != nil {
		logger.Error().Err(err).Msg("database connection failed")
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// Nobody is going to run `sag migrate` on their own laptop, and a schema one
	// release behind is not a state the application should ever be in. Every
	// other mode keeps migrations a deliberate act, because there somebody is
	// deploying and wants to say when.
	if mode == "personal" {
		if err := migrations.Run(ctx, st.DB(), "up"); err != nil {
			logger.Error().Err(err).Msg("the local database could not be brought up to date")
			os.Exit(1)
		}
		// The person this installation belongs to, made once. Nobody should have
		// to run a command before they can use an application they installed.
		state, stateErr := personalStateDir(cfg)
		if stateErr != nil {
			logger.Error().Err(stateErr).Msg("cannot locate the application data directory")
			os.Exit(1)
		}
		if err := seedPersonalOwner(ctx, cfg, st, state, logger); err != nil {
			logger.Error().Err(err).Msg("this installation could not be set up")
			os.Exit(1)
		}
		// What the Enter button presents on the caller's behalf. It is the real
		// password of the seeded owner, so the local sign-in is the ordinary
		// login and not a second way in.
		secret, secretErr := ownerPassword(state)
		if secretErr != nil {
			logger.Error().Err(secretErr).Msg("this installation could not be set up")
			os.Exit(1)
		}
		cfg.PersonalOwnerEmail, cfg.PersonalOwnerSecret = personalOwnerEmail, secret
		// Everything that has to happen once has happened. From here a failure
		// is reported rather than answered by starting over.
		markFirstRunFinished(state)
	}

	switch mode {
	case "migrate":
		sub := "up"
		if len(os.Args) > 2 {
			sub = os.Args[2]
		}
		err = migrations.Run(ctx, st.DB(), sub)
	case "schema":
		err = runSchema(ctx, cfg, os.Args[2:])
	case "bootstrap":
		err = runBootstrap(ctx, cfg, st, os.Args[2:])
	case "join-token":
		err = runJoinToken(ctx, cfg, st, os.Args[2:])
	case "server", "worker", "personal":
		err = serve(ctx, cfg, logger, st, mode)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		// A command a person is reading gets an error a person can read. A
		// structured log line is for a machine, and the machine is not the one
		// being told that this would drop a column.
		if mode == "schema" {
			fmt.Fprintln(os.Stderr, "schema: "+err.Error())
			os.Exit(1)
		}
		logger.Error().Err(err).Msg("exited with error")
		os.Exit(1)
	}
	logger.Info().Msg("shutdown complete")
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: sag <server|dev|worker|personal|migrate [up|down|status|version]|schema [--dump-sql|--update]|bootstrap|join-token|version>\n")
}

// serve brings the process up in either mode. Both modes share the same
// codebase and the same startup, which is the point of one binary: the worker
// that generates a title and the server that answers a chat resolve the same
// configuration and load the same tools.
func serve(ctx context.Context, cfg *config.Config, logger zerolog.Logger, st *sqlstore.SQLStore, mode string) error {
	a, err := app.New(cfg, logger, st)
	if err != nil {
		return err
	}

	// The broker, for both modes. It carries wake-ups between this process and
	// the workers (KB/05); the record is always the database row.
	//
	// What a failure MEANS differs by mode, which is why it is handled here
	// rather than inside the connect. A worker with no broker cannot be told
	// anything and has nothing to serve, so it says so and stops. A server with
	// no broker simply cannot dispatch work to workers: it runs, and does not
	// offer the abilities that would need one, which is honest rather than
	// broken.
	// Desktop has no broker and needs none. A broker carries wake-ups BETWEEN
	// processes, and here the web stack and the jobs stack are the same process,
	// so a channel does the whole job (KB/36). Nothing above this notices: the
	// record was always the database row, and a message was always a doorbell.
	if mode == "personal" {
		a.Queue = queue.NewInProcess()
		defer func() { _ = a.Queue.Close() }()
	} else if err := a.ConnectQueue(ctx); err != nil {
		if mode == "worker" {
			return fmt.Errorf("worker mode needs the queue: %w", err)
		}
		logger.Warn().Err(err).Msg("no queue: work that runs on workers is not available")
	} else {
		defer func() { _ = a.Queue.Close() }()
	}

	// The workspace's tool table follows the deploy: a tool added in this
	// release is on offer the moment the process is up, rather than the first
	// time somebody opens an admin page. It never overwrites what an
	// administrator decided about a tool it already knew.
	if err := a.SyncAllTools(ctx); err != nil {
		return err
	}

	// Boot recovery belongs to ONE process, and it is the server's.
	//
	// Everything below closes out rows that CLAIM to be in progress, on the
	// reasoning that nothing can legitimately be running in a process that has
	// just started. That reasoning holds for the process itself and NOT for the
	// deployment: the sweeps are global, so a worker starting beside a live
	// server would close the server's runs, fail its background agents, and
	// close the tool calls they were in the middle of. `docker compose up`
	// brings both up, so this is not hypothetical.
	//
	// Restricting it to the server is the honest fix for one server and one
	// worker, which is what we deploy. It does NOT survive a second server: the
	// real answer is to stamp every in-progress row with the process instance
	// that owns it and recover only what a DEAD instance owns, which is needed
	// for the distributed delegation phase anyway (KB/05, KB/27 Mode B).
	//
	// Desktop is included because there it is not a restriction at all: there is
	// exactly one process, it owns its own database, and nothing else could be
	// running against it.
	if mode == "server" || mode == "personal" {
		// A run whose process died is not running, whatever its row says. Nothing
		// can legitimately be running yet, so every row that claims to be is a
		// ghost of the last shutdown, and the conversation should say it was
		// interrupted rather than leave a turn that never ends. The same goes for
		// the individual tool calls inside those runs.
		if err := run.RecoverOrphans(ctx, st, logger); err != nil {
			return err
		}
		// A background delegation is an in-process goroutine (KB/27), so a restart
		// kills it while its record still says running. Close those ghosts out and,
		// for each, fire the Gateway's completion turn: a reloaded chat shows no chip
		// spinning for work nothing is doing, and the person is told the task did not
		// finish rather than left with silence.
		if n, err := a.RecoverInterruptedDelegations(ctx); err != nil {
			return err
		} else if n > 0 {
			logger.Warn().Int("delegations", n).Msg("background delegations interrupted by a previous shutdown")
		}
	}
	// Whatever is still answering when we go down is told to stop, so we do not
	// leave the same ghosts behind for the next process.
	defer a.Runs.Shutdown()
	// Connections the tools hold open outlive a turn on purpose, so they are
	// closed here rather than left dangling on the far side.
	defer a.Shutdown()

	// The machine this computer is. Started here because minting its join token
	// needs the app, and stopped before the database because it is registered in
	// one. Its failure is not fatal: see startLocalNode.
	if mode == "personal" {
		defer startLocalNode(ctx, a, cfg, logger)()
	}

	switch mode {
	case "personal":
		return serveBoth(ctx, a)
	case "server":
		return api.Serve(ctx, a)
	default:
		return worker.Run(ctx, a)
	}
}

// serveBoth runs the web stack and the jobs stack together, which is what one
// machine serving one person needs and what the queue interface makes ordinary
// rather than special.
//
// Whichever half ends first takes the other with it. They are one process and
// half a desktop, an interface with nothing behind it or a worker with nobody
// asking, is not a state worth staying in.
func serveBoth(ctx context.Context, a *app.App) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan error, 2)
	go func() { done <- api.Serve(ctx, a) }()
	go func() { done <- worker.Run(ctx, a) }()

	first := <-done
	cancel()
	second := <-done
	if first != nil {
		return first
	}
	return second
}
