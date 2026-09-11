package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// ErrBusy is returned when a conversation is already answering. One
// conversation runs one turn at a time: two streams interleaved into one
// transcript would leave neither making sense.
var ErrBusy = errors.New("run: this conversation is already answering")

// ErrShuttingDown is returned once a restart has begun. Work already under way
// is allowed to finish (that is the whole of graceful shutdown); work that has
// not started is refused, because starting it now guarantees interrupting it.
var ErrShuttingDown = errors.New("run: this service is restarting")

// runTimeout is the longest a turn may take. It is generous, because a turn
// that calls several slow tools is doing its job, and it is finite, because a
// vendor that never answers must not hold a goroutine forever.
const runTimeout = 30 * time.Minute

// retention is how long a finished run stays replayable in memory. It covers
// the gap a person actually experiences: the answer lands, the laptop wakes up,
// the tab reconnects. After that the conversation is the record, and the
// transcript has all of it.
const retention = 5 * time.Minute

// Runner is what executes a turn. The manager owns when and where it runs; the
// runner owns what it does.
type Runner interface {
	Run(ctx context.Context, turn agent.Turn, out *chat.Stream) error
}

// Said is where words typed into a conversation that is already answering wait
// to be picked up. The manager only takes from it; who puts things in is the
// application's business.
type Said interface {
	Take(ctx context.Context, sessionID int64) []string
}

// Manager owns the live runs.
//
// It is deliberately the only thing that knows a turn is executing somewhere.
// The API asks it to start one and to find one; it never holds a goroutine
// itself, so an HTTP handler returning cannot end a turn.
type Manager struct {
	said   Said
	store  store.Store
	runner Runner
	log    zerolog.Logger

	mu     sync.RWMutex
	byUID  map[string]*Run
	byChat map[int64]*Run
	// quiescing is set the moment a restart begins: no new turn is accepted
	// after it, and the queued ones are dropped rather than started into a
	// process that is going away.
	quiescing bool
	// pending holds SERVER-INITIATED turns waiting for a session's lane: today,
	// background-delegation completions (KB/27). A user turn is refused when the
	// lane is busy (ErrBusy); a completion instead WAITS its slot, because its
	// work is already done and just needs delivering. One queue per session,
	// drained in order, so completions serialize and never barge a user turn.
	pending map[int64][]agent.Turn

	// onServerTurn is called when a SERVER-INITIATED turn starts (a completion,
	// KB/27), so a client with no request in flight can be told to attach and hear
	// it. Set by the app; nil elsewhere, where it is simply not called.
	onServerTurn func(*Run)

	// onTurnChange is called when a turn starts (true) and when it ends (false),
	// so a listener (the live dashboard, KB/30) can react to the running count
	// moving. Set by the app; nil elsewhere.
	onTurnChange func(started bool, workspaceID int64)
}

func NewManager(st store.Store, runner Runner, said Said, log zerolog.Logger) *Manager {
	return &Manager{
		store:   st,
		runner:  runner,
		said:    said,
		log:     log,
		byUID:   make(map[string]*Run),
		byChat:  make(map[int64]*Run),
		pending: make(map[int64][]agent.Turn),
	}
}

// OnServerTurn registers the callback fired when a server-initiated turn starts.
// It is how the app learns a completion turn is streaming, so it can nudge the
// person's tabs to attach (KB/27). Set once at wiring time.
func (m *Manager) OnServerTurn(fn func(*Run)) { m.onServerTurn = fn }

// OnTurnChange registers the callback fired when a turn starts and ends, so the
// app can announce the running count moved (KB/30). Set once at wiring time.
func (m *Manager) OnTurnChange(fn func(started bool, workspaceID int64)) { m.onTurnChange = fn }

func (m *Manager) turnChanged(started bool, workspaceID int64) {
	if m.onTurnChange != nil {
		m.onTurnChange(started, workspaceID)
	}
}

// Start begins a turn and returns immediately.
//
// The context passed in is used only to create the run. The turn itself gets a
// context of its own, deliberately severed from the caller's: the whole point
// is that the reader hanging up does not cancel the work.
func (m *Manager) Start(ctx context.Context, turn agent.Turn) (*Run, error) {
	m.mu.RLock()
	stopping := m.quiescing
	m.mu.RUnlock()
	if stopping {
		return nil, ErrShuttingDown
	}
	uid, err := model.NewRunUID()
	if err != nil {
		return nil, err
	}
	record := &model.Run{
		UID:         uid,
		WorkspaceID: turn.WorkspaceID,
		SessionID:   turn.SessionID,
		UserID:      turn.UserID,
		Status:      model.RunRunning,
	}
	if err := m.store.Runs().Create(ctx, record); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}

	// Severed from the caller, on purpose. context.WithoutCancel keeps the
	// values (the request id a log line is tagged with) and drops the
	// cancellation, which is exactly the distinction we want: the turn should
	// still be traceable to the request that asked for it, and should not die
	// with it.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)

	r := newRun(record.ID, uid, turn.SessionID, turn.UserID, turn.WorkspaceID, cancel)

	m.mu.Lock()
	if previous, ok := m.byChat[turn.SessionID]; ok && !previous.Done() {
		// One conversation, one turn at a time. A second prompt while the first
		// is still answering would interleave two streams into one transcript,
		// and neither would make sense.
		m.mu.Unlock()
		cancel()
		return nil, ErrBusy
	}
	m.byUID[uid] = r
	m.byChat[turn.SessionID] = r
	m.mu.Unlock()

	// The turn can hear the person while it runs. Every turn started here can,
	// which is why it is attached here and not at each of the places a turn is
	// built: an answer, a resumed park and a completion turn are all turns of the
	// same conversation, and something typed into it belongs to whichever one is
	// running.
	//
	// A detached context, for the same reason the run has one: the request that
	// started this turn is long gone by the time a step asks.
	if m.said != nil {
		heard := context.WithoutCancel(ctx)
		sessionID := turn.SessionID
		turn.Heard = func() []string { return m.said.Take(heard, sessionID) }
	}

	go m.execute(runCtx, cancel, r, turn)
	return r, nil
}

// Answering says whether a conversation has a turn running right now, which is
// what decides whether something typed goes INTO that turn or starts one of its
// own.
//
// A race nobody can close sits under this: a turn can finish between the answer
// and the caller acting on it. That is why anything left unheard when a turn
// ends is started as a turn of its own (see settle), rather than the caller
// being asked to get the timing right.
func (m *Manager) Answering(sessionID int64) bool {
	m.mu.RLock()
	r, ok := m.byChat[sessionID]
	m.mu.RUnlock()
	return ok && !r.Done()
}

// Schedule queues a SERVER-INITIATED turn to run on its session's lane when the
// lane is free, in the order scheduled. Unlike Start it never returns ErrBusy:
// the work behind it (a finished background delegation, KB/27) is already done
// and must be delivered, so it waits its slot rather than being refused. It is
// NOT for user prompts, which are refused when the lane is busy.
func (m *Manager) Schedule(turn agent.Turn) {
	m.mu.Lock()
	m.pending[turn.SessionID] = append(m.pending[turn.SessionID], turn)
	m.mu.Unlock()
	m.startNextPending(turn.SessionID)
}

// Drain starts whatever a session has queued, if anything, now that a reason to
// hold it back may have gone.
//
// A queued turn is refused while the conversation is waiting on a person, and
// the only things that try again are another Schedule and a turn finishing. That
// leaves a queued turn stranded whenever a card is answered and nothing else
// follows: the work is done, the chip is settled, and the Gateway never says so.
// Answering a card calls this, so the queue heals itself rather than depending
// on who happens to write the session's status last.
func (m *Manager) Drain(sessionID int64) { m.startNextPending(sessionID) }

// startNextPending starts the next queued turn for a session if its lane is free
// and it is not waiting on a person. Called when a turn is scheduled, when a
// turn finishes, and when a card is answered, so a queue drains itself, one turn
// at a time.
func (m *Manager) startNextPending(sessionID int64) {
	m.mu.Lock()
	if r, ok := m.byChat[sessionID]; ok && !r.Done() {
		// A turn is running; its finish will call us again.
		m.mu.Unlock()
		return
	}
	if len(m.pending[sessionID]) == 0 {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Do not deliver a background result while a card is on screen: the person is
	// mid-decision. The park's resume, when it finishes, calls us again.
	//
	// It asks whether a card IS on screen, not whether the session's status says
	// one is. Those are different questions, and the day they disagreed the
	// conversation stopped for good: a restart killed three parked agents, their
	// parks were answered later, and the status column was left reading
	// `waiting_approval` with nothing pending behind it. Every completion turn
	// queued from then on hit this gate and returned, forever, with no card
	// anywhere for the person to answer and no way to clear it. A status is a
	// cached opinion; the pending park is the fact. Ask the fact.
	sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := m.store.Agent().PendingPark(sc, sessionID)
	cancel()
	if err == nil {
		return // a card really is waiting on a person
	}
	if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrExpired) {
		// The park could not be read. Deliver rather than stall: a completion
		// turn arriving next to a card is a cosmetic collision, and a
		// conversation that never speaks again is not.
		m.log.Warn().Err(err).Int64("session_id", sessionID).Msg("drain: read pending park")
	}

	// Claim the slot. Popping under the lock keeps two concurrent drainers from
	// starting the same turn; if a user turn slipped onto the lane, Start's own
	// busy check fails and we re-queue at the front, and that user turn's finish
	// will retry us.
	m.mu.Lock()
	if len(m.pending[sessionID]) == 0 {
		m.mu.Unlock()
		return
	}
	next := m.pending[sessionID][0]
	m.pending[sessionID] = m.pending[sessionID][1:]
	m.mu.Unlock()

	if _, err := m.Start(context.Background(), next); err != nil {
		m.mu.Lock()
		m.pending[sessionID] = append([]agent.Turn{next}, m.pending[sessionID]...)
		m.mu.Unlock()
	}
}

// execute runs the turn to its end, whatever happens to the readers.
func (m *Manager) execute(ctx context.Context, cancel context.CancelFunc, r *Run, turn agent.Turn) {
	defer cancel()

	// Logged the moment the turn starts running, not when the stream finishes,
	// so a prompt is visible in the logs the instant it arrives.
	m.log.Info().Str("run", r.UID).Int64("session_id", r.SessionID).
		Int64("workspace_id", r.WorkspaceID).Int64("model_id", turn.ModelID).
		Msg("turn started")
	m.turnChanged(true, r.WorkspaceID)

	// A server-initiated turn has no request listening to it. Tell the person's
	// tabs it started, so they attach and hear it (KB/27).
	if turn.ServerInitiated() && m.onServerTurn != nil {
		m.onServerTurn(r)
	}

	// Only what this person may see. Nothing said means everything, so a turn
	// from a path with no opinion is unaffected.
	show := chat.Everything()
	if turn.Show != nil {
		show = *turn.Show
	}
	out := chat.NewStreamShowing(r, show)
	err := m.runner.Run(ctx, turn, out)

	// The context is checked FIRST, and regardless of the error. A cancelled
	// stream often just ends, with no error to report, and calling that a
	// completed answer would record a sentence the model never finished as
	// though it had.
	status := model.RunCompleted
	switch {
	case ctx.Err() != nil:
		status = model.RunCancelled
	case err != nil:
		status = model.RunFailed
		m.log.Error().Err(err).Str("run", r.UID).Int64("session_id", r.SessionID).
			Msg("run failed")
	}

	// Detached from the run's own context, which by now may be exactly the
	// thing that died.
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()

	// A parked turn has not finished: it is waiting on a person, and a new run
	// picks it up when they answer.
	if session, serr := m.store.Agent().GetSession(settleCtx, r.WorkspaceID, r.SessionID); serr == nil &&
		session.Status == model.SessionWaitingApproval {
		status = model.RunWaitingApproval
	}

	if status == model.RunCancelled {
		// Whoever is still listening is told the turn was stopped, rather than
		// left watching a stream that simply goes quiet. The words already said
		// are kept: they are on the transcript, marked unfinished.
		_ = out.Error("This turn was stopped.")
		if serr := m.store.Agent().SetSessionStatus(settleCtx, r.SessionID, model.SessionCancelled); serr != nil {
			m.log.Error().Err(serr).Str("run", r.UID).Msg("mark session cancelled")
		}
	}

	m.settle(r, status)

	// The lane is free again. Drain the next queued completion turn, unless this
	// turn parked, in which case the session is waiting on a person and its
	// resume will drain the queue instead.
	if status != model.RunWaitingApproval {
		m.startNextPending(r.SessionID)
	}
}

// settle closes the run: no more frames, the record says how it ended, and the
// log stays readable for a while so a client coming back still sees the answer.
func (m *Manager) settle(r *Run, status string) {
	m.log.Info().Str("run", r.UID).Int64("session_id", r.SessionID).
		Str("status", status).Msg("turn finished")
	m.turnChanged(false, r.WorkspaceID)

	r.mu.Lock()
	r.finish()
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Runs().SetStatus(ctx, r.ID, status); err != nil {
		m.log.Error().Err(err).Str("run", r.UID).Msg("record run status")
	}

	time.AfterFunc(retention, func() { m.evict(r) })
}

func (m *Manager) evict(r *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byUID, r.UID)
	if current, ok := m.byChat[r.SessionID]; ok && current == r {
		delete(m.byChat, r.SessionID)
	}
}

// Get finds a run by its public id.
func (m *Manager) Get(uid string) (*Run, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.byUID[uid]
	return r, ok
}

// Latest finds the most recent run of a conversation, whether it is still going
// or has just finished. It is what a reloaded page attaches to.
func (m *Manager) Latest(sessionID int64) (*Run, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.byChat[sessionID]
	return r, ok
}

// Live is what is happening at this moment.
//
// It is read from memory, because the runs ARE here: the process answering this
// question is the process running the turns. That is exact today and it will
// stop being exact the moment a second node runs turns of its own, which is why
// callers ask the manager rather than counting rows: when the queue lands, this
// is the one function that changes (KB/17).
// Running counts the turns being answered at this moment.
//
// A turn WAITING on a person is deliberately not counted here, and not counted
// from memory at all: it is not running, it holds nothing, it can sit for hours,
// and it survives a restart of this process. It is a row (an unexpired pending
// park), and the database is the only thing that can answer for it honestly.
func (m *Manager) Running(workspaceID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	running := 0
	for _, r := range m.byUID {
		if r.WorkspaceID == workspaceID && !r.Done() {
			running++
		}
	}
	return running
}

// Cancel stops the conversation's current turn. It reports whether there was
// one to stop.
func (m *Manager) Cancel(sessionID int64) bool {
	r, ok := m.Latest(sessionID)
	if !ok || r.Done() {
		return false
	}
	r.Cancel()
	return true
}

// Shutdown ends every run and waits for the records to settle. A process going
// down cleanly should not leave rows claiming to be running forever.
// Quiesce stops the manager accepting work. Turns already running are left
// exactly alone: they are the thing shutdown is trying to protect.
func (m *Manager) Quiesce() {
	m.mu.Lock()
	m.quiescing = true
	// Queued server-initiated turns have not started, so dropping them costs
	// nothing: the delegation whose result they were going to deliver is still
	// recorded, and the next process picks it up.
	m.pending = map[int64][]agent.Turn{}
	m.mu.Unlock()
}

// DrainForShutdown waits for the turns still answering, and reports whether
// they all finished. It cancels nothing: that is Shutdown's job, and it is what
// happens when this runs out of patience. (Not to be confused with Drain, which
// starts a session's queued turn.)
func (m *Manager) DrainForShutdown(ctx context.Context) bool {
	for {
		if m.Live() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Live counts the turns still answering.
func (m *Manager) Live() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, r := range m.byUID {
		if !r.Done() {
			n++
		}
	}
	return n
}

// Shutdown cancels whatever is still answering. It is the END of a graceful
// shutdown, not the whole of it: Quiesce and Drain come first, and by the time
// this runs the only turns left are the ones that would not finish in the time
// we were willing to give them.
func (m *Manager) Shutdown() {
	m.mu.RLock()
	runs := make([]*Run, 0, len(m.byUID))
	for _, r := range m.byUID {
		runs = append(runs, r)
	}
	m.mu.RUnlock()

	for _, r := range runs {
		if !r.Done() {
			r.Cancel()
		}
	}
}

// RecoverOrphans marks the runs that a dead process left behind.
//
// A run whose process died is not running, and a row that says otherwise is a
// lie that never expires. The conversation keeps whatever the checkpoint saved,
// which is the answer up to the moment the lights went out, and the run says it
// was interrupted rather than pretending the silence was an answer.
func RecoverOrphans(ctx context.Context, st store.Store, log zerolog.Logger) error {
	n, err := st.Runs().MarkInterrupted(ctx)
	if err != nil {
		return fmt.Errorf("recover orphaned runs: %w", err)
	}
	if n > 0 {
		log.Warn().Int64("runs", n).Msg("runs interrupted by a previous shutdown")
	}
	// And the individual tool calls inside those runs. A run row is not the only
	// thing that claims to be in progress: each call writes itself as running
	// before it starts and is resolved when it returns, so a process that dies
	// mid-call, or a turn that parks for approval and therefore never runs the
	// calls queued behind the parked one, leaves a row saying "running" for good.
	// The transcript renders that as a spinner, beside a tool nothing is doing.
	calls, err := st.Agent().MarkToolCallsInterrupted(ctx)
	if err != nil {
		return fmt.Errorf("recover orphaned tool calls: %w", err)
	}
	if calls > 0 {
		log.Warn().Int64("tool_calls", calls).Msg("tool calls interrupted by a previous shutdown")
	}
	return nil
}
