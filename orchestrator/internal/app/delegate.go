package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"flexie.io/sag/internal/agent"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools"
	"flexie.io/sag/internal/tools/toolkit"
)

// The Gateway's side of delegation: the app decides which agents exist, how
// they are described to the Gateway, and what one runs as when the Gateway routes
// to it. The runtime (internal/agent) only executes what this hands it.

// agentInfo is one agent as the Gateway needs to see it to ROUTE to it:
// the key it routes by, the name, the FULL instructions the administrator wrote
// (so the Gateway knows what it is for, not just the first line), and its tools
// as names + a short line (so the Gateway knows what it can reach). The tool's
// deep guide is NOT here: that belongs to the agent, read when it runs. The
// agent's hardcoded prompt frame is likewise not here; only what the
// administrator entered.
type agentInfo struct {
	Key          string
	Name         string
	Instructions string
	Tools        []toolBrief
	// DelegationMode pins how the Gateway runs this agent (KB/27): background,
	// inline, or auto (the Gateway decides). It shapes both the roster text and the
	// delegate tool's mode parameter.
	DelegationMode string
}

// toolBrief is an agent's tool as the Gateway sees it in the roster: its name
// and the one-line, model-facing description, never the deep how-to guide.
type toolBrief struct {
	Name        string
	Description string
}

// agents lists the agents a Gateway may route to: every active agent
// that is not the Gateway itself, with the instructions it was given and the
// tools it holds, so the Gateway can route on real information. A workflow can
// narrow this later; today the roster is simply the workspace's own agents.
func (a *App) agentRoster(ctx context.Context, workspaceID, userID int64) ([]agentInfo, error) {
	agents, err := a.Store.Agents().List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	infos := make([]agentInfo, 0, len(agents))
	for _, ag := range agents {
		if ag.Key == model.DefaultAgentKey || ag.Status != model.StatusActive {
			continue
		}
		// The agent's own tools, resolved as it would get them, so the Gateway
		// sees exactly what it can reach: names and the short description, not the
		// deep guide. This loadout is read, never called, so it belongs to no agent.
		loadout, err := a.Loadout(ctx, workspaceID, userID, "", ag.Tools, ag.ConfirmTools, ag.Brains, tool.OwnerNone)
		if err != nil {
			return nil, fmt.Errorf("resolve agent %q tools: %w", ag.Key, err)
		}
		briefs := make([]toolBrief, 0, len(loadout.Schemas))
		for _, s := range loadout.Schemas {
			// The FRIENDLY name, never the callable key: the key (e.g.
			// "http_request") reads to the Gateway as a tool IT can call, and it
			// hallucinates the call, which fails because the tool is the
			// agent's, not the Gateway's. A friendly name is a capability, not
			// an invocation.
			name := s.FriendlyName
			if name == "" {
				name = s.Name
			}
			briefs = append(briefs, toolBrief{Name: name, Description: s.Description})
		}
		infos = append(infos, agentInfo{
			Key:            ag.Key,
			Name:           ag.Name,
			Instructions:   strings.TrimSpace(ag.Instructions),
			Tools:          briefs,
			DelegationMode: ag.DelegationMode,
		})
	}
	return infos, nil
}

// ResolveAgent resolves what one agent runs as when the Gateway routes a
// task to it: its own prompt, model, reasoning, and tools. The delegate tool is
// never among them (an agent cannot delegate). An unknown or inactive key is
// store.ErrNotFound.
// deviceID is which of the person's computers the turn came from, carried so a
// delegated agent can reach it too: an agent works on the person's behalf, and
// what they can reach it can reach.
func (a *App) ResolveAgent(ctx context.Context, workspaceID, userID int64, c Computer, key string) (agent.AgentProfile, error) {
	if key == model.DefaultAgentKey {
		return agent.AgentProfile{}, store.ErrNotFound
	}
	ag, err := a.Store.Agents().GetByKey(ctx, workspaceID, key)
	if err != nil {
		return agent.AgentProfile{}, err
	}
	if ag.Status != model.StatusActive {
		return agent.AgentProfile{}, store.ErrNotFound
	}

	// The agent carries its own approval set: an approval-gated tool it holds
	// parks the whole turn and asks, exactly as the Gateway's does (agent park,
	// KB/11). Its confirm set is passed through like the Gateway's is.
	// One owner per resolution, which is one per running agent: this is
	// called once when the Gateway routes to it and the loadout lives for the
	// whole delegation. Ten background agents get ten owners whether they
	// are ten different agents or ten copies of one, so a session one of them
	// opens can never be reached by another (tool.Owner).
	loadout, err := a.Loadout(ctx, workspaceID, userID, c.DeviceID, ag.Tools, ag.ConfirmTools, ag.Brains, tool.OwnerOfAgent())
	if err != nil {
		return agent.AgentProfile{}, err
	}
	// An agent is a full agent: if it has its own memory brain, it manages it
	// exactly as the Gateway does, the same internal tool wired the same way.
	tools.AppendMemory(&loadout, a.Store, ag.MemoryBrainID)

	// Its own knowledge and memory brains, resolved into the map its prompt shows.
	brains, err := a.brainRosterFor(ctx, workspaceID, ag.Brains, ag.MemoryBrainID)
	if err != nil {
		return agent.AgentProfile{}, err
	}

	// The agent gets the same assembled frame as the Gateway, wrapped around
	// its own instructions as its role: it cannot delegate (no agents), but a
	// terminal answer reaches the person, so it must speak in the same voice,
	// under the same communication rules.
	system := renderAgent(agentPrompt{
		now:          time.Now(),
		zone:         a.PersonZone(ctx, userID),
		role:         ag.Instructions,
		capabilities: loadout.Schemas,
		brains:       brains,
		folder:       c.Folder,
		machine:      c.Env,
	})
	modelID := int64(0)
	if ag.ModelID != nil {
		modelID = *ag.ModelID
	}
	profile := agent.AgentProfile{
		Key:            ag.Key,
		Name:           ag.Name,
		SystemPrompt:   system,
		Tools:          loadout,
		ModelID:        modelID,
		Reasoning:      ag.Reasoning,
		Settings:       ag.Settings,
		DelegationMode: ag.DelegationMode,
	}
	if ag.MaxIterations != nil {
		profile.MaxIterations = *ag.MaxIterations
	}
	if ag.BackgroundTimeout != nil {
		profile.BackgroundTimeout = *ag.BackgroundTimeout
	}
	return profile, nil
}

// AgentResolver binds a turn to the app's agent resolution, so the loop
// can run a delegation without knowing how an agent is configured. It is
// harmless on a turn whose Gateway has no agents: the delegate tool is not
// in the loadout, so the model never calls it and this is never invoked.
// deviceID is the computer the turn came from, so an agent the Gateway hands
// work to can reach it too: an agent acts on the person's behalf, and what they
// can reach it can reach.
func (a *App) AgentResolver(workspaceID, userID int64, c Computer) agent.AgentResolver {
	return func(ctx context.Context, key string) (agent.AgentProfile, error) {
		return a.ResolveAgent(ctx, workspaceID, userID, c, key)
	}
}

// StartBackground launches a background delegation's owned goroutine (Mode C,
// KB/27). A turn carries it as its StartBackground seam; the runtime calls it
// from delegate() when the Gateway chooses background mode. The passed context
// bounds only the synchronous launch, never the goroutine: the work lives on the
// app's own base context and is drained on shutdown, so the Gateway's turn ending
// does not cancel the agent.
func (a *App) StartBackground(_ context.Context, bg agent.BackgroundDelegation) {
	a.bg.launch(bg)
}

// RunBackground blocks until ctx is cancelled, then drains the in-flight
// background delegations. The server owns it (one goroutine, alongside the
// memory worker and the socket hub); the worker, which runs no turns, never
// starts it.
func (a *App) RunBackground(ctx context.Context) {
	<-ctx.Done()
	a.bg.shutdown()
}

// Quiesce stops the app taking on new work: no new turn, no new background
// agent. What is already running is untouched, which is the point.
func (a *App) Quiesce() {
	a.Runs.Quiesce()
	a.bg.quiesce()
}

// DrainWork waits for everything already under way to finish, and reports
// whether it did. Bounded by the caller's context: nothing waits for ever.
func (a *App) DrainWork(ctx context.Context) bool {
	agentsDone := a.bg.drain(ctx)
	turnsDone := a.Runs.DrainForShutdown(ctx)
	return agentsDone && turnsDone
}

// WorkInFlight reports what is still going, for the shutdown log.
func (a *App) WorkInFlight() (turns, agents int) {
	a.bg.mu.Lock()
	agents = len(a.bg.running)
	a.bg.mu.Unlock()
	return a.Runs.Live(), agents
}

// RecoverInterruptedDelegations closes out and narrates the background
// delegations a previous process left running (KB/27). A restart kills the
// goroutines but not their rows, so this settles each as failed and fires the
// Gateway's completion turn to tell the person it did not finish. Called once at
// boot, before anything can legitimately run, and reports how many it recovered.
func (a *App) RecoverInterruptedDelegations(ctx context.Context) (int, error) {
	return a.bg.recoverInterrupted(ctx)
}

// CancelBackground stops a background delegation and reports whether there was
// one to stop. A running delegation ends without a completion turn; the person
// asked for it to stop, so the Gateway does not narrate a result (KB/27).
func (a *App) CancelBackground(delegationID int64) bool {
	return a.bg.cancel(delegationID)
}

// CancelSessionBackground stops every background delegation a conversation still
// has running, and reports how many. It is called when a chat is deleted, so
// removing a conversation kills the agents working under it.
func (a *App) CancelSessionBackground(sessionID int64) int {
	return a.bg.cancelSession(sessionID)
}

// ReleaseNextApprovalCard surfaces the conversation's next queued background
// approval card, if any, after one was answered (KB/27). A no-op when the queue
// is empty.
func (a *App) ReleaseNextApprovalCard(sessionID int64) {
	a.bg.releaseNextCard(sessionID)
}

// DrainApprovalQueue lets every queued background agent through as approved
// with no card, which is what "approve all" does for a conversation (KB/27).
func (a *App) DrainApprovalQueue(sessionID int64) {
	a.bg.drainApprovalQueue(sessionID)
}

// ResumeBackground continues a background agent that stopped for approval,
// after the person answered its card (KB/27). It re-launches the agent as a
// goroutine; on finish the delegation is recorded and the Gateway's completion
// turn is scheduled, exactly as the first leg was.
func (a *App) ResumeBackground(snapshot *model.ParkSnapshot, approved bool) {
	a.bg.resume(snapshot, approved)
}

// backgroundTools are the built-in tools that let the Gateway watch and stop its
// own background delegations (Mode C, KB/27). They are internal (no grant,
// invisible to the admin UI) and offered only to a Gateway that can delegate, so
// they ride alongside the delegate tool.
func (a *App) backgroundTools() []struct {
	schema  tool.Schema
	handler tool.Handler
} {
	statusSchema, statusHandler := a.backgroundStatusTool()
	cancelSchema, cancelHandler := a.cancelBackgroundTaskTool()
	return []struct {
		schema  tool.Schema
		handler tool.Handler
	}{
		{statusSchema, statusHandler},
		{cancelSchema, cancelHandler},
	}
}

// backgroundStatusSchema is what the model sees of it, apart from the handler so
// that everything the loop adds to a loadout itself can be enumerated in one
// place (loopInternal).
func backgroundStatusSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "Optional. The running id of ONE agent to report on. Omit to report on every agent you started in this conversation.",
			},
		},
	})
	schema := tool.Schema{
		Name:              model.BackgroundStatusToolName,
		FriendlyName:      "Background agent status",
		FriendlyNarration: "Checking background agent status",
		Description: "Report how the background agents in this conversation are going, for when the PERSON asks for a progress update or a count. " +
			"With an `id`, returns that one agent's status. With NO id, returns how many of the ones you started are running, finished, failed, or cancelled, plus each one's status. " +
			"The `internal_id` in the reply is for YOUR use only, to ask after one agent or to stop it: NEVER show it to the person and never refer to an agent by it. " +
			"When you tell somebody what is still outstanding, say what each agent is DOING, in your own words. " +
			"Call this ONLY when the person asks how the background work is going. You never need it to collect results or to know which finished: that is already recorded in this conversation and you are brought back automatically the moment one finishes. Do not call it on your own initiative, and never to poll. " +
			"It only reads; it never affects the work. Never mention this tool or its name to the person.",
		InputSchema: input,
		Kind:        tool.KindInternal,
		Risk:        tool.RiskReadOnly,
	}
	return schema
}

// backgroundStatusTool answers, for the person, how the background agents are
// going: one by its running id, or a count of the set. It only reads the durable
// records, so it never disturbs the work. It is NOT how the Gateway learns its own
// results: each finished agent's result is already recorded on its own tool
// call in the Gateway's context, and a finish brings the Gateway back on its own.
// This tool is for a person-facing status question, nothing more.
func (a *App) backgroundStatusTool() (tool.Schema, tool.Handler) {
	schema := backgroundStatusSchema()
	handler := func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(call.Args, &args)
		return a.backgroundStatus(ctx, call.WorkspaceID, call.SessionID, args.ID)
	}
	return schema, handler
}

func (a *App) backgroundStatus(ctx context.Context, workspaceID, sessionID, id int64) (tool.Result, error) {
	dels, err := a.Store.Agent().SessionDelegations(ctx, sessionID)
	if err != nil {
		return toolkit.Failed("the background agents could not be read")
	}
	names := a.AgentNames(ctx, workspaceID)
	now := time.Now().UTC()
	one := func(d *model.AgentDelegation) map[string]any {
		name := names[d.AgentKey]
		if name == "" {
			name = d.AgentKey
		}
		status := d.Status
		if d.Waiting() {
			status = model.SessionWaitingApproval
		}
		m := map[string]any{
			// Named internal_id so the model cannot mistake it for something to
			// say. It reads this list to narrate progress, and with `id` in it
			// the narration came out as "waiting on #257, #258 and #261".
			"internal_id":         d.ID,
			"name":                name,
			"status":              status,
			"running_for_seconds": int(now.Sub(d.CreatedAt).Seconds()),
		}
		if len(d.Progress) > 0 {
			m["progress"] = d.Progress
		}
		return m
	}
	// One agent, by its running id.
	if id != 0 {
		for _, d := range dels {
			if d.ID == id {
				return toolkit.Success(one(d))
			}
		}
		return toolkit.Success(map[string]any{"internal_id": id, "found": false, "message": "You have no agent with that id in this conversation."})
	}
	// The whole picture: counts by state, and the list.
	//
	// A BATCH is one entry, not one per agent. The Gateway started it with one
	// call and is brought back from it with one answer, so reading it as five
	// separate agents would have it narrating five things the person asked for
	// once, and offering five ids to stop where only the batch can be stopped.
	list := make([]map[string]any, 0, len(dels))
	counts := map[string]int{}
	batches := map[int64][]*model.AgentDelegation{}
	for _, d := range dels {
		if d.FleetID != nil {
			batches[*d.FleetID] = append(batches[*d.FleetID], d)
			continue
		}
		list = append(list, one(d))
		counts[d.Status]++
	}
	for _, entry := range a.batchStatus(ctx, batches, names, now) {
		list = append(list, entry)
		counts[entry["status"].(string)]++
	}
	return toolkit.Success(map[string]any{
		"started":    len(list),
		"running":    counts[model.DelegationRunning],
		"finished":   counts[model.DelegationDone],
		"failed":     counts[model.DelegationFailed],
		"cancelled":  counts[model.DelegationCancelled],
		"sub_agents": list,
	})
}

// batchStatus describes each fleet as the one thing it is: what is working, how
// many, and how many are back.
//
// The status comes from the fleet ROW, not from counting its members, and the
// difference is the one that matters to a Gateway deciding what to say: a batch
// whose members have all finished is not necessarily over, because whether it is
// over is a decision (CloseFleet) and this may be read in the moment between the
// last member landing and that decision being made.
func (a *App) batchStatus(ctx context.Context, batches map[int64][]*model.AgentDelegation, names map[string]string, now time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(batches))
	for id, members := range batches {
		fleet, err := a.Store.Agent().Fleet(ctx, id)
		if err != nil {
			continue
		}
		done := 0
		for _, m := range members {
			if m.Terminal() {
				done++
			}
		}
		out = append(out, map[string]any{
			// Named for what it is, so the model cannot pass it where an agent's
			// running id belongs: they are ids from different tables.
			"internal_fleet_id":   fleet.ID,
			"batch":               true,
			"name":                fleetChipName(members, names),
			"status":              fleetChipStatus(fleet),
			"agents":              len(members),
			"finished":            done,
			"running_for_seconds": int(now.Sub(fleet.CreatedAt).Seconds()),
		})
	}
	// Map iteration has no order, and the same question asked twice should not
	// come back describing the same work in a different order.
	sort.Slice(out, func(i, j int) bool {
		return out[i]["internal_fleet_id"].(int64) < out[j]["internal_fleet_id"].(int64)
	})
	return out
}

// cancelBackgroundSchema is what the model sees of it, apart from the handler
// for the same reason backgroundStatusSchema is (loopInternal).
func cancelBackgroundSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "The running id of ONE background agent to stop (returned when you started it, or shown by background_status).",
			},
			"fleet_id": map[string]any{
				"type": "integer",
				"description": "The fleet id of a BATCH to stop, all of its agents at once " +
					"(returned when you started them). Give this or `id`, never both.",
			},
		},
	})
	schema := tool.Schema{
		Name:              "cancel_background_task",
		FriendlyName:      "Stop a background agent",
		FriendlyNarration: "Stopping a background agent",
		Description: "Stop work you started in the background: one agent by its running id, or a whole batch by its fleet id. " +
			"It ends immediately with no result. Use it when the person asks to stop or cancel background work, then confirm in plain words. " +
			"Never mention this tool or its name to the person.",
		InputSchema: input,
		Kind:        tool.KindInternal,
		Risk:        tool.RiskReadOnly,
	}
	return schema
}

// cancelBackgroundTaskTool (operation b) lets the Gateway stop one background
// agent it started, named by its running id.
func (a *App) cancelBackgroundTaskTool() (tool.Schema, tool.Handler) {
	schema := cancelBackgroundSchema()
	handler := func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var args struct {
			ID      int64 `json:"id"`
			FleetID int64 `json:"fleet_id"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolkit.BadArguments("give the running id of the agent to stop, or the fleet id of the batch")
		}
		// One or the other. They are ids from different tables, so a number
		// meant as a fleet and read as an agent would stop somebody else's work.
		switch {
		case args.ID != 0 && args.FleetID != 0:
			return toolkit.BadArguments("give either an agent's running id or a fleet id, not both")
		case args.FleetID != 0:
			return a.cancelFleetTask(ctx, call.WorkspaceID, call.SessionID, args.FleetID)
		case args.ID != 0:
			return a.cancelBackgroundTask(ctx, call.SessionID, args.ID)
		default:
			return toolkit.BadArguments("give the running id of the agent to stop, or the fleet id of the batch")
		}
	}
	return schema, handler
}

func (a *App) cancelBackgroundTask(ctx context.Context, sessionID, id int64) (tool.Result, error) {
	del, err := a.Store.Agent().GetDelegation(ctx, id)
	if err != nil || del.SessionID != sessionID {
		// Not found, or another conversation's agent: never cancel across sessions.
		return toolkit.Success(map[string]any{"cancelled": false, "internal_id": id, "message": "You have no agent with that id in this conversation."})
	}
	if !a.CancelBackground(id) {
		return toolkit.Success(map[string]any{"cancelled": false, "internal_id": id, "message": "That agent is not running; it may have already finished."})
	}
	return toolkit.Success(map[string]any{"cancelled": true, "internal_id": id})
}

// cancelFleetTask stops a whole batch, scoped to this conversation exactly as
// cancelling one agent is: a fleet belonging to another conversation does not
// exist as far as this Gateway goes.
func (a *App) cancelFleetTask(ctx context.Context, workspaceID, sessionID, fleetID int64) (tool.Result, error) {
	fleet, err := a.Store.Agent().Fleet(ctx, fleetID)
	if err != nil || fleet.SessionID != sessionID {
		return toolkit.Success(map[string]any{"cancelled": false, "internal_fleet_id": fleetID,
			"message": "You have no batch of agents with that id in this conversation."})
	}
	if !a.CancelFleet(ctx, workspaceID, fleetID) {
		return toolkit.Success(map[string]any{"cancelled": false, "internal_fleet_id": fleetID,
			"message": "That batch is not running; it may have already finished."})
	}
	return toolkit.Success(map[string]any{"cancelled": true, "internal_fleet_id": fleetID})
}

// AgentNames maps every agent key in a workspace to its friendly name, for
// resolving a delegation's agent without a query per row.
func (a *App) AgentNames(ctx context.Context, workspaceID int64) map[string]string {
	agents, err := a.Store.Agents().List(ctx, workspaceID)
	if err != nil {
		return map[string]string{}
	}
	names := make(map[string]string, len(agents))
	for _, ag := range agents {
		names[ag.Key] = ag.Name
	}
	return names
}

// anyHandoffModeIsAuto reports whether at least one agent leaves the
// delegation mode to the Gateway, which is when the delegate tool offers a mode.
func anyHandoffModeIsAuto(subs []agentInfo) bool {
	for _, s := range subs {
		if s.DelegationMode == "" || s.DelegationMode == model.DelegationModeAuto {
			return true
		}
	}
	return false
}

// delegateToolSchema builds the one tool the Gateway calls to route a task. The
// agent keys are an enum so the model can only name one that exists.
func delegateToolSchema(subs []agentInfo) tool.Schema {
	keys := make([]string, len(subs))
	for i, s := range subs {
		keys[i] = s.Key
	}
	agentProp := map[string]any{
		"type":        "string",
		"enum":        keys,
		"description": "The key of the agent to route to. Each has its own role, tools, and model, listed in your instructions.",
	}
	properties := map[string]any{
		"agent": agentProp,
		"task": map[string]any{
			"type": "string",
			"description": "What the agent should do, stated FULLY and self-contained: it cannot see this conversation, " +
				"only this task, so include every detail, value, and piece of context it needs.",
		},
	}
	// The mode parameter is offered only when at least one agent leaves the
	// choice to the Gateway. If EVERY agent pins its mode, the Gateway has
	// nothing to decide: the parameter is dropped, and the runtime runs each
	// agent in its pinned mode regardless (KB/27).
	if anyHandoffModeIsAuto(subs) {
		properties["mode"] = map[string]any{
			"type": "string",
			"enum": []string{model.HandoffContinue, model.HandoffTerminal, model.HandoffBackground},
			"description": "What happens after the agent finishes. " +
				"\"continue\" (default): the agent reports its technical result back to YOU (not shown to the person) and you keep working, " +
				"read it, then act, answer the person yourself, or delegate again. Use this when the delegation is a SUB-STEP of a larger task. " +
				"\"terminal\": the agent's answer IS the final response to the person; you stop and add nothing. " +
				"Use this ONLY when the agent fully and self-containedly handles the person's entire request end to end. " +
				"\"background\": the agent runs on its own while you finish this turn; tell the person you have started it, and you will be " +
				"brought back with its result when it is done. Use this ONLY for genuinely heavy, long, or multi-step work the person should not " +
				"have to wait on; you can start several and keep chatting meanwhile. When the work is quick, or you need the result to continue THIS " +
				"turn, do NOT use background. " +
				"When unsure, use \"continue\".",
		}
	}
	input, _ := json.Marshal(map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"agent", "task"},
	})
	return tool.Schema{
		Name:         model.DelegateToolName,
		FriendlyName: "Delegate to an agent",
		Description: "Hand a focused task to an agent that runs independently with its own role, tools, and model. " +
			"Route to one when the task fits an agent better than answering yourself or calling a tool directly, " +
			"for example when the ability you need lives on an agent, not on you. The `mode` decides what comes back: " +
			"\"continue\" returns the agent's technical result to you to narrate; \"terminal\" lets the agent answer " +
			"the person directly. The agent never speaks to the person in \"continue\" mode, and never reports back to you in \"terminal\".",
		InputSchema: input,
		// Internal so the loadout keeps it without a per-tool grant: delegation
		// is a property of the Gateway, resolved here, not a tool to switch on.
		Kind: tool.KindInternal,
		Risk: tool.RiskReadOnly,
	}
}

// fleetToolSchema builds the tool that starts SEVERAL agents at once. Its whole
// argument is the list, so the model cannot half-fill it the way it could a
// delegate call that grew an optional array.
func fleetToolSchema(subs []agentInfo, maxAgents int) tool.Schema {
	if maxAgents <= 0 {
		maxAgents = model.DefaultMaxFleetAgents
	}
	// Only agents that can ACTUALLY be in a fleet. The same check fleet.go makes
	// when the call arrives, so the enum and the refusal cannot disagree: an
	// agent that would be turned away is never offered.
	//
	// It listed every agent, and that is the defect this exists for. An agent
	// pinned inline appeared here as a legal choice while the prompt's line for
	// inline forbade only the BACKGROUND and said nothing about fleets, so the
	// model asked for the one thing nobody had told it not to and was refused by
	// a rule stated in neither place it reads. The refusal then outlived the
	// fact: the pin was changed hours later and the model went on believing it,
	// because a tool result is permanent and configuration is not.
	keys := make([]string, 0, len(subs))
	for _, s := range subs {
		if agent.Detached(agent.ResolveHandoff(model.HandoffFleet, s.DelegationMode)) {
			keys = append(keys, s.Key)
		}
	}
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"description": "One entry per agent you want working, each with its own task. " +
					"The same agent may appear more than once with different tasks.",
				"minItems": 1,
				"maxItems": maxAgents,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"agent": map[string]any{
							"type":        "string",
							"enum":        keys,
							"description": "The key of the agent to run.",
						},
						"task": map[string]any{
							"type": "string",
							"description": "What this agent should do, stated FULLY and self-contained: it cannot see " +
								"this conversation or the other tasks, only this one, so include every detail it needs.",
						},
					},
					"required": []string{"agent", "task"},
				},
			},
		},
		"required": []string{"tasks"},
	})
	return tool.Schema{
		Name:         model.FleetToolName,
		FriendlyName: "Run agents in parallel",
		Description: "Start several agents at once, each on its own task, and be brought back with ALL of their " +
			"results once every one has finished. Use this when a request breaks into parts that do not depend on " +
			"each other, so they can be worked on at the same time instead of one after another. " +
			"You do not wait: tell the person you have started them and finish this turn; you are brought back " +
			"automatically with every result when the last one is done. " +
			"When the parts DO depend on each other, or you need a result to decide the next step, delegate one at a time instead.",
		InputSchema: input,
		// Internal for the same reason delegation is: it is a property of the
		// Gateway, resolved here, not a capability to switch on.
		Kind: tool.KindInternal,
		Risk: tool.RiskReadOnly,
	}
}

// memoryToolSchema is the one tool a Gateway calls to flag that a turn is worth
// remembering. It is internal (no grant, invisible to the admin UI) because it
// is a property of the Gateway, like delegation, not a capability to switch on.
// Calling it does no work in the turn: the loop records the flag and the note is
// distilled in the background, so the model can flag freely without stalling an
// answer.
func memoryToolSchema() tool.Schema {
	input, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{
				"type": "string",
				"enum": []string{"person", "workspace"},
				"description": "What to remember about: \"person\" for who you are helping and how they like their answers, " +
					"\"workspace\" for your own working notes on how to do the work well here. Omit to reconsider both.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Optional: a short hint of what is worth remembering. You do not need to write the full note; it is composed for you from the conversation.",
			},
		},
	})
	return tool.Schema{
		Name:         model.MemoryToolName,
		FriendlyName: "Remember this",
		Description: "Flag that this conversation taught you something lasting about the person or " +
			"about how the work is done here. It is written up in the background and does not " +
			"interrupt your answer. Read what is already known with recall.",
		InputSchema: input,
		// Internal so it rides along without a per-tool grant and never shows in
		// the admin tool table: it is infrastructure, resolved here.
		Kind: tool.KindInternal,
		Risk: tool.RiskReadOnly,
	}
}

// loopInternal names the tools the turn loop adds to a loadout itself.
//
// They are internal like tool_guide, but unlike it they are never REGISTERED:
// each is built here, per turn, from what this conversation resolved to (the
// roster it may delegate to, the fleet size, whether there is a queue at all).
// So the registry cannot answer for them, and anything asking "is this one of
// ours" has to ask here.
//
// Built by calling the same constructors the loadout is built from, so a sixth
// one cannot be added without appearing in this list: it would have to be a
// constructor, and a constructor left out of this list is what the test
// TestEveryToolTheLoopAddsIsKnownAsOurs fails on.
var loopInternal = sync.OnceValue(func() map[string]bool {
	names := make(map[string]bool)
	for _, schema := range []tool.Schema{
		memoryToolSchema(),
		delegateToolSchema(nil),
		fleetToolSchema(nil, 0),
		backgroundStatusSchema(),
		cancelBackgroundSchema(),
	} {
		names[schema.Name] = true
	}
	return names
})

// InternalTool reports whether a tool is a piece of our own wiring rather than
// work somebody asked for: tool_guide, delegation, remember, the background
// controls.
//
// Two places to look, because internal tools arrive two ways. Most are
// registered in code and carry their kind on the schema. The rest are the
// loop's own, built per turn and never registered (loopInternal).
//
// A name in neither is a tool an administrator created or a service projected.
// Those are NOT ours: they do real work, and everything about them is somebody's
// to look at.
func (a *App) InternalTool(name string) bool {
	if schema, known := a.Tools.Lookup(name); known {
		return schema.Kind == tool.KindInternal
	}
	return loopInternal()[name]
}
