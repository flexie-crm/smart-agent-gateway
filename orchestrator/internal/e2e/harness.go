package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The identity the Playwright tests sign in with. Constants, so the spec and the
// seed cannot drift apart.
const (
	WorkspaceSlug = "e2e"
	Email         = "e2e@acme.test"
	// Onlooker may use the chat and may not be told how an answer was reached.
	Onlooker = "watcher@acme.test"
	Password = "e2e-password-1234"
)

// Seed puts the minimum into a fresh database for the background-delegation flow
// (KB/27): a workspace and a member who can sign in through the real login, a
// model on the scripted provider, a Gateway agent, and one background agent
// whose single tool is approval-gated so the flow parks on a real card. It is
// idempotent: an already-seeded database (its Gateway agent present) is left as
// it is, so a restart of the harness does not duplicate rows.
func Seed(ctx context.Context, a *app.App) error {
	st := a.Store

	ws, err := st.Workspaces().GetBySlug(ctx, WorkspaceSlug)
	if err != nil {
		ws = &model.Workspace{Slug: WorkspaceSlug, Name: "E2E"}
		if err := st.Workspaces().Create(ctx, ws); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
	}

	// The build's tools, offered to the workspace, so the agents' tool names
	// resolve to real grants.
	if err := a.SyncTools(ctx, ws.ID); err != nil {
		return fmt.Errorf("sync tools: %w", err)
	}

	if _, err := st.Users().GetByEmail(ctx, Email); err != nil {
		hash, herr := auth.HashPassword(Password)
		if herr != nil {
			return fmt.Errorf("hash password: %w", herr)
		}
		u := &model.User{Email: Email, Name: "E2E", PasswordHash: hash}
		if err := st.Users().Create(ctx, u); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		if err := st.Workspaces().SetMembers(ctx, u.ID, []int64{ws.ID}); err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		if err := permit(ctx, a, ws.ID, u.ID); err != nil {
			return err
		}
	}

	// A second person, who may use the chat and may NOT see how an answer was
	// reached. Seeded because that permission is only testable with somebody
	// who lacks it, and a test that grants and revokes as it goes is testing
	// its own setup as much as the rule.
	if _, err := st.Users().GetByEmail(ctx, Onlooker); err != nil {
		hash, herr := auth.HashPassword(Password)
		if herr != nil {
			return fmt.Errorf("hash password: %w", herr)
		}
		u := &model.User{Email: Onlooker, Name: "Onlooker", PasswordHash: hash}
		if err := st.Users().Create(ctx, u); err != nil {
			return fmt.Errorf("create the onlooker: %w", err)
		}
		if err := st.Workspaces().SetMembers(ctx, u.ID, []int64{ws.ID}); err != nil {
			return fmt.Errorf("add the onlooker: %w", err)
		}
		if err := permitLess(ctx, a, ws.ID, u.ID); err != nil {
			return err
		}
	}

	// A conversation long enough to have to be read in pages.
	//
	// Seeded rather than produced by talking, because what it exists to exercise
	// is READING one: the gate scrolls back through it and asserts that a page
	// arrives once, that the reader stays where they were, and that opening it
	// does not build the whole thing. Talking a 250-row conversation into
	// existence would take the gate minutes and prove nothing extra.
	if err := seedLongConversation(ctx, a, ws.ID); err != nil {
		return err
	}
	if err := seedCodeConversation(ctx, a, ws.ID); err != nil {
		return err
	}

	// The Gateway agent's presence is the "already seeded" mark: create the model
	// and both agents together, once.
	if _, err := st.Agents().GetByKey(ctx, ws.ID, model.DefaultAgentKey); err == nil {
		return nil
	}

	vendor := &model.AIVendor{WorkspaceID: ws.ID, VendorKey: model.VendorAnthropic, Name: "Scripted", Status: model.StatusActive}
	if err := st.Vendors().Create(ctx, vendor); err != nil {
		return fmt.Errorf("create vendor: %w", err)
	}
	m := &model.AIModel{
		WorkspaceID: ws.ID, VendorID: vendor.ID, ModelKey: "scripted-1",
		Type: model.ModelTypeChat, ContextWindow: 100_000, Status: model.StatusActive,
	}
	if err := st.AIModels().Create(ctx, m); err != nil {
		return fmt.Errorf("create model: %w", err)
	}

	// A shared knowledge base the Gateway writes to (parked on approval), and the
	// Gateway's own memory brain (written silently). Both unlocked.
	knowledge := &model.Brain{WorkspaceID: ws.ID, Name: KnowledgeBrain}
	if err := st.Brains().CreateBrain(ctx, knowledge); err != nil {
		return fmt.Errorf("create knowledge brain: %w", err)
	}
	if err := st.Brains().CreateCategory(ctx, ws.ID,
		&model.BrainCategory{BrainID: knowledge.ID, Name: KnowledgeCategory}); err != nil {
		return fmt.Errorf("create category: %w", err)
	}
	memory := &model.Brain{WorkspaceID: ws.ID, Name: MemoryBrain}
	if err := st.Brains().CreateBrain(ctx, memory); err != nil {
		return fmt.Errorf("create memory brain: %w", err)
	}
	memoryID := memory.ID

	gateway := &model.Agent{
		WorkspaceID: ws.ID, Key: model.DefaultAgentKey, Name: "Assistant",
		Instructions: "You are the E2E assistant.", ModelID: &m.ID,
		Status: model.StatusActive,
		// current_time for the delegation flow; brain_write (confirmed, so it parks)
		// plus the assigned knowledge and memory brains for the brain-tool flow.
		Tools: []string{"current_time", "brain_write"}, ConfirmTools: []string{"brain_write"},
		Brains: []int64{knowledge.ID}, MemoryBrainID: &memoryID,
	}
	if err := st.Agents().Create(ctx, gateway); err != nil {
		return fmt.Errorf("create gateway: %w", err)
	}

	// A second agent, identical except that its tool does NOT ask.
	//
	// It exists so a spec about BATCH MECHANICS is not also a spec about the
	// approval toggle. Turning approvals off through the UI is an optimistic
	// write: the label flips before the server has it, so a test that clicks it
	// and carries straight on can start a batch the server still thinks must
	// ask, and then waits for a result that is sitting behind a card nobody in
	// that test is looking for. Which agent runs is a fact about the seed, and
	// facts about the seed do not race.
	quiet := &model.Agent{
		WorkspaceID: ws.ID, Key: QuietAgentKey, Name: "Quiet Worker",
		Instructions: "You check the current time. You never ask permission.", ModelID: &m.ID,
		Status: model.StatusActive,
		Tools:  []string{AgentTool},
	}
	if err := st.Agents().Create(ctx, quiet); err != nil {
		return fmt.Errorf("create quiet agent: %w", err)
	}

	agent := &model.Agent{
		WorkspaceID: ws.ID, Key: AgentKey, Name: "Background Worker",
		Instructions: "You check the current time as a background task.", ModelID: &m.ID,
		Status: model.StatusActive,
		// Its one tool is approval-gated, so the flow parks on a real card.
		Tools: []string{AgentTool}, ConfirmTools: []string{AgentTool},
	}
	if err := st.Agents().Create(ctx, agent); err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	return nil
}

// permit gives the seeded person what somebody using the chat holds.
//
// A member of a workspace and nothing else has no permissions at all, which
// used to be invisible here because the chat routes asked only for a valid
// token. They no longer do: what an answer was reached BY is a permission of
// its own, so without this the gate drives a person who cannot see a tool row,
// and every spec about one passes or fails for the wrong reason.
//
// Granted through a group and a role, the way a deployment does it, rather than
// by writing permissions onto the person: there is no other route, and a seed
// that invented one would be seeding a shape the product does not have.
func permit(ctx context.Context, a *app.App, workspaceID, userID int64) error {
	st := a.Store
	role := &model.Role{
		WorkspaceID: workspaceID,
		Name:        "Chat",
		Permissions: []string{
			model.PermChatsSeeReasoning,
			model.PermChatsSeeTools,
			model.PermChatsDelete,
		},
	}
	if err := st.Roles().Create(ctx, role); err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	group := &model.Group{WorkspaceID: workspaceID, Name: "Everyone"}
	if err := st.Groups().Create(ctx, group); err != nil {
		return fmt.Errorf("create group: %w", err)
	}
	if err := st.Groups().AssignRole(ctx, group.ID, role.ID); err != nil {
		return fmt.Errorf("assign role: %w", err)
	}
	if err := st.Groups().AddMember(ctx, group.ID, userID); err != nil {
		return fmt.Errorf("add to group: %w", err)
	}
	return nil
}

// permitLess gives somebody leave to USE the chat and nothing more: they may
// hold a conversation and delete their own, and may not be told which tools ran
// or what the assistant was thinking. Seeded because a permission is only
// testable against somebody who lacks it.
func permitLess(ctx context.Context, a *app.App, workspaceID, userID int64) error {
	st := a.Store
	role := &model.Role{
		WorkspaceID: workspaceID,
		Name:        "Chat, without the workings",
		Permissions: []string{model.PermChatsDelete},
	}
	if err := st.Roles().Create(ctx, role); err != nil {
		return fmt.Errorf("create the plain role: %w", err)
	}
	group := &model.Group{WorkspaceID: workspaceID, Name: "Onlookers"}
	if err := st.Groups().Create(ctx, group); err != nil {
		return fmt.Errorf("create the onlookers: %w", err)
	}
	if err := st.Groups().AssignRole(ctx, group.ID, role.ID); err != nil {
		return fmt.Errorf("assign the plain role: %w", err)
	}
	return st.Groups().AddMember(ctx, group.ID, userID)
}

// LongConversationTitle is what the gate clicks to find it.
const LongConversationTitle = "A conversation worth scrolling"

// CodeConversationTitle is the other shape a real conversation comes in.
//
// Two fixtures because real conversations are not one thing. The long one is a
// session of work: hundreds of tool calls, thinking before most answers, and not
// a fenced block in it. This one is the other half of the day, where the answers
// are code. They cost completely different things to draw, and tuning against
// either alone points the work at the wrong cost: the first hid every landing
// fault behind uniform rows, the second sent me tuning a syntax highlighter for
// a conversation that contains no code.
const CodeConversationTitle = "A conversation full of code"

// The conversation is shaped after a REAL one, because the shape is the whole
// point of it.
//
// The one it copies is 284 steps: 108 messages averaging 246 characters with a
// couple over four thousand, 207 reasoning blocks averaging 1,747, and FOUR
// HUNDRED tool calls across nine tools, whose results run from 95 characters to
// nearly five thousand. Not one fenced code block in the whole thing.
//
// That last detail is why this is written down. The fixture before it was 260
// one-line rows, a quarter of them code blocks, and it was wrong in both
// directions at once: uniform heights are the one shape a virtual list cannot
// get wrong, so it hid every landing and anchoring fault, while the code blocks
// pointed the tuning at a cost the real conversation does not have. What the
// real one is made of is TOOL CALLS, hundreds of them, each a row that draws a
// name, a duration and a result somebody can open.
const (
	longConversationTurns = 142
	// How many of the assistant's turns think before they answer, and how many
	// tool calls a turn makes, both taken from the counts above.
	reasoningInEvery = 4 // 3 of every 4
	toolCallsPerTurn = 3
)

func seedLongConversation(ctx context.Context, a *app.App, workspaceID int64) error {
	st := a.Store
	person, err := st.Users().GetByEmail(ctx, Email)
	if err != nil {
		return fmt.Errorf("find the seeded person: %w", err)
	}

	chats, err := st.Agent().ListChats(ctx, store.ChatQuery{
		WorkspaceID: workspaceID, UserID: person.ID, Limit: 100,
	})
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}
	for _, chat := range chats {
		if chat.Title == LongConversationTitle {
			return nil // already seeded
		}
	}

	session := &model.AgentSession{
		WorkspaceID: workspaceID,
		UserID:      person.ID,
		Channel:     model.ChannelChat,
		Title:       LongConversationTitle,
	}
	if err := st.Agent().CreateSession(ctx, session); err != nil {
		return fmt.Errorf("create the long conversation: %w", err)
	}

	for turn := 0; turn < longConversationTurns; turn++ {
		seq := turn * 2
		asked := &model.AgentStep{
			SessionID: session.ID, Seq: seq, Kind: model.StepUser,
			Text:      seededQuestion(turn),
			CreatedAt: time.Now().UTC(),
		}
		if err := st.Agent().SaveStep(ctx, asked); err != nil {
			return fmt.Errorf("write a question: %w", err)
		}
		answered := &model.AgentStep{
			SessionID: session.ID, Seq: seq + 1, Kind: model.StepAssistant,
			Text:      seededAnswer(turn),
			CreatedAt: time.Now().UTC(),
			ToolCalls: seededToolCalls(session.ID, workspaceID, turn),
		}
		if turn%reasoningInEvery != 0 {
			answered.Reasoning = seededReasoning(turn)
		}
		if err := st.Agent().SaveStep(ctx, answered); err != nil {
			return fmt.Errorf("write an answer: %w", err)
		}
	}
	return nil
}

// What the person asks. Short, the way questions are.
func seededQuestion(turn int) string {
	return fmt.Sprintf("question %d: have a look at that and tell me what you find", turn)
}

// What the assistant says. Mostly a couple of sentences, and every so often the
// long one that a whole screen belongs to, which is the height a list has to
// discover rather than guess.
func seededAnswer(turn int) string {
	if turn%17 == 0 {
		para := "Going through it in order: the call is made where the row is built rather than where it runs, which is why the name on the row was the one nobody wanted. "
		out := fmt.Sprintf("answer %d.\n\n", turn)
		for i := 0; i < 12; i++ {
			out += para + "\n\n"
		}
		return out
	}
	return fmt.Sprintf("answer %d: found it, fixed it, and the test now fails on the old code, which is the part that matters.", turn)
}

// What it thought first. Collapsed on the page, but it is real content that has
// to be measured, and there is a lot of it.
func seededReasoning(turn int) string {
	line := "Checking whether the row is built before or after the call is resolved, because that decides which name is shown. "
	out := ""
	for i := 0; i < 15; i++ {
		out += line
	}
	return fmt.Sprintf("%d: %s", turn, out)
}

// The tool calls, in the proportions the real conversation has them, with
// results the size the real ones are: a file read comes back with thousands of
// characters, an edit with a line.
func seededToolCalls(sessionID, workspaceID int64, turn int) []*model.ToolCall {
	kinds := []struct {
		name   string
		nice   string
		result int
	}{
		{"terminal", "Terminal", 1400},
		{"edit_file", "Edit file", 95},
		{"read_file", "Read file", 4900},
		{"search_files", "Search files", 2300},
	}
	calls := make([]*model.ToolCall, 0, toolCallsPerTurn)
	for i := 0; i < toolCallsPerTurn; i++ {
		k := kinds[(turn+i)%len(kinds)]
		body := strings.Repeat("output line that a tool came back with. ", 1+k.result/40)
		payload, _ := json.Marshal(map[string]string{"output": body})
		calls = append(calls, &model.ToolCall{
			SessionID: sessionID, WorkspaceID: workspaceID,
			ToolCallID:     fmt.Sprintf("call_seed_%d_%d", turn, i),
			ToolName:       k.name,
			FriendlyName:   k.nice,
			Args:           json.RawMessage(`{"path":"orchestrator/internal/api/server.go"}`),
			Result:         payload,
			Status:         model.ToolCallCompleted,
			DurationMS:     int64(120 + i*35),
			ExecutionOrder: i,
			CreatedAt:      time.Now().UTC(),
		})
	}
	return calls
}

// seedCodeConversation is the conversation whose answers are code, which is the
// most expensive thing a chat can draw: every fenced block is a tokeniser run.
func seedCodeConversation(ctx context.Context, a *app.App, workspaceID int64) error {
	st := a.Store
	person, err := st.Users().GetByEmail(ctx, Email)
	if err != nil {
		return fmt.Errorf("find the seeded person: %w", err)
	}
	chats, err := st.Agent().ListChats(ctx, store.ChatQuery{
		WorkspaceID: workspaceID, UserID: person.ID, Limit: 100,
	})
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}
	for _, chat := range chats {
		if chat.Title == CodeConversationTitle {
			return nil // already seeded
		}
	}

	session := &model.AgentSession{
		WorkspaceID: workspaceID,
		UserID:      person.ID,
		Channel:     model.ChannelChat,
		Title:       CodeConversationTitle,
	}
	if err := st.Agent().CreateSession(ctx, session); err != nil {
		return fmt.Errorf("create the code conversation: %w", err)
	}

	for turn := 0; turn < 40; turn++ {
		asked := &model.AgentStep{
			SessionID: session.ID, Seq: turn * 2, Kind: model.StepUser,
			Text:      fmt.Sprintf("question %d: show me how that handler should look", turn),
			CreatedAt: time.Now().UTC(),
		}
		if err := st.Agent().SaveStep(ctx, asked); err != nil {
			return fmt.Errorf("write a question: %w", err)
		}
		answered := &model.AgentStep{
			SessionID: session.ID, Seq: turn*2 + 1, Kind: model.StepAssistant,
			Text:      codeAnswer(turn),
			CreatedAt: time.Now().UTC(),
		}
		if err := st.Agent().SaveStep(ctx, answered); err != nil {
			return fmt.Errorf("write an answer: %w", err)
		}
	}
	return nil
}

func codeAnswer(turn int) string {
	return fmt.Sprintf("answer %d, with something to look at:\n\n```go\nfunc handle(ctx context.Context, in Request) (Response, error) {\n\tif err := in.Validate(); err != nil {\n\t\treturn Response{}, fmt.Errorf(\"validate: %%w\", err)\n\t}\n\tout, err := run(ctx, in)\n\tif err != nil {\n\t\treturn Response{}, fmt.Errorf(\"run: %%w\", err)\n\t}\n\treturn out, nil\n}\n```\n\nAnd a line after it, because a code block is rarely the last thing said.", turn)
}
