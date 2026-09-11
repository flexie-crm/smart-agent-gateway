package agent

import (
	"context"
	"strings"
	"time"

	"flexie.io/sag/internal/provider"
)

// Naming a conversation.
//
// The title runs through the SAME gateway as the conversation itself: the same
// model routing, the same credentials, the same trimming. It is not a special
// path with its own rules, because the moment it is, it drifts.
//
// It runs detached from the request. The user is not waiting on it: their
// answer already streamed, and a title is worth nothing if it delays the
// reply. If it fails, the chat simply keeps its default name.

// titleTimeout bounds the work. A title is a nicety, and a nicety does not get
// to hold a goroutine open indefinitely.
const titleTimeout = 30 * time.Second

// titlePrompt asks for a label, not an answer. Models want to be helpful and
// will write a sentence unless told plainly not to.
const titlePrompt = `Write a title for this conversation.

Rules:
- Between two and six words.
- No quotes, no punctuation at the end, no prefix like "Title:".
- Plain nouns. Describe the subject, not the exchange ("Invoice rounding bug",
  never "User asks about invoices").
- Use the language the user wrote in.

Reply with the title and nothing else.`

// maxTitleChars stops a model that ignored the instructions from writing an
// essay into a column that a sidebar has to render.
const maxTitleChars = 80

// NameConversation generates and stores a title, in the background.
//
// It is fire-and-forget by design, and it owns its own context: the request
// that triggered it has already finished, so inheriting that context would
// cancel the work the moment the answer was sent.
func (r *Runner) NameConversation(turn Turn, firstPrompt, firstAnswer string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
		defer cancel()

		title, err := r.generateTitle(ctx, turn, firstPrompt, firstAnswer)
		if err != nil {
			// A conversation without a generated title is a conversation with
			// its default name. That is a cosmetic loss, not a failure worth
			// surfacing to anyone.
			r.log.Warn().Err(err).Int64("session_id", turn.SessionID).
				Msg("could not name the conversation")
			return
		}
		if title == "" {
			// Said out loud, because this was the silent one. The model answered
			// and what came back cleaned up to nothing, which leaves a
			// conversation unnamed with no error anywhere and nothing in the log
			// to find later. A conversation with 14 steps and no name and not one
			// line about why is how an afternoon goes.
			r.log.Warn().Int64("session_id", turn.SessionID).
				Msg("the model returned nothing usable to name the conversation")
			return
		}
		if err := r.store.Agent().SetSessionTitle(ctx, turn.SessionID, title); err != nil {
			r.log.Error().Err(err).Int64("session_id", turn.SessionID).Msg("store title")
		}
	}()
}

func (r *Runner) generateTitle(ctx context.Context, turn Turn, prompt, answer string) (string, error) {
	resolved, err := r.gateway.Resolve(ctx, turn.WorkspaceID, turn.ModelID)
	if err != nil {
		return "", err
	}

	// The exchange, not the whole transcript: a title is about what was asked,
	// and sending more costs more for no better answer.
	req := resolved.Prepare(provider.GenerateRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: titlePrompt},
			{Role: provider.RoleUser, Content: exchange(prompt, answer)},
		},
		// max_completion_tokens is the TOTAL budget: a reasoning model (OpenAI's
		// o-series, gpt-5) spends most of it thinking before it writes a single
		// character, so a tight cap starves it and returns an empty title. This
		// leaves room to think; the title is short regardless (cleanTitle trims).
		MaxTokens: 2000,
		// Reasoning OUTPUT is still not requested: a label is not worth showing
		// the model's thinking.
		Reasoning: false,
	})

	resp, err := resolved.Provider.Generate(ctx, req)
	if err != nil {
		return "", err
	}
	r.recordModelCall(ctx, turn, resolved, time.Now(), resp.Usage)

	return cleanTitle(resp.Message.Content), nil
}

func exchange(prompt, answer string) string {
	var b strings.Builder
	b.WriteString("User: ")
	b.WriteString(truncate(prompt, 1000))
	if answer != "" {
		b.WriteString("\n\nAssistant: ")
		b.WriteString(truncate(answer, 1000))
	}
	return b.String()
}

// cleanTitle takes what the model wrote and makes it fit a sidebar. Models
// wrap titles in quotes, prefix them, and end them with a full stop no matter
// how clearly they were asked not to.
func cleanTitle(raw string) string {
	title := strings.TrimSpace(raw)

	// A model that explained itself gets read up to its first line only.
	if line, _, found := strings.Cut(title, "\n"); found {
		title = strings.TrimSpace(line)
	}
	title = strings.TrimPrefix(title, "Title:")
	title = strings.TrimSpace(title)
	title = strings.Trim(title, `"'“”`)
	title = strings.TrimRight(title, ".!,;:")
	title = strings.TrimSpace(title)

	return truncate(title, maxTitleChars)
}

// truncate cuts on a rune boundary, so a title in any language survives.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max]))
}
