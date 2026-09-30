package app

import (
	"context"
	"encoding/json"
	"strconv"

	"flexie.io/sag/internal/chat"
	"flexie.io/sag/internal/model"
)

// The timeline is what the chat draws for a conversation: a message per step,
// carrying what the step said, what it thought on the way, and the tools it
// called. The history a conversation opens with, one agent's work opened from
// its chip, and a step pushed while somebody watches that agent all draw it,
// so it is built in ONE place and a message looks the same whichever of the
// three delivered it.

// ChatMessage is one step, in the shape the chat renders.
type ChatMessage struct {
	ID        string       `json:"id"`
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Reasoning string       `json:"reasoning,omitempty"`
	Tools     []ChatTool   `json:"tools,omitempty"`
	Confirm   *ChatConfirm `json:"confirmation,omitempty"`
	// Attachments are the files sent with this message, so a reloaded
	// conversation still shows what was attached rather than a question with no
	// visible reason for the answer it got.
	Attachments []ChatAttachment `json:"attachments,omitempty"`
}

// ChatAttachment is a file as the chat shows it back: enough to draw the card,
// and the id to fetch the bytes with. The account of what it contained is not
// here, being for the model rather than for the person.
type ChatAttachment struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	FileType  string `json:"file_type"`
	SizeBytes int64  `json:"size_bytes"`
}

// ChatTool is one tool call as a row: what it is called, how it went, and the
// id to open it with.
type ChatTool struct {
	// ID names this call, so the chat can ask what it carried when somebody
	// opens it.
	ID                string `json:"id"`
	Name              string `json:"name"`
	FriendlyName      string `json:"friendly_name"`
	Status            string `json:"status"`
	DurationMS        int64  `json:"duration_ms"`
	RequestedApproval bool   `json:"requested_approval,omitempty"`
}

// ChatConfirm is an approval card, rebuilt for a conversation that is waiting
// on one.
type ChatConfirm struct {
	Token       string          `json:"token"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Severity    string          `json:"severity"`
	Details     json.RawMessage `json:"details,omitempty"`
	Status      string          `json:"status"`
}

// ShownStep is a step as the chat shows it to a person who may see what show
// says, and false when there is nothing in it to show.
//
// The timeline is rebuilt from the step: the text it produced, the reasoning it
// produced on the way there, and every tool it called with the outcome. A step
// that only called tools has no text and is drawn as its rows, which is exactly
// what the live stream showed at the time.
func (a *App) ShownStep(ctx context.Context, workspaceID int64, step *model.AgentStep, show chat.Show) (ChatMessage, bool) {
	message := ChatMessage{
		ID:      strconv.FormatInt(step.ID, 10),
		Role:    step.Kind,
		Content: step.Text,
	}
	// The same rule the live stream applies, applied again here. A person
	// who was not shown the thinking while it happened must not find it by
	// reloading, and the transcript keeps it either way: what this decides
	// is who is shown the record, not whether it is kept.
	if show.Reasoning {
		message.Reasoning = step.Reasoning
	}
	// The same rule again, for what the assistant DID. A person the live
	// stream withheld the tool rows from must not find them by opening the
	// conversation again: the frames and this answer say the same thing, or
	// the permission is a delay rather than a decision.
	for _, call := range step.ToolCalls {
		if !show.Tools {
			break
		}
		// The row, and the id to open it with. What it was sent and what it
		// answered are not here: they are the largest thing in a
		// conversation and the one thing a person may not be allowed to
		// see, so they are fetched when somebody asks (`/chat/tool-call`).
		//
		// No id on an INTERNAL tool, so it cannot be opened at all. Looking
		// up an ability, handing work to an agent, keeping a note: these are
		// how the assistant is put together, not work it did for somebody,
		// and their arguments are our own plumbing. Withholding the id is
		// structural: there is nothing to open rather than a panel that
		// refuses.
		message.Tools = append(message.Tools, ChatTool{
			ID:                a.OpenableID(call),
			Name:              call.ToolName,
			FriendlyName:      call.FriendlyName,
			Status:            call.Status,
			DurationMS:        call.DurationMS,
			RequestedApproval: call.RequestedApproval,
		})
	}
	for _, id := range step.Attachments {
		at, err := a.Store.Attachments().ByID(ctx, workspaceID, id)
		if err != nil {
			// A file that has been cleaned up is not a reason to lose the
			// message it came with.
			continue
		}
		message.Attachments = append(message.Attachments, ChatAttachment{
			ID: at.PublicID, FileName: at.FileName,
			FileType: at.FileType, SizeBytes: at.SizeBytes,
		})
	}
	if message.Content == "" && len(message.Tools) == 0 && message.Reasoning == "" &&
		len(message.Attachments) == 0 {
		// An empty step is the wreckage of an interrupted turn. There is
		// nothing to show and nothing worth explaining.
		return ChatMessage{}, false
	}
	return message, true
}

// OpenableID is the id a tool row is opened by, or nothing for a tool that
// should not be opened.
//
// An internal tool is infrastructure: tool_guide, delegation, remember, the
// background controls. What they were sent is our own wiring and reads as noise
// beside the work somebody actually asked for. Everything else is openable, a
// custom tool and a projected one included: those do real work, and what they
// carried is exactly what somebody wants to see.
//
// Asked of the app, not of the registry. Half of these tools are never
// registered (they are built per turn from the roster), so a registry lookup
// answers "not ours" for delegate and delegate_fleet, which is how the fleet
// tool ended up with an arrow on it.
func (a *App) OpenableID(call *model.ToolCall) string {
	if a.InternalTool(call.ToolName) {
		return ""
	}
	return strconv.FormatInt(call.ID, 10)
}

// MaySee is what a person may be told of HOW an answer was reached: the
// model's thinking, and the tools it used.
//
// Asked live, per request, like every other permission here: somebody whose
// role changed this morning sees the change on their next message rather than
// when their token expires.
//
// A failure to ask is a refusal, not a pass. This is the one direction that is
// safe to be wrong in: showing less than somebody is entitled to is a support
// question, and showing more is a disclosure.
func (a *App) MaySee(ctx context.Context, userID int64) chat.Show {
	show := chat.Show{}
	for _, allowed := range []struct {
		permission string
		field      *bool
	}{
		{model.PermChatsSeeReasoning, &show.Reasoning},
		{model.PermChatsSeeTools, &show.Tools},
	} {
		ok, err := a.Authorize(ctx, userID, allowed.permission)
		if err != nil {
			a.Log.Error().Err(err).Int64("user_id", userID).
				Str("permission", allowed.permission).Msg("resolve what a person may see")
			continue
		}
		*allowed.field = ok
	}
	return show
}
