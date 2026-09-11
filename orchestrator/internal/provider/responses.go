package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// The OpenAI vendor's own current API.
//
// # Why this is a second file and not a branch in the first one
//
// `openai.go` is the OpenAI-COMPATIBLE wire: the chat-completions shape that
// DeepSeek, Z.ai, Mistral, self-hosted servers and our own machines all copied
// and will go on speaking. This is a different wire, and the difference is the
// same KIND of difference that already earned Anthropic its own file:
//
//   - a flat `input` array of typed ITEMS instead of a `messages` array
//   - named stream events instead of chunk deltas
//   - a tool result that goes back as its own top-level item keyed by `call_id`,
//     with no `role:"tool"` and no `tool_call_id` anywhere
//
// So the count of adapters here is the count of wire protocols that actually
// exist, which is a floor set by the world rather than a choice we made. What
// stays true is the rule that matters: which wire a vendor speaks is one field
// on its dialect (`dialect.go`), decided once per VENDOR. No model name appears
// in this file or in the choice to use it, and none may: there are more models
// every month and a table of them is a table that is always wrong.
//
// # What forced it
//
// OpenAI does not support function tools together with reasoning on the chat
// wire for its current models, and every agent here has tools. So a first-party
// reasoning model refused every single call:
//
//	Function tools with reasoning_effort are not supported for <model> in
//	/v1/chat/completions. To use function tools, use /v1/responses.
//
// Staying on the old wire would have meant a workaround per model as that line
// turns over, which is the debt this exists to avoid rather than create.
//
// It also closes a gap nobody had noticed: the `openai` dialect asks for
// reasoning through `reasoning_content` (`openai.go`), which is DeepSeek's
// field and one OpenAI has never returned. Every OpenAI model run through SAG
// has had its thinking silently discarded. On this wire it arrives as typed
// events, so it is shown for the first time.
type OpenAIResponses struct {
	// The chat adapter, for the methods that are not about either chat wire.
	//
	// `ListModels`, `Media`, `Transcribe`, `Embed`, `Capabilities` and `Name`
	// are separate endpoints on the same API and are identical here, so they
	// are inherited rather than copied. `Generate` and `Stream` are the two
	// that differ and both are defined below, which shadows the embedded pair.
	//
	// The hazard of embedding is a method added to `Provider` later being
	// served by the chat implementation without anybody noticing. What catches
	// that is `TestTheResponsesAdapterNeverCallsTheChatWire`, which fails if
	// this adapter ever sends anything to /chat/completions.
	*OpenAICompatible
}

// NewOpenAIResponses builds the adapter for a vendor whose dialect says it
// speaks this wire.
func NewOpenAIResponses(dialect Dialect, apiKey, baseURL string, hc *http.Client) (*OpenAIResponses, error) {
	chat, err := NewOpenAICompatible(dialect, apiKey, baseURL, hc)
	if err != nil {
		return nil, err
	}
	return &OpenAIResponses{OpenAICompatible: chat}, nil
}

func (o *OpenAIResponses) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	params, err := o.buildResponse(req)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Responses.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("%s generate: %w", o.dialect.Key, err)
	}
	// The floor the chat adapter has ("no choices returned"), kept. Without it
	// an empty answer becomes a blank title and a skipped memory rewrite, with
	// nothing anywhere saying the model returned nothing.
	if len(resp.Output) == 0 {
		return nil, fmt.Errorf("%s generate: the model returned nothing", o.dialect.Key)
	}

	out := Message{Role: RoleAssistant}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				out.Content += part.Text
			}
		case "reasoning":
			for _, part := range item.Content {
				out.Reasoning += part.Text
			}
			for _, s := range item.Summary {
				out.Reasoning += s.Text
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   item.CallID,
				Name: item.Name,
				Args: rawArgs(item.Arguments.OfString),
			})
		}
	}
	return &GenerateResponse{
		Message: out,
		Usage: Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

func (o *OpenAIResponses) Stream(ctx context.Context, req GenerateRequest) (<-chan StreamEvent, error) {
	// A model already known to refuse it never gets asked again (unsupported.go).
	if req.Reasoning && refusesReasoning(req.Model) {
		req = withoutReasoning(req)
	}
	params, err := o.buildResponse(req)
	if err != nil {
		// Before the channel exists, so an unreadable file is a refusal the
		// caller gets back rather than an error event on a stream nobody asked
		// for. Same shape as both other adapters.
		return nil, err
	}

	events := make(chan StreamEvent, streamBuffer)
	go func() {
		defer close(events)
		stream := o.client.Responses.NewStreaming(ctx, params)
		spoke, err := o.pump(ctx, stream, events)

		// The one recoverable failure: this model has no reasoning and said so.
		// Only before anything was said, because a retry that replays a stream
		// somebody has already read would say it twice, and only once, because
		// the second attempt asks for nothing unusual and a second refusal is a
		// real error the caller must see.
		if !spoke && req.Reasoning && isReasoningRefusal(err) {
			rememberReasoningRefusal(req.Model)
			plain, buildErr := o.buildResponse(withoutReasoning(req))
			if buildErr != nil {
				emit(ctx, events, StreamEvent{Kind: EventError, Err: buildErr})
				return
			}
			_, err = o.pump(ctx, o.client.Responses.NewStreaming(ctx, plain), events)
		}
		if err != nil {
			emit(ctx, events, StreamEvent{Kind: EventError, Err: err})
		}
	}()
	return events, nil
}

// pump turns the vendor's named events into ours.
//
// Every send goes through `emit` so a caller that has stopped reading cannot
// wedge this goroutine.
//
// It REPORTS its failure rather than emitting it, and says whether the model had
// said anything before it. Both are for the caller's benefit: a request refused
// over the reasoning parameter can be sent again without it, and that is only
// safe while nothing has reached the reader, because a retry replays from the
// beginning and a reader who has already seen half an answer would see it twice.
// Reasoning does not count as having spoken, deliberately, for the same reason
// it does not in the agent loop: reasoning is not the answer.
func (o *OpenAIResponses) pump(
	ctx context.Context,
	stream *ssestream.Stream[responses.ResponseStreamEventUnion],
	events chan<- StreamEvent,
) (spoke bool, failed error) {
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			spoke = true
			emit(ctx, events, StreamEvent{Kind: EventContentDelta, ContentDelta: event.Delta})

		// Both spellings become the one event, which is the entire job of this
		// layer: a caller never learns that this vendor has two ways of saying
		// the model thought about something.
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			emit(ctx, events, StreamEvent{Kind: EventReasoningDelta, ReasoningDelta: event.Delta})

		// A refusal is something the person has to see. Dropped, it reads as a
		// turn that produced nothing, which is the failure the clients were
		// fixed for (KB/12, silence is not a refusal).
		case "response.refusal.done":
			if event.Refusal != "" {
				spoke = true
				emit(ctx, events, StreamEvent{Kind: EventContentDelta, ContentDelta: event.Refusal})
			}

		// A finished item, which is where a tool call arrives WHOLE. There is
		// no accumulation to do and therefore no half-built call to lose if the
		// stream ends early, which is a gap the chat wire has.
		case "response.output_item.done":
			if event.Item.Type == "function_call" {
				spoke = true
				emit(ctx, events, StreamEvent{Kind: EventToolCall, ToolCall: &ToolCall{
					// The correlation id, never the item id. The next turn's
					// tool result is keyed by this, and sending the other one
					// back is a 400 that says the call does not exist.
					ID:   event.Item.CallID,
					Name: event.Item.Name,
					Args: rawArgs(event.Item.Arguments.OfString),
				}})
			}

		case "response.completed":
			emit(ctx, events, StreamEvent{Kind: EventUsage, Usage: &Usage{
				InputTokens:  event.Response.Usage.InputTokens,
				OutputTokens: event.Response.Usage.OutputTokens,
			}})
			emit(ctx, events, StreamEvent{Kind: EventDone})
			return spoke, nil

		// Ran out of budget. NOT a clean end: on this wire the cap covers
		// thinking as well as writing, so a reasoning model can spend all of it
		// before the first character and finish with nothing to show. Reported
		// as done, that is a successful, silent, empty turn, and the person is
		// left to conclude the model had nothing to say.
		case "response.incomplete":
			reason := "the model stopped before it finished"
			if d := event.Response.IncompleteDetails.Reason; d != "" {
				reason = d
			}
			return spoke, fmt.Errorf("%s stream: %s", o.dialect.Key, reason)

		case "response.failed":
			return spoke, fmt.Errorf("%s stream: %s", o.dialect.Key, event.Response.Error.Message)

		// Typed bare, not "response.error". A dispatcher that filtered on the
		// prefix would drop exactly the event that must never be dropped.
		case "error":
			return spoke, fmt.Errorf("%s stream: %s", o.dialect.Key, event.Message)
		}
	}

	if err := stream.Err(); err != nil {
		if ctx.Err() != nil {
			return spoke, nil // the caller went away; that is not a fault to report
		}
		return spoke, fmt.Errorf("%s stream: %w", o.dialect.Key, err)
	}
	return spoke, nil
}

// buildResponse turns our request into this wire's shape.
func (o *OpenAIResponses) buildResponse(req GenerateRequest) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model: req.Model,
		// Nothing is kept on the vendor's side. Our transcript is the truth
		// (KB/16): a turn is rebuilt from rows every time, park and resume
		// depends on that, and server-side chaining would put a customer's
		// conversation into somebody else's retention window for no gain.
		Store: openai.Bool(false),
	}
	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(*req.Temperature)
	}
	// Only when it was asked for, which is what the Anthropic adapter does and
	// is the only thing that works here. Sending the field in both states is
	// the DeepSeek rule and it does not transfer: DeepSeek defaults thinking
	// ON so "off" has to be said, while these models reject the object outright
	// unless they are reasoning models, and the ones that accept it disagree
	// about which values are legal. Omission is the one behaviour every model
	// on this wire understands.
	if req.Reasoning {
		params.Reasoning = shared.ReasoningParam{
			// Chosen, not fixed. The dialect declares the key and its default
			// (medium); an agent, a model or a vendor may say otherwise, and
			// this is already resolved by the time it arrives.
			Effort: shared.ReasoningEffort(reasoningEffortSetting.Value(req.Settings)),
			// Asking for the summary is what makes thinking VISIBLE. Without
			// it the model reasons, the tokens are billed, and nothing comes
			// back: a live run against a current model returned a tool call,
			// spent 31 output tokens and zero characters of thinking. That is
			// the state this vendor has silently been in all along, and the
			// reason it went unnoticed is that the chat wire could not show
			// thinking either.
			Summary: shared.ReasoningSummaryAuto,
		}
	}

	var input []responses.ResponseInputItemUnionParam
	for _, m := range req.Messages {
		items, err := responseItems(m)
		if err != nil {
			return params, err
		}
		input = append(input, items...)
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}

	for _, t := range req.Tools {
		schema, err := toolSchema(t)
		if err != nil {
			// A tool whose schema will not parse cannot be offered. Skipped
			// rather than sent, because a malformed one fails the whole turn.
			continue
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  schema,
				// Off, matching the chat wire. Strict mode rejects schemas that
				// are perfectly valid JSON Schema, and a tool that will not
				// load is worse than one the model occasionally miscalls.
				Strict: openai.Bool(false),
			},
		})
	}
	return params, nil
}

// responseItems turns one of our messages into the items this wire wants.
//
// One message can become SEVERAL items, which is the shape that differs most
// from chat completions: an assistant turn that both said something and called
// two tools is three items, and a tool result is a top-level item rather than a
// message with a role.
func responseItems(m Message) ([]responses.ResponseInputItemUnionParam, error) {
	switch m.Role {
	case RoleSystem:
		// In place, where it was, rather than hoisted into `instructions`. The
		// trimmer's contract is that a system message keeps its position
		// (`trim.go`), and a second way of expressing one is a second thing to
		// keep in step.
		return []responses.ResponseInputItemUnionParam{
			responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleSystem),
		}, nil

	case RoleUser:
		if len(m.Files) == 0 {
			return []responses.ResponseInputItemUnionParam{
				responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleUser),
			}, nil
		}
		content, err := responseContent(m)
		if err != nil {
			return nil, err
		}
		return []responses.ResponseInputItemUnionParam{
			responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser),
		}, nil

	case RoleTool:
		return []responses.ResponseInputItemUnionParam{
			responses.ResponseInputItemParamOfFunctionCallOutput(m.ToolCallID, m.Content),
		}, nil

	case RoleAssistant:
		var items []responses.ResponseInputItemUnionParam
		if m.Content != "" {
			items = append(items, responses.ResponseInputItemParamOfMessage(
				m.Content, responses.EasyInputMessageRoleAssistant))
		}
		for _, call := range m.ToolCalls {
			items = append(items, responses.ResponseInputItemParamOfFunctionCall(
				string(call.Args), call.ID, call.Name))
		}
		// An assistant turn that said nothing and called nothing is left out
		// entirely. An empty item is a thing some vendors reject and none needs.
		return items, nil
	}
	return nil, nil
}

// responseContent is a user message that carries files.
//
// The same three-way split both other adapters make, and for the same reasons:
// an image is an image, a PDF is a file, and TEXT IS TEXT. A text file's bytes
// are already its content, so decoding it into base64 for the model to unpack
// helps nobody, and on the chat wire doing that made a plain .txt the one file
// that could not be read at all.
func responseContent(m Message) (responses.ResponseInputMessageContentListParam, error) {
	content := make(responses.ResponseInputMessageContentListParam, 0, len(m.Files)+1)
	for _, f := range m.Files {
		encoded := base64.StdEncoding.EncodeToString(f.Data)
		switch {
		case strings.HasPrefix(f.MediaType, "image/"):
			content = append(content, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: openai.String("data:" + f.MediaType + ";base64," + encoded),
					Detail:   responses.ResponseInputImageDetailAuto,
				},
			})
		case f.MediaType == "application/pdf":
			content = append(content, responses.ResponseInputContentUnionParam{
				OfInputFile: &responses.ResponseInputFileParam{
					Filename: openai.String(f.FileName),
					FileData: openai.String("data:" + f.MediaType + ";base64," + encoded),
				},
			})
		case isText(f.MediaType):
			content = append(content, responses.ResponseInputContentParamOfInputText(
				fmt.Sprintf("%s:\n\n%s", f.FileName, string(f.Data))))
		default:
			return nil, fmt.Errorf(
				"%w: this model was given %s (%s), which it cannot read; "+
					"point that file type at a model that can",
				ErrUnsupportedFile, f.FileName, f.MediaType)
		}
	}
	if m.Content != "" {
		content = append(content, responses.ResponseInputContentParamOfInputText(m.Content))
	}
	return content, nil
}

// toolSchema is a tool's parameter schema as this SDK wants it.
func toolSchema(t ToolDef) (map[string]any, error) {
	var schema map[string]any
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return nil, fmt.Errorf("tool %s has an unreadable schema: %w", t.Name, err)
	}
	return schema, nil
}
