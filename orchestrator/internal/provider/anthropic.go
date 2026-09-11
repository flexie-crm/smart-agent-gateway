package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Anthropic adapts the vendor's Messages API to the gateway contract. The
// SDK owns the wire protocol; this file owns only the translation, which is
// where every vendor quirk is allowed to live.
type Anthropic struct {
	client anthropic.Client
}

// NewAnthropic builds the adapter. baseURL is optional and exists so tests
// (and proxies) can point the SDK somewhere other than the vendor.
func NewAnthropic(apiKey, baseURL string) *Anthropic {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{client: anthropic.NewClient(opts...)}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Capabilities(_ context.Context, _ string) (Capabilities, error) {
	// Capabilities are configured per model in the registry rather than
	// probed: the vendor exposes no endpoint that reports them, and guessing
	// from a model name would break the moment a new model ships.
	return Capabilities{
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsReasoning: true,
	}, nil
}

// ListModels asks Anthropic what it offers. The vendor names its models, so a
// person choosing one reads "Claude Sonnet 5" rather than "claude-sonnet-5".
func (a *Anthropic) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models := []ModelInfo{}
	pager := a.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pager.Next() {
		info := pager.Current()
		models = append(models, ModelInfo{ID: info.ID, Name: info.DisplayName})
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("anthropic list models: %w", err)
	}
	return models, nil
}

func (a *Anthropic) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	params, err := a.buildParams(req)
	if err != nil {
		return nil, err
	}
	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic generate: %w", err)
	}

	out := Message{Role: RoleAssistant}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "thinking":
			out.Reasoning += block.Thinking
		case "tool_use":
			args, err := json.Marshal(block.Input)
			if err != nil {
				return nil, fmt.Errorf("anthropic tool arguments: %w", err)
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID: block.ID, Name: block.Name, Args: args,
			})
		}
	}
	return &GenerateResponse{
		Message: out,
		Usage: Usage{
			InputTokens:  msg.Usage.InputTokens,
			OutputTokens: msg.Usage.OutputTokens,
		},
	}, nil
}

func (a *Anthropic) Stream(ctx context.Context, req GenerateRequest) (<-chan StreamEvent, error) {
	// A model already known to refuse thinking never gets asked again
	// (unsupported.go). The same rule as the other wire, because a person
	// choosing how hard a model should think should not have to know which
	// vendor they are on, and neither should this.
	if req.Reasoning && refusesReasoning(req.Model) {
		req = withoutReasoning(req)
	}
	params, err := a.buildParams(req)
	if err != nil {
		return nil, err
	}

	events := make(chan StreamEvent, streamBuffer)
	go func() {
		defer close(events)

		spoke, failed := a.pump(ctx, params, events)

		// This model has no extended thinking and said so. Retried once, and
		// only while nothing has reached the reader: a retry replays from the
		// start, and somebody who has already seen half an answer would see it
		// twice.
		if !spoke && req.Reasoning && isReasoningRefusal(failed) {
			rememberReasoningRefusal(req.Model)
			plain, buildErr := a.buildParams(withoutReasoning(req))
			if buildErr != nil {
				emit(ctx, events, StreamEvent{Kind: EventError, Err: buildErr})
				return
			}
			_, failed = a.pump(ctx, plain, events)
		}
		if failed != nil {
			emit(ctx, events, StreamEvent{Kind: EventError, Err: failed})
		}
	}()
	return events, nil
}

// pump runs one attempt, reporting whether the model said anything and what went
// wrong, so the caller can decide whether asking again is safe.
func (a *Anthropic) pump(
	ctx context.Context,
	params anthropic.MessageNewParams,
	events chan<- StreamEvent,
) (spoke bool, failed error) {
	{
		stream := a.client.Messages.NewStreaming(ctx, params)
		// Tool arguments arrive as partial JSON across many deltas, so they
		// are assembled per content block and only emitted once complete.
		pending := map[int64]*ToolCall{}
		var usage Usage

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					pending[event.Index] = &ToolCall{
						ID:   event.ContentBlock.ID,
						Name: event.ContentBlock.Name,
					}
				}

			case "content_block_delta":
				switch event.Delta.Type {
				case "text_delta":
					spoke = true
					emit(ctx, events, StreamEvent{
						Kind: EventContentDelta, ContentDelta: event.Delta.Text,
					})
				case "thinking_delta":
					emit(ctx, events, StreamEvent{
						Kind: EventReasoningDelta, ReasoningDelta: event.Delta.Thinking,
					})
				case "input_json_delta":
					if call, ok := pending[event.Index]; ok {
						call.Args = append(call.Args, event.Delta.PartialJSON...)
					}
				}

			case "content_block_stop":
				if call, ok := pending[event.Index]; ok {
					delete(pending, event.Index)
					if len(call.Args) == 0 {
						// A tool called with no arguments still needs valid
						// JSON, or the tool loop cannot unmarshal it.
						call.Args = json.RawMessage("{}")
					}
					spoke = true
					emit(ctx, events, StreamEvent{Kind: EventToolCall, ToolCall: call})
				}

			case "message_delta":
				usage.OutputTokens += event.Usage.OutputTokens

			case "message_start":
				usage.InputTokens += event.Message.Usage.InputTokens
				usage.OutputTokens += event.Message.Usage.OutputTokens
			}
		}

		if err := stream.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				return spoke, nil // the caller went away; not a fault to report
			}
			return spoke, fmt.Errorf("anthropic stream: %w", err)
		}
		emit(ctx, events, StreamEvent{Kind: EventUsage, Usage: &usage})
		emit(ctx, events, StreamEvent{Kind: EventDone})
	}
	return spoke, nil
}

func (a *Anthropic) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	// The vendor has no embeddings endpoint. Reporting that plainly is
	// better than silently routing to a different provider.
	return nil, ErrUnsupported
}

func (a *Anthropic) buildParams(req GenerateRequest) (anthropic.MessageNewParams, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     req.Model,
		MaxTokens: maxTokens,
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			// The system prompt is a separate field, not a message.
			params.System = append(params.System, anthropic.TextBlockParam{Text: m.Content})

		case RoleUser:
			blocks, err := userBlocks(m)
			if err != nil {
				return anthropic.MessageNewParams{}, err
			}
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))

		case RoleAssistant:
			blocks := []anthropic.ContentBlockParamUnion{}
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, call := range m.ToolCalls {
				var input any
				if err := json.Unmarshal(call.Args, &input); err != nil {
					return params, fmt.Errorf("anthropic tool call %s: %w", call.Name, err)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, input, call.Name))
			}
			if len(blocks) == 0 {
				continue
			}
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))

		case RoleTool:
			// A tool result is a user-role message carrying a tool_result
			// block; the pairing with its tool_use id is what the vendor
			// validates, so it must never be broken by trimming.
			params.Messages = append(params.Messages, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false)))
		}
	}

	for _, t := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return params, fmt.Errorf("anthropic tool schema %s: %w", t.Name, err)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}

	if req.Reasoning {
		// How hard to think is CHOSEN, the same four words as every other
		// vendor, and translated here into the number this one takes. A person
		// should not have to know that one vendor calls it an effort and
		// another a token budget.
		budget := ThinkingBudget(req.Settings)

		// Two vendor rules, both of which are a rejected request if broken:
		// max_tokens must leave room for the answer on top of the thinking
		// budget, and temperature must not be set at all when thinking is on.
		if params.MaxTokens <= budget {
			params.MaxTokens = budget + defaultMaxTokens
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: budget},
		}
	} else if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}

	return params, nil
}

// userBlocks is a user message as this vendor takes it: the words, and any
// files attached to them.
//
// WHICH MODEL READS WHICH FILE IS THE ADMINISTRATOR'S DECISION, not ours. So a
// file whose type this vendor has no block for is an ERROR here rather than a
// file quietly left out: pointing a rule at a model that cannot read the type
// is a configuration mistake, and a mistake that shows itself is one somebody
// can fix. Dropping the file instead would have the Gateway answer about a
// document nobody gave it, and nothing anywhere would say why.
func userBlocks(m Message) ([]anthropic.ContentBlockParamUnion, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Files)+1)
	for _, f := range m.Files {
		switch {
		case strings.HasPrefix(f.MediaType, "image/"):
			blocks = append(blocks, anthropic.NewImageBlockBase64(f.MediaType, base64.StdEncoding.EncodeToString(f.Data)))
		case f.MediaType == "application/pdf":
			blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
				Data: base64.StdEncoding.EncodeToString(f.Data),
			}))
		case strings.HasPrefix(f.MediaType, "text/"),
			f.MediaType == "application/json", f.MediaType == "application/xml":
			// Text is sent as text rather than as bytes to be decoded, which is
			// both what this vendor wants and what a model reads better.
			blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{
				Data: string(f.Data),
			}))
		default:
			return nil, fmt.Errorf("%w: this model was given %s (%s), which it cannot read; "+
				"point that file type at a model that can", ErrUnsupportedFile, f.FileName, f.MediaType)
		}
	}
	// The words go last, after what they are about, which is the order this
	// vendor recommends for a question asked of a document.
	if m.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropic.NewTextBlock(""))
	}
	return blocks, nil
}

// Media is what this vendor can be asked to do with something that is not
// words: a file yes, a recording no. It has no transcription endpoint at all,
// so a microphone pointed at it could never work, and saying so where somebody
// chooses the model beats a failure on the first recording.
func (a *Anthropic) Media() Media {
	return Media{ReadsFiles: true, Transcribes: false}
}

// Transcribe is not something this vendor does.
func (a *Anthropic) Transcribe(context.Context, string, string, []byte) (string, error) {
	return "", ErrTranscriptionUnsupported
}
