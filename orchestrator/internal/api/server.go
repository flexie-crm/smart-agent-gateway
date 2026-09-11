// Package api is the HTTP layer for server mode: REST API, SSE chat
// streaming, WebSocket hub, and the MCP surface. Handlers stay thin and
// delegate to the app layer.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"flexie.io/sag/internal/app"
)

// newRouter builds the full route tree. Tests drive this exact handler, so
// routing, middleware, and permission wiring are covered as they ship.
func newRouter(a *app.App) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// Log every request: method, path, status, and how long it took, so the
	// terminal shows what the server is doing. It sits outside Recoverer, so a
	// handler that panics is still logged, as the 500 it became.
	r.Use(requestLogger(a))
	// RealIP is deliberately NOT used: it rewrites RemoteAddr from
	// client-supplied forwarding headers, so any caller can forge the IP we
	// record on a session (GHSA-3fxj-6jh8-hvhx). RemoteAddr stays the true
	// peer. When SAG runs behind a proxy, the trusted-proxy chain must be
	// resolved explicitly rather than trusting whatever a client sends.
	r.Use(middleware.Recoverer)

	// Browsers calling from another origin (the console in development runs
	// on its own port and talks to this API directly) ask permission first.
	// Only origins the deployment explicitly allowed get an answer; with none
	// configured, the API is same-origin only and no CORS header ever leaves.
	// Credentials are ALLOWED, so a browser on an allowed origin may attach the
	// HttpOnly refresh cookie on the cross-origin auth calls. This is only legal
	// because the allow-list is always explicit origins, never "*": a credentialed
	// response must name one origin, which parseOrigins already guarantees.
	if len(a.Config.AllowedOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   a.Config.AllowedOrigins,
			AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
			AllowedHeaders:   []string{"Authorization", "Content-Type"},
			AllowCredentials: true,
			// Chromium caps preflight caching at two hours; ask for all of it
			// so a screen is not paying an extra round trip per endpoint.
			MaxAge: 7200,
		}))
	}

	r.Get("/healthz", handleHealth(a))
	mountOAuth(r, a)
	// The return leg of connecting an outbound MCP integration: a browser
	// redirect, so it lives outside /v1 and its bearer requirement.
	mountMCPCallback(r, a)
	// Our own MCP server: the surface an external agent connects to. Its
	// auth is the OAuth bearer, not the console's JWT, so it mounts at the
	// root with its own gate.
	mountMCPSurface(r, a)
	// A machine registering itself. It has no session and no token of ours, so
	// it cannot be inside the group that requires one.
	mountNodeJoin(r, a)
	r.Route("/v1", func(r chi.Router) {
		// What kind of installation this is, answered before anybody has signed
		// in because the sign-in screen itself depends on it.
		mountPosture(r, a)
		mountAuth(r, a)
		// The WebSocket authenticates on its first message, not on a header a
		// browser cannot set, so it lives outside the bearer-token group and
		// gates itself.
		r.Get("/ws", a.WS.Serve)
		// The machine link, for the same reason and in the same way: both of its
		// sockets authenticate on their first message (internal/link).
		//
		// On every edition. The personal one served no link once, reasoning
		// that its gateway is already on the person's computer; that is an
		// argument about one of the link's two far ends, and the tools that run
		// IN the application are the other (KB/39).
		if a.Link != nil {
			r.Get("/link", a.Link.ServeControl)
			r.Get("/link/stream", a.Link.ServeStream)
		}
		// Everything below requires a valid access token; each route
		// then states the permission it needs.
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(a))
			mountIdentity(r, a)
			mountLink(r, a)
			mountWorkspaces(r, a)
			mountAI(r, a)
			mountNodes(r, a)
			mountChat(r, a)
			mountChats(r, a)
			mountUploads(r, a)
			mountAudio(r, a)
			mountConfig(r, a)
			mountStats(r, a)
			mountSetup(r, a)
			mountBrains(r, a)
			mountMCPServers(r, a)
			mountMCPSettings(r, a)
			mountOAuthClients(r, a)
		})
		// Sessions and the MCP surface mount here as they
		// land.

		// An unknown API path is an API answer, never the console's page: a
		// misspelled endpoint must fail loudly, not load HTML into a client
		// that asked for JSON.
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		})
	})

	// With a built console configured, this server serves it too: the page
	// and the API are one origin from one process. Everything the API did
	// not claim above belongs to the console.
	//
	// A desktop has no proxy in front, so it configures both and one process
	// answers the API and both pages that call it. Order here does not matter:
	// chi matches on specificity, not registration, so /chat/* wins over the
	// console's /* whichever is declared first. (Asserted by swapping them: the
	// tests pass either way, which is why this does not claim otherwise.)
	if a.Config.ChatDir != "" {
		mountChatPage(r, a.Config.ChatDir)
	}
	if a.Config.InferenceDistDir != "" {
		mountInferenceDist(r, a.Config.InferenceDistDir)
	}
	if a.Config.ConsoleDir != "" {
		mountConsole(r, a.Config.ConsoleDir)
	}
	return r
}

// How long a starting server waits for the port, when something else still has
// it. A replacement taking over from a process that is still draining is the
// normal case on a restart, and the drain is bounded by SAG_SHUTDOWN_GRACE.
const bindWait = 90 * time.Second

// How often it tries again while waiting.
const bindRetry = 250 * time.Millisecond

// listen takes the port, waiting for it if the last process has not let go yet.
//
// Failing instantly on "address already in use" is the wrong answer for the one
// case that produces it in practice: a restart, where the outgoing process is
// shutting down gracefully and still holds the socket. It stops accepting first
// and drains what is in flight (KB/27), and a browser with the console open
// always has a socket to drain, so a restart is never instant. A replacement
// that gives up inside that window leaves NOTHING listening, and the next thing
// anybody sees is a page saying the gateway did not answer, with no clue why.
// That is a real failure this codebase has produced repeatedly under `make dev`.
//
// So it waits. If the port never frees, it says what to do about it rather than
// repeating the operating system's sentence.
func listen(ctx context.Context, a *app.App, addr string) (net.Listener, error) {
	var config net.ListenConfig
	deadline := time.Now().Add(bindWait)
	told := false

	for {
		listener, err := config.Listen(ctx, "tcp", addr)
		if err == nil {
			return listener, nil
		}
		if !addressInUse(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"%s is still in use after %s: another server is running on it, and this one cannot start until that one stops",
				addr, bindWait)
		}
		if !told {
			told = true
			a.Log.Warn().Str("addr", addr).
				Msg("the address is still in use, waiting for the previous server to let go")
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(bindRetry):
		}
	}
}

// Serve runs the HTTP server until ctx is cancelled, then shuts down
// gracefully.
func Serve(ctx context.Context, a *app.App) error {
	// The socket hub runs for the life of the server; on shutdown its Run
	// returns and closes every connection.
	go a.WS.Run(ctx)

	// The event bus and the live-dashboard publisher run alongside it: emitters
	// publish, the dashboard reacts and pushes snapshots (KB/30). Both stop when
	// ctx is cancelled.
	go a.Bus.Run(ctx)
	go a.RunLiveDashboard(ctx)

	// The background memory worker drains for the life of the server too: it
	// rewrites what the assistant remembers off the turn's path, so a turn never
	// waits on it. On shutdown its context is cancelled and it returns.
	go a.Agent.RunMemory(ctx)

	// Background delegations (Mode C, KB/27) run as goroutines the app owns. This
	// waits for shutdown, then drains them, so a process going down does not
	// abandon an agent mid-run.
	go a.RunBackground(ctx)

	// The fleet join (Mode B, KB/27): hear a worker report that one agent of a
	// batch is back, count the rest from the database, and when the last one is
	// in, wake the Gateway once with all of it. It also sweeps, so a lost report
	// is a slower answer rather than a conversation left waiting.
	go a.RunFleetJoin(ctx)

	srv := &http.Server{
		Addr:              a.Config.HTTPAddr,
		Handler:           newRouter(a),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := listen(ctx, a, a.Config.HTTPAddr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		a.Log.Info().Str("addr", a.Config.HTTPAddr).Msg("http server listening")
		errCh <- srv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		return shutdown(a, srv)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// shutdown brings the server down without throwing away work in progress.
//
// The order is the whole of it:
//
//  1. STOP ACCEPTING. Quiesce refuses new turns and new background agents, and
//     the HTTP server stops taking connections. Anything that has not started
//     is better refused than started into a process that is going away.
//  2. LET IT FINISH. Everything already under way gets SAG_SHUTDOWN_GRACE to
//     end on its own. This is the part that was missing: shutdown used to
//     cancel every live turn immediately, so an ordinary deploy interrupted
//     answers people were reading and left the ghosts that boot recovery then
//     had to clean up.
//  3. GIVE UP HONESTLY. Whatever is still going when the grace runs out is
//     cancelled, and every goroutine is waited for, so the outcome is recorded
//     before the process exits rather than inferred by the next one.
//
// Every wait here is a CEILING, not a delay. `srv.Shutdown` returns the moment
// connections go idle, and both drains count the work before they look at the
// clock, so a process with nothing in flight goes down at once and only one that
// is genuinely mid-answer spends any of the grace. Nothing waits for ever
// either: a deploy that hangs is its own outage.
func shutdown(a *app.App, srv *http.Server) error {
	grace := a.Config.ShutdownGrace
	turns, agents := a.WorkInFlight()
	a.Log.Info().Int("turns", turns).Int("agents", agents).Str("grace", grace.String()).
		Msg("shutting down: no new work, finishing what is under way")

	// 1. No new work, and no new connections. The HTTP drain is bounded
	// separately and short: a request in flight is a person waiting on a socket,
	// not a turn, and a turn outlives its request by design (KB/17).
	a.Quiesce()
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHTTP()
	err := srv.Shutdown(httpCtx)

	// 2. Let the work finish.
	if grace > 0 {
		workCtx, cancelWork := context.WithTimeout(context.Background(), grace)
		defer cancelWork()
		if a.DrainWork(workCtx) {
			a.Log.Info().Msg("all work finished")
			return err
		}
	}

	// 3. Out of patience. What is left is cancelled and settled on the way down.
	turns, agents = a.WorkInFlight()
	a.Log.Warn().Int("turns", turns).Int("agents", agents).
		Msg("shutdown grace expired: interrupting what is still running")
	return err
}

// requestLogger writes one line per request. The health check is logged at
// debug, not info: a load balancer polls it constantly, and that noise would
// bury the requests a person actually wants to see.
func requestLogger(a *app.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			event := a.Log.Info()
			if r.URL.Path == "/healthz" {
				event = a.Log.Debug()
			}
			event.
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", ww.Status()).
				Str("took", time.Since(started).Round(time.Millisecond).String()).
				Str("req_id", middleware.GetReqID(r.Context())).
				Msg("request")
		})
	}
}

func handleHealth(a *app.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbStatus := "ok"
		status := http.StatusOK
		if err := a.Store.Ping(r.Context()); err != nil {
			dbStatus = "unreachable"
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": dbStatus})
	}
}
