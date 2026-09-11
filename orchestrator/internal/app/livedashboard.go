package app

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/events"
	"flexie.io/sag/internal/ws"
)

// dashboardInterval coalesces live-dashboard pushes. However many things change
// in a workspace within it, its watchers get ONE push carrying the current
// snapshot, so a burst of turns and approvals is not a burst of messages.
const dashboardInterval = time.Second

// LiveDashboard is the workspace's live picture: who is connected, how many
// turns are running, how many approvals wait. It is the SAME shape whether it is
// read once from /stats on load or pushed over the socket as things change, so
// the two can never disagree.
type LiveDashboard struct {
	Connected       int `json:"connected"`
	Sessions        int `json:"sessions"`
	Running         int `json:"running"`
	WaitingApproval int `json:"waiting_approval"`
}

// liveDashboard composes the snapshot and pushes it. It is the one consumer of
// the event bus for the dashboard: it subscribes to the events that move a live
// number (a turn started or ended, an approval raised or answered, a person
// joined or left), coalesces them per workspace, rebuilds the snapshot from the
// authoritative sources, and broadcasts it. The sources are read fresh every
// time (never a counter that can drift), off the hub goroutine, so a database
// read never blocks delivery (KB/24, KB/29).
type liveDashboard struct {
	log zerolog.Logger

	// The authoritative sources, as functions so this is unit-testable without a
	// hub, a run manager, or a database.
	connected func(workspaceID int64) (users, sessions int)
	running   func(workspaceID int64) int
	waiting   func(ctx context.Context, workspaceID int64) (int, error)
	broadcast func(workspaceID int64, topic string, payload any)

	touch chan int64
}

// RunLiveDashboard subscribes the dashboard to the event bus and runs its
// publisher until ctx is cancelled. Subscribing here, not in app.New, is
// deliberate: it must happen once the bus's Run goroutine is up (which Serve
// starts just before this), or Subscribe would block on a bus that is not yet
// draining. The server starts this alongside the hub; the worker never does.
func (a *App) RunLiveDashboard(ctx context.Context) {
	a.live.Listen(a.Bus)
	a.live.Run(ctx)
}

// LiveDashboardSnapshot builds the workspace's current live picture, the same
// one the socket pushes. The stats endpoint returns it on load, so a dashboard
// is never blank or half-populated before the first push arrives.
func (a *App) LiveDashboardSnapshot(ctx context.Context, workspaceID int64) (LiveDashboard, error) {
	return a.live.Snapshot(ctx, workspaceID)
}

// newLiveDashboard wires the publisher to the app's live sources.
func (a *App) newLiveDashboard() *liveDashboard {
	return &liveDashboard{
		log: a.Log,
		connected: func(workspaceID int64) (int, int) {
			p := a.WS.Connected(workspaceID)
			return p.Users, p.Connections
		},
		running: a.Runs.Running,
		waiting: a.Store.Stats().WaitingApproval,
		broadcast: func(workspaceID int64, topic string, payload any) {
			a.WS.Broadcast(workspaceID, topic, payload)
		},
		// Buffered generously: a touch is dropped only if a whole second of
		// changes backs up unread, and a dropped touch is corrected by the next
		// one or by the snapshot /stats hands out on load.
		touch: make(chan int64, 1024),
	}
}

// Listen subscribes the dashboard to the events that move its numbers. Any of
// them, from anywhere, just means "workspace W changed": the snapshot is rebuilt
// wholesale, so the publisher never has to know what specifically happened.
func (d *liveDashboard) Listen(bus *events.Bus) {
	for _, kind := range []string{
		events.KindTurnStarted, events.KindTurnEnded,
		events.KindApprovalPending, events.KindApprovalResolved,
		events.KindPresenceChanged,
	} {
		bus.Subscribe(kind, func(e events.Event) {
			if we, ok := e.(events.WorkspaceEvent); ok {
				d.Touch(we.Workspace())
			}
		})
	}
}

// Touch marks a workspace as needing a fresh push. Non-blocking: a full buffer
// drops rather than stall the caller (which is a bus handler, or the hub).
func (d *liveDashboard) Touch(workspaceID int64) {
	select {
	case d.touch <- workspaceID:
	default:
	}
}

// Run coalesces touches and pushes one snapshot per changed workspace each tick,
// until ctx is cancelled.
func (d *liveDashboard) Run(ctx context.Context) {
	dirty := map[int64]struct{}{}
	ticker := time.NewTicker(dashboardInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case workspaceID := <-d.touch:
			dirty[workspaceID] = struct{}{}
		case <-ticker.C:
			for workspaceID := range dirty {
				d.push(ctx, workspaceID)
				delete(dirty, workspaceID)
			}
		}
	}
}

// Snapshot builds the current live picture for a workspace, reading each number
// from its authoritative source. It is what /stats returns and what a push
// carries, so the endpoint and the socket agree by construction.
func (d *liveDashboard) Snapshot(ctx context.Context, workspaceID int64) (LiveDashboard, error) {
	users, sessions := d.connected(workspaceID)
	waiting, err := d.waiting(ctx, workspaceID)
	if err != nil {
		return LiveDashboard{}, err
	}
	return LiveDashboard{
		Connected:       users,
		Sessions:        sessions,
		Running:         d.running(workspaceID),
		WaitingApproval: waiting,
	}, nil
}

func (d *liveDashboard) push(ctx context.Context, workspaceID int64) {
	snap, err := d.Snapshot(ctx, workspaceID)
	if err != nil {
		d.log.Warn().Err(err).Int64("workspace_id", workspaceID).Msg("live dashboard snapshot failed")
		return
	}
	d.broadcast(workspaceID, ws.TopicDashboard, snap)
}
