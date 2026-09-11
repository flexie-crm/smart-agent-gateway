package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
)

// Turning an uploaded file into something the Gateway can think with.
//
// A screenshot is not a prompt and a contract is not a prompt. Each is read
// FIRST, by the model the Gateway's rules name for that type, and what comes
// out is text that travels with the person's words. The Gateway then has the
// whole picture in one turn instead of holding a file it cannot open.
//
// # Why it is read once and kept
//
// The extraction is stored on the attachment. Re-reading the same PDF on every
// turn of a conversation would cost the same money again, take the same time
// again, and, because a model is not a function, answer slightly differently
// each time, so the transcript would quietly disagree with itself about what
// the file said. Read once, keep, reuse.

// extractionTimeout bounds reading one file. It is longer than a chat turn
// because a large document legitimately takes a while, and shorter than
// forever because a person is waiting.
const extractionTimeout = 3 * time.Minute

// extractionMaxTokens caps what comes back. A description of a file, not a
// retelling of it: past this length the Gateway is better served by asking the
// file a question than by holding all of it.
const extractionMaxTokens = 4000

// extractionPrompt is what the reading model is asked for.
//
// A faithful account rather than an opinion, because this model is not the one
// answering the person: whatever it leaves out, the Gateway can never see. It
// is asked to read, and nothing else. What the result IS, somebody's file
// rather than somebody's instruction, is said where it matters, in the
// narration the Gateway receives.
const extractionPrompt = `Describe the contents of this file completely and faithfully, so that somebody who cannot see it can work with it.

Include the text it contains, its structure, and anything visible that carries meaning: tables, figures, labels, handwriting, signatures, stamps.

Report what is there. Do not summarize away detail, and do not add anything that is not there.`

// extractionOf is the text of an attachment, read once and kept.
//
// It returns what was stored if the file has already been read. Otherwise it
// finds the rule that covers this file's type, reads the file with that rule's
// model, and stores the result.
func (a *App) extractionOf(ctx context.Context, gateway *model.Agent, at *model.Attachment) (string, error) {
	if at.ExtractedAt != nil {
		return at.Extraction, nil
	}

	modelID, ok := readerFor(gateway, at.FileType)
	if !ok {
		// The upload gate refuses a type no rule covers, so getting here means
		// the rules changed after the file was accepted. The person is not shown
		// this; the Gateway is told the file could not be read.
		return "", fmt.Errorf("no rule covers a %s file", at.FileType)
	}

	mediaType := model.MediaType(at.FileType)
	if mediaType == "" {
		return "", fmt.Errorf("a %s file has no type a model could be told", at.FileType)
	}

	f, err := a.Files.Open(at.WorkspaceID, at.PublicID)
	if err != nil {
		return "", fmt.Errorf("open attachment: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read attachment: %w", err)
	}

	resolved, err := a.Gateway.Resolve(ctx, at.WorkspaceID, modelID)
	if err != nil {
		return "", fmt.Errorf("resolve the model that reads a %s: %w", at.FileType, err)
	}

	ctx, cancel := context.WithTimeout(ctx, extractionTimeout)
	defer cancel()

	prepared := resolved.Prepare(provider.GenerateRequest{
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: extractionPrompt,
			Files: []provider.FilePart{{
				FileName: at.FileName, MediaType: mediaType, Data: data,
			}},
		}},
		MaxTokens: extractionMaxTokens,
		Reasoning: false,
	})
	resp, err := resolved.Provider.Generate(ctx, prepared)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", at.FileName, err)
	}

	text := strings.TrimSpace(resp.Message.Content)
	if text == "" {
		return "", fmt.Errorf("%s was read and produced nothing", at.FileName)
	}
	// Stored on the way past. A failure to store is not a failure to read: the
	// person gets their answer, and the next turn reads the file again.
	if err := a.Store.Attachments().SaveExtraction(ctx, at.ID, text); err != nil {
		a.Log.Warn().Err(err).Str("attachment", at.PublicID).Msg("extraction not stored")
	}
	return text, nil
}

// readerFor is the model that reads this file type: the first rule that covers
// it wins, which is what makes the order of the rules mean something.
func readerFor(gateway *model.Agent, fileType string) (int64, bool) {
	if gateway == nil {
		return 0, false
	}
	for _, rule := range gateway.FileRules {
		if rule.Matches(fileType) {
			return rule.ModelID, true
		}
	}
	return 0, false
}

// AttachedText is our narration to the Gateway: what the person attached, and
// what each file turned out to contain.
//
// The Gateway never sees the file. It sees this, in our words, saying plainly
// that what follows is an account of something the person uploaded. That
// framing is ours to write and is the whole reason the Gateway can tell the
// difference between what the person asked and what their document happens to
// say.
//
// A file that could not be read still gets a line saying so. Silence would
// leave the Gateway answering as though nothing had been attached, while the
// person watches it ignore the thing they just sent.
func (a *App) AttachmentText(ctx context.Context, workspaceID int64, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	attachments := make([]*model.Attachment, 0, len(ids))
	for _, id := range ids {
		at, err := a.Store.Attachments().ByID(ctx, workspaceID, id)
		if err != nil {
			a.Log.Warn().Err(err).Str("attachment", id).Msg("attachment gone")
			continue
		}
		attachments = append(attachments, at)
	}
	if len(attachments) == 0 {
		return ""
	}
	gateway, err := a.Store.Agents().GetByKey(ctx, workspaceID, model.DefaultAgentKey)
	if err != nil {
		gateway = nil
	}

	var b strings.Builder
	b.WriteString("\n\nThe person attached ")
	if len(attachments) == 1 {
		b.WriteString("a file with their message. Here is what it contains, read for you:")
	} else {
		fmt.Fprintf(&b, "%d files with their message. Here is what each contains, read for you:", len(attachments))
	}

	for _, at := range attachments {
		fmt.Fprintf(&b, "\n\n--- %s ---\n", at.FileName)
		text, err := a.extractionOf(ctx, gateway, at)
		if err != nil {
			a.Log.Warn().Err(err).Str("attachment", at.PublicID).Msg("attachment not read")
			b.WriteString("This file could not be read. Tell the person so, and do not guess at what it contained.")
			continue
		}
		b.WriteString(text)
	}
	return b.String()
}
