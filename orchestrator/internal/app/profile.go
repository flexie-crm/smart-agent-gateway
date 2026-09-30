package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"flexie.io/sag/internal/cmdpolicy"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools"
	"flexie.io/sag/internal/tools/agentguide"
	"flexie.io/sag/internal/tools/integrations"
	"flexie.io/sag/internal/tools/machine"
	"flexie.io/sag/internal/workflow"
)

// ProfileRequest is one turn asking what it is allowed to be.
type ProfileRequest struct {
	WorkspaceID int64
	UserID      int64
	// DeviceID is which of the person's computers this turn came from, when it
	// came from one. It decides whether a tool that runs on their own machine
	// is in the turn at all.
	DeviceID string
	// WorkingFolder is the folder on that computer the person has given the
	// assistant to work in, when they have given one. It is told to the model,
	// because nothing else can tell it: the choice is made in the application
	// and used on that machine, and an assistant that has not been told answers
	// "I cannot see your project" while holding the tools to read every file in
	// it.
	WorkingFolder string

	// Machine is what kind of computer that is, as the application described
	// itself. Nil from a browser and from any run with no computer to act on.
	Machine *MachineEnv
	// Channel is where the request came from: the chat UI, an HTTP endpoint,
	// an MCP client. It is a dimension a workflow can condition on, which is
	// how one workspace can give its people a rich assistant in the chat and a
	// narrow, tool-less one over MCP.
	Channel string
	// PreferredModelID is the caller's choice, from the model picker. It is
	// honoured only when no configured layer pinned a model: an administrator
	// who pins one is giving an instruction, not a default.
	PreferredModelID int64
	// SessionID is the conversation this turn belongs to. It identifies the
	// Gateway as an agent, so a tool that keeps anything between calls files it
	// under this Gateway rather than under something its background agents
	// share (tool.Owner). Zero on the first turn of a new conversation, which
	// has no earlier state to find.
	SessionID int64
}

// Resolve answers the only question a surface needs to ask before running a
// turn: what is this, and what may it do. Every channel (the chat UI, an HTTP
// endpoint, an MCP client) goes through here, so they cannot drift into
// disagreeing about who gets which assistant.
func (a *App) Resolve(ctx context.Context, req ProfileRequest) (*model.Profile, tool.Loadout, error) {
	profile, err := a.ResolveProfile(ctx, req)
	if err != nil {
		return nil, tool.Loadout{}, err
	}
	loadout, err := a.ResolveTools(ctx, req, profile)
	if err != nil {
		return nil, tool.Loadout{}, err
	}
	return profile, loadout, nil
}

// ResolveTools is the second half of Resolve: the Gateway's tools and, once they
// are known, its system prompt. It is separate for the one caller that cannot
// use Resolve whole, the chat stream, which must create the conversation before
// the tools are bound, because a tool that keeps state between calls files it
// under the agent using it and the Gateway's identity IS its conversation
// (req.SessionID, tool.Owner).
func (a *App) ResolveTools(ctx context.Context, req ProfileRequest, profile *model.Profile) (tool.Loadout, error) {
	loadout, err := a.Loadout(
		ctx, req.WorkspaceID, req.UserID, req.DeviceID,
		profile.Tools, profile.ConfirmTools, profile.Brains,
		tool.OwnerOfSession(req.SessionID),
		// What this turn's writes are attributed to. A knowledge base written
		// during a conversation was written by the agent, on the person's
		// behalf: which person it was for is in the transcript, and putting
		// their name on the row instead would credit them for text they never
		// wrote.
		gatewayActing(profile),
	)
	if err != nil {
		return tool.Loadout{}, err
	}
	// The Gateway can flag a turn as worth remembering. It is infrastructure, so
	// it rides along rather than being granted, but only when the assistant has
	// at least one real tool: a deliberately tool-less assistant (a locked-down
	// channel) stays exactly that, and its memory still updates on the cadence.
	if len(loadout.Schemas) > 0 {
		loadout.Schemas = append(loadout.Schemas, memoryToolSchema())
	}

	// The Gateway gets its roster: the delegate tool that routes to an agent,
	// and the agents themselves for the prompt. A workspace with no
	// agents gets neither, and is exactly as simple as before.
	subs, err := a.agentRoster(ctx, req.WorkspaceID, req.UserID, req.DeviceID)
	if err != nil {
		return tool.Loadout{}, err
	}
	if len(subs) > 0 {
		loadout.Schemas = append(loadout.Schemas, delegateToolSchema(subs))
		// Starting many agents at once needs somewhere to run them, and that is
		// the workers. With no broker connected there is nothing to dispatch to,
		// so the ability is not offered rather than offered and refused: a tool
		// the model can call and never succeed with is a turn spent learning
		// what this deployment cannot do.
		if a.Queue != nil {
			loadout.Schemas = append(loadout.Schemas, fleetToolSchema(subs, profile.MaxFleetAgents))
		}
		// The Gateway can also watch and stop its own background delegations, so
		// those tools ride along with the roster (Mode C, KB/27).
		for _, t := range a.backgroundTools() {
			loadout.Schemas = append(loadout.Schemas, t.schema)
			loadout.Handlers[t.schema.Name] = t.handler
		}
		// And it can look one up. The prompt names its agents; this is how it
		// finds out what one can actually reach before sending work there.
		// Registered with the roster and not always, because a workspace with no
		// agents would otherwise carry a tool whose only possible answer is
		// "none".
		guide := agentguide.New(func() []agentguide.Agent { return agentGuideRoster(subs) })
		loadout.Schemas = append(loadout.Schemas, guide.Schema)
		loadout.Handlers[guide.Schema.Name] = guide.Handle
		// tool_guide was bound to this turn's own tools inside Loadout, before
		// the agents were known. Rebind it over theirs as well, so a key that
		// came out of agent_guide resolves instead of reading as a tool that
		// does not exist; each one is labelled with the agent that holds it.
		tools.BindToolGuide(loadout, agentToolGuides(subs)...)
	}

	// The skills it can reach: the ones it was assigned, plus the ones its own
	// tools document. Present only where there are skills, for the same reason
	// agent_guide is present only where there are agents.
	tools.AppendSkills(&loadout, a.Store.Skills(), a.Machines, tools.Computer{
		WorkspaceID: req.WorkspaceID, UserID: req.UserID, DeviceID: req.DeviceID,
	}, reachableSkills(profile.Skills, loadout.Skills))

	// The Gateway's own long-term memory, when it has one: an internal tool, no
	// grant and no approval, present only because a memory brain is assigned.
	tools.AppendMemory(&loadout, a.Store, profile.MemoryBrainID, gatewayActing(profile))

	// The prompt is assembled last, once everything it describes is known: the
	// tools actually loaded, the agents on hand, the person, the memory.
	prompt, err := a.buildGatewayPrompt(ctx, req, profile, loadout, subs)
	if err != nil {
		return tool.Loadout{}, err
	}
	profile.SystemPrompt = prompt
	return loadout, nil
}

// buildGatewayPrompt gathers the live context the assembler needs and renders
// the Gateway's system prompt. The workspace and person are read fresh, not
// taken from a token, for the same reason permissions are: what is true now is
// what the assistant should be told.
func (a *App) buildGatewayPrompt(ctx context.Context, req ProfileRequest, profile *model.Profile, loadout tool.Loadout, subs []agentInfo) (string, error) {
	ws, err := a.Store.Workspaces().GetByID(ctx, req.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("load workspace for prompt: %w", err)
	}
	user, err := a.Store.Users().GetByID(ctx, req.UserID)
	if err != nil {
		return "", fmt.Errorf("load person for prompt: %w", err)
	}
	userMem, err := a.Store.Memory().UserMemory(ctx, req.WorkspaceID, req.UserID)
	if err != nil {
		return "", fmt.Errorf("load person memory for prompt: %w", err)
	}
	wsMem, err := a.Store.Memory().WorkspaceMemory(ctx, req.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("load workspace memory for prompt: %w", err)
	}

	brains, err := a.brainRosterFor(ctx, req.WorkspaceID, profile.Brains, profile.MemoryBrainID)
	if err != nil {
		return "", err
	}
	held, err := a.skillRosterFor(ctx, req.WorkspaceID, profile.Skills)
	if err != nil {
		return "", err
	}

	return renderGateway(gatewayPrompt{
		now:             time.Now(),
		zone:            a.PersonZone(ctx, req.UserID),
		services:        a.connectedIntegrations(ctx, req.WorkspaceID, loadout.OnDemand),
		workspaceName:   ws.Name,
		workspaceAbout:  ws.Description,
		personName:      user.Name,
		userMemory:      userMem,
		workspaceMemory: wsMem,
		capabilities:    loadout.Schemas,
		agents:          subs,
		brains:          brains,
		skills:          held,
		folder:          req.WorkingFolder,
		machine:         req.Machine,
		instructions:    profile.Instructions,
	}), nil
}

// agentGuideRoster is the roster as agent_guide answers it: each agent with the
// abilities it can actually reach, grouped the way an administrator sees them
// when they choose an agent's tools, plus the knowledge it holds.
func agentGuideRoster(subs []agentInfo) []agentguide.Agent {
	out := make([]agentguide.Agent, 0, len(subs))
	for _, sub := range subs {
		abilities := make([]agentguide.Ability, 0, len(sub.Abilities))
		for _, schema := range sub.Abilities {
			name := schema.FriendlyName
			if name == "" {
				name = schema.Name
			}
			abilities = append(abilities, agentguide.Ability{
				// The callable key as well as the friendly name: it is what
				// tool_guide takes, and the answer carrying it says in the same
				// breath that it is not the Gateway's to call.
				Key:         schema.Name,
				Name:        name,
				Description: schema.Description,
				Group:       abilityGroup(schema),
			})
		}
		knowledge := make([]agentguide.Knowledge, 0, len(sub.Brains.knowledge))
		for _, kb := range sub.Brains.knowledge {
			knowledge = append(knowledge, agentguide.Knowledge{Name: kb.name, ReadOnly: kb.readOnly})
		}
		out = append(out, agentguide.Agent{
			Key:          sub.Key,
			Name:         sub.Name,
			Instructions: sub.Instructions,
			Abilities:    abilities,
			Knowledge:    knowledge,
			Memory:       sub.Brains.memory,
		})
	}
	return out
}

// abilityGroup is the heading one ability sits under. The same three answers the
// admin console gives on the agent's own form (api.toolSource), because a
// catalogue that disagrees with itself depending on who asked is worse than
// either answer.
func abilityGroup(schema tool.Schema) string {
	switch schema.Kind {
	case tool.KindMCP:
		if service := serviceOfSchema(schema); service != "" {
			return service
		}
		return "Connected service"
	case tool.KindCustom:
		return "Custom"
	default:
		return "Built-in"
	}
}

// agentToolGuides is every tool this turn's agents hold, each tagged with the
// agent that holds it, so tool_guide can answer for one without ever letting it
// read as the Gateway's own.
func agentToolGuides(subs []agentInfo) []tools.AgentTool {
	var held []tools.AgentTool
	for _, sub := range subs {
		for _, schema := range sub.Abilities {
			held = append(held, tools.AgentTool{Schema: schema, Agent: sub.Key})
		}
	}
	return held
}

// skillRosterFor resolves an agent's skill assignments into the map its prompt
// shows: the handle it addresses each by, and the name a person gave it.
//
// Only what RESOLVES is in it, which is the rule brainRosterFor follows and for
// the same reason: a skill since deleted, or one an administrator switched off,
// would otherwise produce a line naming something the agent cannot open. A
// disabled skill is left out rather than marked, because the prompt is what the
// agent may do and a skill it may not use is not a decision to put in front of
// it every turn.
//
// The order is the store's (by handle), so the same assignment reads the same
// way every turn: a prompt that reshuffles itself between turns is a prompt the
// model cannot cache.
// reachableSkills is what this turn may open: what the agent was ASSIGNED, plus
// what its TOOLS document.
//
// The two are different in kind and both belong here. An assigned skill is in
// the prompt's map, because it could bear on anything. A tool's skill is not
// mentioned until the tool is, and it is reachable for a plainer reason: the
// guide names it, so a guide whose named procedure could not be opened would be
// a signpost to a locked door. Granting the tool granted the instructions.
//
// Deduplicated, because a skill can honestly be both, and the assignment is
// kept first so the prompt's examples read in the order the person set.
func reachableSkills(assigned, fromTools []int64) []int64 {
	if len(fromTools) == 0 {
		return assigned
	}
	seen := make(map[int64]bool, len(assigned)+len(fromTools))
	all := make([]int64, 0, len(assigned)+len(fromTools))
	for _, id := range append(append([]int64{}, assigned...), fromTools...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		all = append(all, id)
	}
	return all
}

// toolSkillIndex resolves every skill named by a custom tool in this turn, in
// ONE query for the whole loadout.
//
// Nothing is read when no tool names one, which is the ordinary case: a
// workspace that has never attached documentation to a tool pays nothing for
// this. The same liveness rule as an agent's roster (active, with a version
// that is actually live), because a guide naming a skill that cannot be opened
// is worse than a guide naming none.
func (a *App) toolSkillIndex(ctx context.Context, workspaceID int64, rows map[string]*model.Tool) (map[int64]skillOnHand, error) {
	wanted := map[int64]bool{}
	for _, row := range rows {
		if row == nil || row.Kind != string(tool.KindCustom) {
			continue
		}
		for _, id := range row.Skills {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	all, err := a.Store.Skills().Skills(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load the skills a tool documents: %w", err)
	}
	index := make(map[int64]skillOnHand, len(wanted))
	for _, s := range all {
		if !wanted[s.ID] || s.Status != model.SkillActive || s.ActiveVersionID == 0 {
			continue
		}
		index[s.ID] = skillOnHand{id: s.ID, handle: s.Name, name: s.Label()}
	}
	return index, nil
}

// heldSkills picks one tool's skills out of the index, keeping the order the
// tool named them in and dropping the ones that did not resolve.
func heldSkills(index map[int64]skillOnHand, ids []int64) []skillOnHand {
	if len(index) == 0 || len(ids) == 0 {
		return nil
	}
	held := make([]skillOnHand, 0, len(ids))
	for _, id := range ids {
		if s, ok := index[id]; ok {
			held = append(held, s)
		}
	}
	return held
}

func (a *App) skillRosterFor(ctx context.Context, workspaceID int64, skillIDs []int64) ([]skillOnHand, error) {
	if len(skillIDs) == 0 {
		return nil, nil
	}
	all, err := a.Store.Skills().Skills(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load skills for prompt: %w", err)
	}
	held := make(map[int64]bool, len(skillIDs))
	for _, id := range skillIDs {
		held[id] = true
	}
	roster := make([]skillOnHand, 0, len(skillIDs))
	for _, s := range all {
		if !held[s.ID] || s.Status != model.SkillActive || s.ActiveVersionID == 0 {
			continue
		}
		roster = append(roster, skillOnHand{id: s.ID, handle: s.Name, name: s.Label()})
	}
	return roster, nil
}

// brainRosterFor resolves an agent's brain assignments into the map its prompt
// shows: the knowledge bases it can consult (name, read-only, categories) and the
// name of the brain it manages as memory. A brain since deleted drops out, so the
// prompt never names one that is gone.
func (a *App) brainRosterFor(ctx context.Context, workspaceID int64, brainIDs []int64, memoryBrainID *int64) (brainRoster, error) {
	var roster brainRoster
	for _, id := range brainIDs {
		// The memory brain is not listed as knowledge as well.
		//
		// It is often assigned as both, and then the prompt named it twice: once
		// in the roster of bases to consult and again, at length, as the
		// assistant's own memory. The second says everything the first does and
		// more, so the first is the redundancy.
		if memoryBrainID != nil && *memoryBrainID == id {
			continue
		}
		b, err := a.Store.Brains().Brain(ctx, workspaceID, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return brainRoster{}, fmt.Errorf("load knowledge brain: %w", err)
		}
		cats, err := a.Store.Brains().Categories(ctx, workspaceID, id)
		if err != nil {
			return brainRoster{}, fmt.Errorf("load brain categories: %w", err)
		}
		names := make([]string, 0, len(cats))
		for _, c := range cats {
			names = append(names, c.Name)
		}
		roster.knowledge = append(roster.knowledge, knowledgeBrain{
			name: b.Name, readOnly: b.Locked, categories: names,
		})
	}
	if memoryBrainID != nil {
		b, err := a.Store.Brains().Brain(ctx, workspaceID, *memoryBrainID)
		if err == nil {
			roster.memory = b.Name
		} else if !errors.Is(err, store.ErrNotFound) {
			return brainRoster{}, fmt.Errorf("load memory brain: %w", err)
		}
	}
	return roster, nil
}

// ResolveProfile applies the layered configuration model and returns what this
// turn actually runs as.
//
// The layers, in order, each overriding only the fields it has an opinion
// about:
//
//  1. the code defaults, so a workspace with a vendor and a model can chat
//     without configuring anything at all;
//  2. the workspace default agent, the "default package": one agent, keyed
//     'default', that an administrator uses to set the house prompt and the
//     house tool set;
//  3. the workflow that matches this user and this channel, which overrides
//     the package for the requests it was assigned to.
//
// Exactly one workflow applies. Overrides do not stack, because a configuration
// nobody can predict is not a configuration, it is a surprise (KB/15).
//
// It is resolved per turn, never cached in a token: a user moved out of a group
// loses that group's workflow on their very next message.
func (a *App) ResolveProfile(ctx context.Context, req ProfileRequest) (*model.Profile, error) {
	profile := &model.Profile{
		ModelID: req.PreferredModelID,
		// SystemPrompt is assembled last, in Resolve, from the structured base
		// and the live context. The layers below set Instructions, the house
		// text that gets appended to that base, never the base itself.
		Reasoning: DefaultReasoning,
		Tools:     a.DefaultTools(),
		// The deployment's opinion on how long a person has to answer a
		// confirmation. An agent or a workflow may still overrule it.
		ApprovalTTL: a.Config.ApprovalTTL,
	}

	if err := a.applyDefaultAgent(ctx, req.WorkspaceID, profile); err != nil {
		return nil, err
	}
	if err := a.applyWorkflow(ctx, req, profile); err != nil {
		return nil, err
	}
	return profile, nil
}

// applyDefaultAgent lays the workspace's own package over the code defaults.
// Its absence is the normal state of a new workspace, not an error.
func (a *App) applyDefaultAgent(ctx context.Context, workspaceID int64, profile *model.Profile) error {
	agent, err := a.Store.Agents().GetByKey(ctx, workspaceID, model.DefaultAgentKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve default agent: %w", err)
	}
	if agent.Status != model.StatusActive {
		return nil
	}

	profile.AgentID = &agent.ID
	profile.AgentName = agent.Name
	// The house prompt is appended to the base, not a replacement for it: an
	// administrator shapes the assistant, they do not get to drop the identity
	// and the communication rules underneath.
	profile.Instructions = agent.Instructions
	profile.Reasoning = agent.Reasoning
	profile.Settings = agent.Settings
	if agent.ApprovalTTL != nil {
		profile.ApprovalTTL = *agent.ApprovalTTL
	}
	if agent.MaxIterations != nil {
		profile.MaxIterations = *agent.MaxIterations
	}
	if agent.MaxFleetAgents != nil {
		profile.MaxFleetAgents = *agent.MaxFleetAgents
	}
	if agent.ModelID != nil {
		profile.ModelID, profile.ModelPinned = *agent.ModelID, true
	}
	if agent.Tools != nil {
		profile.Tools = agent.Tools
	}
	if agent.ConfirmTools != nil {
		profile.ConfirmTools = agent.ConfirmTools
	}
	// The brains this agent may read, and the one it manages as its own memory.
	// They ride onto the profile so the loadout can scope the brain tools to
	// exactly this set when it binds their handlers (KB/26).
	profile.Brains = agent.Brains
	profile.MemoryBrainID = agent.MemoryBrainID
	// And the skills it may use, which ride along for the same reason: the skill
	// tools are scoped to exactly this set when their handlers are bound.
	profile.Skills = agent.Skills
	return nil
}

// applyWorkflow finds the workflow assigned to this person on this channel and
// lays its overrides on top.
func (a *App) applyWorkflow(ctx context.Context, req ProfileRequest, profile *model.Profile) error {
	candidates, err := a.Store.Workflows().Candidates(ctx, req.WorkspaceID)
	if err != nil {
		return fmt.Errorf("load workflows: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	subject, err := a.subject(ctx, req)
	if err != nil {
		return err
	}
	winner, ok := workflow.Select(candidates, subject)
	if !ok {
		return nil
	}

	var definition model.Definition
	if err := json.Unmarshal(winner.Definition, &definition); err != nil {
		// A definition that cannot be read is a configuration error, and the
		// honest response is to refuse the turn. Falling back to the defaults
		// would quietly hand the user a more powerful assistant than the
		// administrator configured, which is the one failure this layer exists
		// to prevent.
		return fmt.Errorf("workflow %d: unreadable definition: %w", winner.WorkflowID, err)
	}
	if definition.Kind != model.DefinitionProfile {
		return fmt.Errorf("workflow %d: unsupported definition kind %q", winner.WorkflowID, definition.Kind)
	}

	profile.WorkflowVersionID = &winner.VersionID
	if definition.Profile == nil {
		return nil
	}
	override := definition.Profile
	// A workflow's prompt is this channel's house instructions: it replaces the
	// default agent's instructions (the layer below), but like them it is
	// appended to the assembled base, never a replacement for it.
	if override.SystemPrompt != nil {
		profile.Instructions = *override.SystemPrompt
	}
	if override.Reasoning != nil {
		profile.Reasoning = *override.Reasoning
	}
	if override.Tools != nil {
		profile.Tools = *override.Tools
	}
	if override.ApprovalTTLSeconds != nil {
		profile.ApprovalTTL = time.Duration(*override.ApprovalTTLSeconds) * time.Second
	}
	if override.ModelID != nil {
		profile.ModelID, profile.ModelPinned = *override.ModelID, true
	}
	return nil
}

// subject is who is asking, resolved live from the database rather than read
// from the token: permissions and memberships must take effect immediately.
func (a *App) subject(ctx context.Context, req ProfileRequest) (workflow.Subject, error) {
	subject := workflow.Subject{UserID: req.UserID, Channel: req.Channel}

	groups, err := a.Store.Groups().ListForUser(ctx, req.UserID)
	if err != nil {
		return workflow.Subject{}, fmt.Errorf("resolve groups: %w", err)
	}
	for _, g := range groups {
		subject.GroupIDs = append(subject.GroupIDs, g.ID)

		roles, err := a.Store.Groups().ListRoles(ctx, g.ID)
		if err != nil {
			return workflow.Subject{}, fmt.Errorf("resolve roles: %w", err)
		}
		for _, r := range roles {
			subject.RoleIDs = append(subject.RoleIDs, r.ID)
		}
	}
	return subject, nil
}

// Loadout turns the profile's tool names into the tools this turn can actually
// run: the code that backs them, the workspace's decisions about them, and this
// person's right to reach them.
//
// Three things have to agree before a tool reaches the model:
//
//   - the registry has a handler for it, because a name in configuration is
//     not an implementation;
//   - the workspace has it active, because an administrator switched it on;
//   - the user is granted it, because authorization is a grant and not a
//     property of the tool.
//
// Approval is the exception to "the admin decides": it is resolved as (code OR
// row), so an administrator can add friction to a tool that has none, and can
// never take it off one the code says is dangerous.
// Loadout resolves the schemas and handlers for a turn: the tools the code
// ships, filtered to what the workspace has active and this user is granted,
// plus the projected MCP and custom tools. confirm names the tools this
// assistant must stop and confirm; a tool listed there loads requiring
// approval, on top of the code floor and any MCP drift lock.
// gatewayActing is who the Gateway's own writes are recorded as: the name the
// workspace gave it, or the product's own word when it has no row to be named
// by. Never blank, because a write with no author is a write nobody can account
// for.
func gatewayActing(profile *model.Profile) model.Actor {
	if profile.AgentName != "" {
		return model.Actor{Name: profile.AgentName}
	}
	return model.Actor{Name: model.DefaultAgentName}
}

func (a *App) Loadout(
	ctx context.Context,
	workspaceID, userID int64,
	deviceID string,
	names, confirm []string,
	brains []int64,
	owner tool.Owner,
	by model.Actor,
) (tool.Loadout, error) {
	loadout := a.Tools.Load(names)
	if len(names) == 0 && len(loadout.Schemas) == 0 {
		return loadout, nil
	}

	// What the person's own computer can actually do this turn.
	//
	// A gateway is often newer than somebody's application, so a tool this
	// build ships may be one their installation has never heard of. Left in,
	// the model would be told about an ability that fails the moment it is
	// used; left out, it simply is not there. Nothing fails mid-conversation
	// and nobody is asked to update before using what does work.
	offers := machine.Offers(a.Machines, workspaceID, userID, deviceID)

	allowed, err := a.Store.Tools().ListForUser(ctx, workspaceID, userID)
	if err != nil {
		return tool.Loadout{}, fmt.Errorf("resolve tool grants: %w", err)
	}
	byName := make(map[string]*model.Tool, len(allowed))
	for _, t := range allowed {
		byName[t.Name] = t
	}
	confirmSet := make(map[string]bool, len(confirm))
	for _, name := range confirm {
		confirmSet[name] = true
	}

	kept := make([]tool.Schema, 0, len(loadout.Schemas))
	// Runnable, and deliberately not offered: what a connected service projects
	// (below). Kept apart from the moment it is built, so nothing downstream has
	// to remember which list a schema came from.
	var onDemand []tool.Schema
	for _, schema := range loadout.Schemas {
		if schema.Kind == tool.KindInternal {
			// Infrastructure, not capability. It is loaded by the registry, is
			// invisible to the admin UI, and has nothing to grant.
			kept = append(kept, schema)
			continue
		}
		// A tool that runs on the person's own computer is offered only where
		// there is a computer that can run it. An administrator's grant still
		// decides who MAY, below; this decides what is there to grant at all.
		if machine.Runs(schema.Name) && !offers[schema.Name] {
			continue
		}
		row, ok := byName[schema.Name]
		if !ok {
			// Disabled in this workspace, or not granted to this user. Either
			// way the model never learns the tool exists, which is the only
			// refusal it cannot argue with.
			delete(loadout.Handlers, schema.Name)
			continue
		}
		schema.RequiresApproval = schema.RequiresApproval || row.RequiresApproval || confirmSet[schema.Name]
		kept = append(kept, schema)
	}

	// The registry answers for the tools the code ships. A projected MCP tool
	// has no compile-time handler: its schema is the row the sync wrote, and
	// its handler is a proxy to the connection that offers it. The same three
	// agreements hold: the row exists (the sync wrote it and the remote still
	// offers it), the workspace has it active, the user is granted it, all of
	// which ListForUser already said.
	var serverNames map[int64]string
	for _, name := range names {
		if _, taken := loadout.Handlers[name]; taken {
			continue
		}
		row, ok := byName[name]
		if !ok || row.Kind != string(tool.KindMCP) || row.MCPServerID == nil {
			continue
		}
		if serverNames == nil {
			if serverNames, err = a.mcpServerNames(ctx, workspaceID); err != nil {
				return tool.Loadout{}, err
			}
		}
		service := serverNames[*row.MCPServerID]
		// ON DEMAND, not offered. A connected service can project dozens of
		// tools, and every one of them costs tokens on every turn merely by
		// being described, whether or not that service is ever touched. The
		// model discovers these through the integrations ability and calls one
		// through it; everything else about them is unchanged.
		onDemand = append(onDemand, tool.Schema{
			Name:             row.Name,
			FriendlyName:     shownAs(service, row.FriendlyName),
			Description:      row.Description,
			InputSchema:      row.InputSchema,
			Kind:             tool.KindMCP,
			Service:          service,
			RemoteName:       row.RemoteName,
			Risk:             tool.RiskLevel(row.Risk),
			RequiresApproval: row.RequiresApproval || confirmSet[row.Name],
			DefinitionHash:   row.DefinitionHash,
			// The card is written HERE, not taken from the remote: its
			// description is untrusted text, and a person deciding must at
			// least be told whose action this is.
			ApprovalTitle: fmt.Sprintf("%s, on %s", row.FriendlyName, service),
			ApprovalPrompt: fmt.Sprintf(
				"This happens on %s, which is connected to your workspace but is not part of it. Read what would be sent there before you allow it.",
				service),
		})
		loadout.Handlers[name] = a.mcpToolHandler(*row.MCPServerID, row.RemoteName)
	}

	// A custom tool has no compile-time handler either: it is a data row backed
	// by a template. Its schema is the row (self-describing, like an MCP tool's),
	// its handler is the template bound to its stored config, and its deep guide
	// comes from the template's code. The same three agreements held before it
	// reached here (the row exists, is active, is granted).
	// The documentation the custom tools in this turn point at, resolved ONCE:
	// a workspace has one list of skills, and asking per tool would be a query
	// per tool on every turn.
	docs, err := a.toolSkillIndex(ctx, workspaceID, byName)
	if err != nil {
		return tool.Loadout{}, err
	}
	for _, name := range names {
		if _, taken := loadout.Handlers[name]; taken {
			continue
		}
		row, ok := byName[name]
		if !ok || row.Kind != string(tool.KindCustom) || row.Template == "" {
			continue
		}
		held := heldSkills(docs, row.Skills)
		schema, handler, bound := a.bindCustom(row, owner, held)
		if !bound {
			continue
		}
		// What this turn may open BECAUSE it holds this tool. Only the ones
		// that resolved: a skill that was switched off is not named in the
		// guide either, so the two cannot disagree.
		for _, sk := range held {
			loadout.Skills = append(loadout.Skills, sk.id)
		}
		schema.RequiresApproval = schema.RequiresApproval || row.RequiresApproval || confirmSet[schema.Name]
		kept = append(kept, schema)
		loadout.Handlers[name] = handler
	}

	loadout.Schemas = kept
	loadout.OnDemand = onDemand
	// Scope the brain tools to this agent's own brains. They loaded with an empty
	// allow-list; this is where their handlers are bound to what the agent may
	// reach, for the Gateway and every agent alike.
	tools.BindBrains(loadout, a.Store, brains, by)
	// Point tool_guide at THIS turn's abilities. Bound to the code's registry it
	// cannot see a tool an administrator created or one projected from a remote
	// server: their guides and topics exist and nothing could reach them.
	tools.BindToolGuide(loadout)
	// And the connected services at what this turn holds on demand, named by
	// the service each tool came from.
	byService := make(map[string]int64, len(serverNames))
	for id, name := range serverNames {
		byService[name] = id
	}
	tools.BindIntegrations(loadout, connectedServices(byService, onDemand))
	// Point the terminal at what this workspace decided it may run. Its handler
	// loaded with an empty policy, because a policy is read at boot and edited
	// at four in the afternoon: bound once, the old rules would go on being
	// enforced until the process restarted.
	if row, ok := byName[machine.TerminalName]; ok {
		schema, _ := a.Tools.Lookup(machine.TerminalName)
		policy, failed := terminalPolicy(schema, row.Config)
		if failed != nil {
			// Not a turn that fails: a terminal that refuses, saying the one
			// thing that is true and actionable. Logged as well, because it is
			// a row nobody can have written through the console.
			a.Log.Error().Err(failed).Int64("workspace_id", workspaceID).
				Msg("the terminal's settings could not be read")
			machine.RefuseTerminal(loadout, "this tool's settings could not be read, so nothing was run. "+
				"An administrator should open its settings and save them again. Report that rather than "+
				"trying another command.")
		} else {
			machine.BindTerminal(loadout, a.Machines, policy, owner)
		}
	}
	return loadout, nil
}

// connectedIntegrations is what the prompt lists: every connected service this
// turn can reach, by each handle it has (its id, its name, the prefix its tools
// wear), because any of them identifies it in a lookup.
//
// The ids are read here rather than carried on the schemas, because this is the
// one place that both knows which connections exist and is building a prompt.
// A connection whose tools this person is not granted is not listed at all: it
// is not reachable, and naming it would send the model looking for nothing.
func (a *App) connectedIntegrations(ctx context.Context, workspaceID int64, held []tool.Schema) []connectedService {
	if len(held) == 0 {
		return nil
	}
	names, err := a.mcpServerNames(ctx, workspaceID)
	if err != nil {
		// Not a reason to fail a turn: the model loses the ids, not the ability.
		a.Log.Warn().Err(err).Int64("workspace_id", workspaceID).Msg("name the connected services")
	}
	ids := make(map[string]int64, len(names))
	for id, name := range names {
		ids[name] = id
	}
	services := connectedServices(ids, held)
	listed := make([]connectedService, 0, len(services))
	for _, s := range services {
		listed = append(listed, connectedService{id: s.ID, name: s.Name, alias: s.Alias})
	}
	return listed
}

// connectedServices names the services this turn holds tools from, with the
// alias their tools carry.
//
// Only services something was actually kept from: a connection whose tools this
// person is not granted is not a service they can reach, and listing it would
// invite the model to look for what is not there.
func connectedServices(ids map[string]int64, held []tool.Schema) []integrations.Service {
	if len(held) == 0 {
		return nil
	}
	seen := map[string]bool{}
	services := make([]integrations.Service, 0, len(ids))
	for _, schema := range held {
		service := serviceOfSchema(schema)
		if service == "" || seen[service] {
			continue
		}
		seen[service] = true
		services = append(services, integrations.Service{
			ID: ids[service], Name: service, Alias: aliasOf(schema.Name),
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services
}

// serviceOfSchema is which connected service a tool came from, empty for one of
// ours. It used to cut the name back out of the approval title ("Search, on
// CRM"), which was prose written for a person to read on a card; the schema
// carries the fact now.
func serviceOfSchema(schema tool.Schema) string {
	return schema.Service
}

// shownAs is what a PROJECTED tool is called on the screen and in the
// transcript: the connection it came from, then the tool.
//
// "Flexie Search" on its own does not say whose search it is, and a workspace
// can connect two services that both offer one, so a person reading what the
// assistant just did had no way to tell them apart. The name a person sees is
// now the pair, and because the frame and the stored row are the same string
// (agent_tool_calls.friendly_name is written from this), the transcript reads
// the same way afterwards as it did live.
//
// Only a projected tool gets a prefix. A built-in IS the product, and naming
// the product inside its own interface says nothing.
//
// Bounded, because it is two columns joined into one. Both halves are
// varchar(255) (tools.friendly_name, mcp_servers.name), so an unbounded join
// reaches 513 characters against the varchar(255) it is stored in, and the
// database runs in STRICT_TRANS_TABLES where that is error 1406: a failed
// insert, not a quiet truncation, which would take the tool call down with it.
// The service keeps its room and the tool's half gives way, since the prefix is
// the half that tells two services apart. Same shape as truncateError in the
// store, cutting back to a rune boundary rather than through one.
func shownAs(service, friendly string) string {
	if service == "" {
		return friendly
	}
	const max = 255
	const sep = " / "
	name := service + sep + friendly
	if len(name) <= max {
		return name
	}
	cut := max - len("…")
	for cut > 0 && !utf8.ValidString(name[:cut]) {
		cut--
	}
	return name[:cut] + "…"
}

// aliasOf is the prefix a service's tools carry, which is what makes a name
// unambiguous when two services both offer a "search" (KB/20).
func aliasOf(toolName string) string {
	before, _, found := strings.Cut(toolName, "_")
	if !found {
		return ""
	}
	return before
}

// terminalPolicy reads what an administrator wrote in the tool's settings, over
// what the tool declares it ships as.
//
// The second half is the whole point, and its absence made the terminal a tool
// that could not run anything. A row nobody has configured has no settings, and
// this returned the zero policy: an empty mode, which cmdpolicy reads as an
// allowlist, and an empty allowlist permits nothing. Meanwhile the tool's own
// form declares its rule ships as a denylist, so an administrator opening the
// settings read "Every command except the ones I list" and the model was told
// no commands had been permitted and an administrator would have to choose
// some. Both halves were defensible and they described different tools.
//
// tool.Settings is now the one answer to "what are this tool's settings": what
// is stored, and the declared default for what is not. Merged per key, so a
// policy saved without a rule gets the declared rule rather than the zero value
// of the type that parses it.
// Settings that cannot be read are refused rather than defaulted. The declared
// default is a denylist, so reading a corrupt row as "nothing was configured"
// would turn it into a terminal that runs anything; the zero policy is an
// allowlist of nothing, which refuses.
func terminalPolicy(schema tool.Schema, config json.RawMessage) (cmdpolicy.Policy, error) {
	settings, failed := tool.SettingsConfig(schema.Settings, config)
	if failed != nil {
		return cmdpolicy.Policy{}, failed
	}
	var stored struct {
		Policy cmdpolicy.Policy `json:"policy"`
	}
	if err := json.Unmarshal(settings, &stored); err != nil {
		return cmdpolicy.Policy{}, fmt.Errorf("read the terminal's policy: %w", err)
	}
	return stored.Policy, nil
}

// mcpServerNames maps connection ids to their names, for the approval cards.
func (a *App) mcpServerNames(ctx context.Context, workspaceID int64) (map[int64]string, error) {
	servers, err := a.Store.MCPServers().List(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list mcp connections: %w", err)
	}
	names := make(map[int64]string, len(servers))
	for _, s := range servers {
		names[s.ID] = s.Name
	}
	return names, nil
}
