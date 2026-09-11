// Package app holds the shared application core, the business-logic layer
// both entry modes (server, worker) are wired onto. HTTP handlers and queue
// consumers stay thin and call into App; App talks to the store, queue,
// providers, and tool registry.
package app

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/crypto"
	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/filestore"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/oauth"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/queue"
	"flexie.io/sag/internal/run"
	"flexie.io/sag/internal/state"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/tools/query"
	"flexie.io/sag/internal/tools/sshtool"
	"flexie.io/sag/internal/tools/template"
	"flexie.io/sag/internal/ws"
	"github.com/rs/zerolog"

	// The one database this build can reach, and the one dialect it can read
	// well enough to hold a policy. Each registers itself from its own package,
	// so it is named here or it is not in the binary at all: a driver nobody
	// imports is a database nobody can connect to, and an analyzer nobody
	// imports is a policy that cannot be saved. Losing one of these is silent,
	// which is why there is a test next door that asks for them by name.
	_ "flexie.io/sag/internal/datasource/mysql"
	_ "flexie.io/sag/internal/datasource/postgres"
	_ "flexie.io/sag/internal/sqlguard/mysql"
	_ "flexie.io/sag/internal/sqlguard/postgres"
)

type App struct {
	Config   *config.Config
	Log      zerolog.Logger
	Store    store.Store
	Tools    *tool.Registry
	OAuth    *oauth.Service
	Sessions *auth.SessionManager
	Tokens   *auth.TokenService
	Keyring  *crypto.Keyring
	// trust is the machine authority and the certificate we dial machines with,
	// worked out the first time a machine is reached and kept: making a signing
	// key is not something to repeat per call. machineTrustOnce guards it, and
	// is a field rather than a package variable because tests run more than one
	// App in a process.
	trust            *machineTrust
	machineTrustOnce sync.Mutex
	// Files keeps the bytes somebody uploaded. The rows are in Store; this is
	// only where the bytes live.
	Files   *filestore.Store
	Gateway *provider.Gateway
	Agent   *agent.Runner
	// Templates are the recipes for native custom tools (the query template, and
	// later others). An administrator instantiates one into a tools row; the
	// loadout binds it back to a live handler.
	Templates *template.Registry
	// Runs owns the turns in flight. A turn outlives the request that started
	// it, so something other than the request has to hold it.
	Runs *run.Manager
	// WS is the bidirectional socket hub: every live client connection, and the
	// way any part of the server pushes a notification to a person. It is built
	// here and started by the server (its Run goroutine), so the worker, which
	// serves no sockets, never starts it.
	WS *ws.Hub
	// Link is the machine link: the chat applications connected right now, and
	// the connections carried through them to addresses this server cannot
	// reach itself (internal/link). Held here because the tool templates that
	// dial through it are built here, and the server mounts its two endpoints.
	Link *link.Registry
	// Machines is the same link, as the tools that run on somebody's computer
	// need it: something to call, and a way to ask what an installation can do.
	//
	// An interface beside the concrete registry, and not a helper that converts
	// one to the other, because the conversion has a trap in it: a nil
	// *Registry assigned to an interface is an interface that is NOT nil, and
	// every "can this reach a computer" check would then answer yes. Declared
	// once, set once, from the same registry every edition builds.
	//
	// It is also the seam a test replaces, which is how the turn's tools can be
	// tested without a websocket and a second process.
	Machines machine.Machines
	// Bus is the in-process event bus: how one part of the system reacts to what
	// another did without the two knowing each other (KB/30). Emitters Publish;
	// listeners (the live dashboard, later others) Subscribe. Started by the
	// server alongside the hub.
	Bus *events.Bus
	// live composes and pushes the dashboard's live picture, a listener on the
	// bus. Held so the stats endpoint reads the SAME snapshot the socket pushes.
	live *liveDashboard
	// bg owns the goroutines that run background-mode delegations (Mode C,
	// KB/27): the server drains it (RunBackground), the worker never starts it.
	bg *backgroundManager
	// ssh is the SSH tool template, held because it owns live connections to the
	// servers its tools work on. Shutdown closes them.
	ssh interface{ Close() }
	// State is the short-lived facts this system keeps between requests: how
	// far a batch has got, and anything else that could be written down and
	// read back. One interface, so the day there are two gateways it becomes a
	// shared store and nothing above it changes (internal/state).
	//
	// What is NOT here, and never will be: a live handle. A websocket, a cancel
	// function, an open connection. Those are held by the process that can use
	// them.
	State state.Store
	// Said is what people have typed into conversations that are still
	// answering, waiting to be folded into the turn at its next step.
	Said *Said

	// Queue carries wake-ups between this process and the workers (KB/05): a
	// job's truth is its database row, and this is only how somebody is told to
	// go and look. Nil when no broker is reachable, which is a real state rather
	// than an error: the server then does not offer work it cannot dispatch, and
	// a worker still finds jobs by scanning.
	Queue queue.Queue
	// fleets is what a WORKER is running of any batch, so a cancel broadcast
	// over the broker can stop it. Empty on a server, which runs no members.
	fleets *fleetRuns
	// fleetTracker is what the MASTER remembers about the batches it dispatched:
	// how many are back, and everything a chip needs. It is why hearing that one
	// more member has finished costs a socket write and nothing else.
	fleetTracker *fleetTracker
	// ModelRuntime is attached here as its first real implementation lands.
}

// ConnectQueue dials the broker and hands the app its queue. Called once at
// boot, by whichever mode the process is running in.
//
// A failure is returned rather than swallowed, and what to do about it is the
// caller's: a worker with no broker still works (it scans), a server with no
// broker simply cannot start a fleet and does not offer to.
func (a *App) ConnectQueue(ctx context.Context) error {
	q, err := queue.Connect(ctx, a.Config.NATSURL)
	if err != nil {
		return err
	}
	a.Queue = q
	return nil
}

// New fails rather than starting with a broken security configuration: a
// keyring that cannot be built would mean stored credentials silently become
// unreadable.
func New(cfg *config.Config, log zerolog.Logger, st store.Store) (*App, error) {
	if cfg.SessionSecretGenerated {
		log.Warn().Msg("SAG_SESSION_SECRET not set; using a per-boot secret (sessions reset on restart)")
	}
	if cfg.EncryptionKeysGenerated {
		log.Warn().Msg("SAG_ENCRYPTION_KEYS not set; using a per-boot key (stored credentials become unreadable after a restart)")
	}

	keyring, err := crypto.ParseKeyring(cfg.EncryptionKeys, cfg.EncryptionPrimaryKeyID)
	if err != nil {
		return nil, fmt.Errorf("encryption keys: %w", err)
	}

	// Where uploaded bytes go. It is prepared HERE, at startup, rather than on
	// the first upload: a deployment whose upload directory cannot be written to
	// should say so while somebody is watching it boot.
	files, err := filestore.New(cfg.UploadDir)
	if err != nil {
		return nil, fmt.Errorf("upload directory: %w", err)
	}

	a := &App{
		Config:   cfg,
		Log:      log,
		Store:    st,
		Tools:    tool.NewRegistry(),
		OAuth:    oauth.NewService(st.OAuth(), cfg.BaseURL),
		Sessions: auth.NewSessionManager(cfg.SessionSecret),
		Tokens:   auth.NewTokenService(cfg.SessionSecret),
		Keyring:  keyring,
		Files:    files,
		fleets:   newFleetRuns(),
		State:    state.NewMemory(),
	}
	// The short-lived facts this system keeps between requests, behind one
	// interface so the day there are two gateways they can be shared rather
	// than each having its own idea of what is going on (internal/state).
	a.fleetTracker = newFleetTracker(a.State)
	a.Said = newSaid(a.State)
	// The gateway opens vendor credentials through the app, which owns the
	// keyring: it never touches encryption itself. For the same reason it is
	// told how to reach a machine of ours rather than working it out: that
	// connection is authenticated with a certificate from the deployment's own
	// authority, and the key that signs those is the app's to hold.
	a.Gateway = provider.NewGateway(&storeLookup{store: st}, a)
	a.Gateway.UseMachineClient(a.MachineClient)

	// The socket surfaces authenticate with the same access token the REST API
	// does, just delivered in the connection's first message: a browser cannot
	// set an Authorization header on a WebSocket. One validator serves both the
	// notification hub and the machine link, so there is one answer to "who is
	// this" wherever a socket is opened.
	validate := func(token string) (int64, int64, bool) {
		claims, err := a.Tokens.ParseAccessToken(token)
		if err != nil {
			return 0, 0, false
		}
		// The same live re-check the REST path does: a revoked or no-longer-permitted
		// session cannot open a socket even with a still-unexpired token.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, err := a.Store.Sessions().ValidateAccess(ctx, claims.SessionID, claims.UserID, claims.WorkspaceID)
		if err != nil || !ok {
			return 0, 0, false
		}
		return claims.UserID, claims.WorkspaceID, true
	}
	// The machine link: the chat applications that are connected right now, and
	// the connections carried through them for a tool whose address this server
	// cannot reach (internal/link).
	//
	// On EVERY edition, the personal one included. It was left out there once,
	// reasoning that the gateway is already on the person's computer so a route
	// through the chat application would be a route from here to here. That is
	// true of one of the link's two far ends and false of the other (KB/39): a
	// CONNECTION reaches something only that computer can see, and a CALL's far
	// end is the application itself. The terminal and the file tools are calls.
	// Leaving the link out took them away from the one edition that is always
	// sitting on the person's own machine, and no screen said why.
	//
	// Machines is declared separately and assigned from the same value on
	// purpose. Assigning a nil *Registry to the interface would produce an
	// interface that is NOT nil, and every "is this offered here" check would
	// answer yes.
	a.Link = newLinkRegistry(a, log, cfg)
	a.Machines = a.Link
	machines := template.Machines(a.Link)

	// Tools take the app as their authorizer: a tool call is never more
	// powerful than the person who triggered it.
	if err := tools.Register(a.Tools, st, a, a.Machines, a.PersonZone); err != nil {
		return nil, fmt.Errorf("register tools: %w", err)
	}
	// The native tool templates this build ships, registered explicitly (the
	// same way the built-in tools are), so the set is named in one place.
	a.Templates = template.NewRegistry()
	a.Templates.Add(query.New(machines))
	// The SSH template holds the connections its tools work over, so it is kept
	// here to be closed at shutdown rather than left to the garbage collector.
	ssh := sshtool.New(machines)
	a.Templates.Add(ssh)
	a.ssh = ssh

	a.Agent = agent.NewRunner(st, a.Gateway, log)
	// A turn belongs to the run manager, not to the request that asked for it.
	a.Runs = run.NewManager(st, a.Agent, a.Said, log)
	// Background delegations run as goroutines this owns; the manager needs the
	// runner and the run manager, which now exist.
	a.bg = newBackgroundManager(a, log)
	// A completion turn has no request behind it, so the run manager tells us when
	// one starts and we nudge the person's tabs to attach (KB/27).
	a.Runs.OnServerTurn(a.notifyServerTurn)

	a.WS = ws.NewHub(log, validate, originHosts(cfg.AllowedOrigins))

	// The event bus and its first listener, the live dashboard. Emitters are
	// wired here so the reaction is set up in one place: a connection change
	// (from the hub) and a turn's start or end (from the run manager) become
	// events; the dashboard listens to those, plus the approval events the
	// background manager publishes, and pushes a fresh snapshot (KB/30).
	a.Bus = events.New(log)
	a.live = a.newLiveDashboard()
	// The emitter callbacks are set here (they only assign fields, never block).
	// The dashboard's SUBSCRIPTIONS are NOT: Subscribe blocks until the bus's Run
	// goroutine is up, and that starts in Serve, after construction. So the
	// dashboard subscribes in RunLiveDashboard, once the bus is running.
	a.WS.OnChange(func(workspaceID int64) {
		a.Bus.Publish(events.PresenceChanged{WorkspaceID: workspaceID})
	})
	a.Runs.OnTurnChange(func(started bool, workspaceID int64) {
		if started {
			a.Bus.Publish(events.TurnStarted{WorkspaceID: workspaceID})
		} else {
			a.Bus.Publish(events.TurnEnded{WorkspaceID: workspaceID})
		}
	})
	return a, nil
}

// Shutdown releases what the app itself holds open, as opposed to what a request
// or a turn holds. Today that is the connections the SSH tools work over: they
// outlive any one turn on purpose, so something has to end them.
func (a *App) Shutdown() {
	if a.ssh != nil {
		a.ssh.Close()
	}
}

// originHosts reduces the configured origin URLs to the host patterns the
// socket upgrader checks (production is same-origin and needs none).
func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

// newLinkRegistry builds the machine link with the same idea of who somebody is
// that every other socket uses, narrowed to a token minted for this and nothing
// else.
func newLinkRegistry(a *App, log zerolog.Logger, cfg *config.Config) *link.Registry {
	return link.NewRegistry(log, func(token string) (int64, int64, string, time.Time, bool) {
		// A link token and nothing else: an access token opens the API and must
		// not open a route into somebody's network, and the credential Rust
		// holds must not open the API.
		claims, err := a.Tokens.ParseLinkToken(token)
		if err != nil {
			return 0, 0, "", time.Time{}, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, err := a.Store.Sessions().ValidateAccess(ctx, claims.SessionID, claims.UserID, claims.WorkspaceID)
		if err != nil || !ok {
			return 0, 0, "", time.Time{}, false
		}
		// And WHEN it runs out, so the application can be asked for a new one
		// before that rather than after this side has dropped the socket.
		var expiresAt time.Time
		if claims.ExpiresAt != nil {
			expiresAt = claims.ExpiresAt.Time
		}
		return claims.UserID, claims.WorkspaceID, claims.DeviceID, expiresAt, true
	}, originHosts(cfg.AllowedOrigins))
}
