package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAICompatible serves every vendor that speaks the OpenAI
// chat-completions format: OpenAI, DeepSeek, Z.ai, Mistral, Azure OpenAI,
// and any self-hosted server (vLLM, llama.cpp). The differences live
// in the Dialect, so this code never asks which vendor it is talking to.
type OpenAICompatible struct {
	client  openai.Client
	dialect Dialect
}

// NewOpenAICompatible builds the adapter. baseURL overrides the dialect
// default, which is how a self-hosted server or an Azure resource is named.
//
// hc is the client to call with, and nil means the ordinary one. It is set only
// for a machine we run ourselves, where the connection is mutually
// authenticated and the certificate at each end comes from our own authority
// (KB/35): every hosted vendor is reached the way anything reaches a public
// endpoint.
func NewOpenAICompatible(dialect Dialect, apiKey, baseURL string, hc *http.Client) (*OpenAICompatible, error) {
	endpoint := baseURL
	if endpoint == "" {
		endpoint = dialect.DefaultBaseURL
	}

	// A keep-alive must not end the stream. See keepalive.go: a server holding a
	// slow answer open sends SSE comments, and the SDK turns each one into an
	// empty chunk that fails to parse.
	opts := []option.RequestOption{keepAliveTolerance}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	switch {
	case dialect.IsAzure:
		if endpoint == "" {
			return nil, errors.New("provider: azure requires an endpoint")
		}
		apiVersion := dialect.AzureAPIVersion
		if apiVersion == "" {
			apiVersion = defaultAzureAPIVersion
		}
		// Azure authenticates with its own header and pins every call to an
		// api-version; the SDK's helper applies both.
		opts = append(opts, azure.WithEndpoint(endpoint, apiVersion), azure.WithAPIKey(apiKey))
	default:
		opts = append(opts, option.WithAPIKey(apiKey))
		if endpoint != "" {
			opts = append(opts, option.WithBaseURL(endpoint))
		}
	}

	return &OpenAICompatible{client: openai.NewClient(opts...), dialect: dialect}, nil
}

func (o *OpenAICompatible) Name() string { return o.dialect.Key }

func (o *OpenAICompatible) Capabilities(_ context.Context, _ string) (Capabilities, error) {
	// Declared per model in the registry: no vendor in this family exposes a
	// capability endpoint, and inferring from a model name breaks the day a
	// new model ships.
	return Capabilities{
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsReasoning: o.dialect.Reasoning != ReasoningNone,
	}, nil
}

// ListModels asks the vendor what it offers, over the /models endpoint every
// member of this family serves. The vendors here publish an id and no name, so
// the id is all a person gets, and it is the thing they must type anyway.
//
// Azure is the exception and says so rather than guessing: what is named there
// is a deployment somebody created in their own resource, not a model Azure
// publishes, so a catalog can neither confirm nor deny it.
func (o *OpenAICompatible) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if o.dialect.IsAzure {
		return nil, ErrListingUnsupported
	}

	models := []ModelInfo{}
	pager := o.client.Models.ListAutoPaging(ctx)
	for pager.Next() {
		models = append(models, ModelInfo{ID: pager.Current().ID})
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("%s list models: %w", o.dialect.Key, err)
	}
	return models, nil
}

func (o *OpenAICompatible) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	params, opts, err := o.buildParams(req)
	if err != nil {
		return nil, err
	}
	completion, err := o.client.Chat.Completions.New(ctx, params, opts...)
	if err != nil {
		return nil, fmt.Errorf("%s generate: %w", o.dialect.Key, err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("%s generate: no choices returned", o.dialect.Key)
	}

	choice := completion.Choices[0].Message
	out := Message{Role: RoleAssistant, Content: choice.Content}
	// Reasoning is a non-standard field, so it is read from the raw payload
	// rather than from a typed one that only some vendors populate.
	out.Reasoning = extraString(choice.JSON.ExtraFields, "reasoning_content")

	for _, call := range choice.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: rawArgs(call.Function.Arguments),
		})
	}
	return &GenerateResponse{
		Message: out,
		Usage: Usage{
			InputTokens:  completion.Usage.PromptTokens,
			OutputTokens: completion.Usage.CompletionTokens,
		},
	}, nil
}

func (o *OpenAICompatible) Stream(ctx context.Context, req GenerateRequest) (<-chan StreamEvent, error) {
	params, opts, err := o.buildParams(req)
	if err != nil {
		// Before the channel exists, so an unreadable file is a refusal the
		// caller gets back rather than an error event on a stream that then has
		// to be closed. Same shape as the Anthropic adapter.
		return nil, err
	}
	// Ask for usage on the final chunk; vendors that do not support the
	// option ignore it, and cost tracking then falls back to zero rather
	// than failing the turn.
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: openai.Bool(true),
	}

	events := make(chan StreamEvent, streamBuffer)
	go func() {
		defer close(events)

		stream := o.client.Chat.Completions.NewStreaming(ctx, params, opts...)
		// Tool arguments stream as fragments across chunks, keyed by index,
		// and are only emitted once the call is complete.
		pending := map[int64]*ToolCall{}
		// A vendor that talks tool calls emits them as markup in the content;
		// the scanner salvages those into real calls and keeps the raw markup
		// out of the chat. Nil for vendors that never do this.
		var markup *markupScanner
		if o.dialect.MarkupToolCalls {
			markup = &markupScanner{}
		}
		var usage Usage

		for stream.Next() {
			chunk := stream.Current()
			if chunk.Usage.TotalTokens > 0 {
				usage.InputTokens = chunk.Usage.PromptTokens
				usage.OutputTokens = chunk.Usage.CompletionTokens
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				if markup != nil {
					prose, calls := markup.push(delta.Content)
					if prose != "" {
						emit(ctx, events, StreamEvent{Kind: EventContentDelta, ContentDelta: prose})
					}
					for _, call := range calls {
						emit(ctx, events, StreamEvent{Kind: EventToolCall, ToolCall: call})
					}
				} else {
					emit(ctx, events, StreamEvent{Kind: EventContentDelta, ContentDelta: delta.Content})
				}
			}
			if o.dialect.Reasoning == ReasoningContentField {
				if reasoning := extraString(delta.JSON.ExtraFields, "reasoning_content"); reasoning != "" {
					emit(ctx, events, StreamEvent{Kind: EventReasoningDelta, ReasoningDelta: reasoning})
				}
			}

			for _, call := range delta.ToolCalls {
				current, ok := pending[call.Index]
				if !ok {
					current = &ToolCall{}
					pending[call.Index] = current
				}
				// Identity arrives on the first fragment, arguments across
				// the rest.
				if call.ID != "" {
					current.ID = call.ID
				}
				if call.Function.Name != "" {
					current.Name = call.Function.Name
				}
				current.Args = append(current.Args, call.Function.Arguments...)
			}

			// A finish reason means the assistant turn is complete, so any
			// assembled tool calls are now whole.
			if chunk.Choices[0].FinishReason != "" {
				for _, index := range sortedKeys(pending) {
					call := pending[index]
					call.Args = rawArgs(string(call.Args))
					emit(ctx, events, StreamEvent{Kind: EventToolCall, ToolCall: call})
				}
				pending = map[int64]*ToolCall{}
			}
		}

		if err := stream.Err(); err != nil {
			if !errors.Is(err, context.Canceled) {
				emit(ctx, events, StreamEvent{
					Kind: EventError, Err: fmt.Errorf("%s stream: %w", o.dialect.Key, err),
				})
			}
			return
		}
		// A block held open at the end is a truncated call; its body is prose,
		// not a call, so it is surfaced rather than dropped.
		if markup != nil {
			if tail := markup.flush(); tail != "" {
				emit(ctx, events, StreamEvent{Kind: EventContentDelta, ContentDelta: tail})
			}
		}
		emit(ctx, events, StreamEvent{Kind: EventUsage, Usage: &usage})
		emit(ctx, events, StreamEvent{Kind: EventDone})
	}()
	return events, nil
}

func (o *OpenAICompatible) Embed(ctx context.Context, modelKey string, inputs []string) ([][]float32, error) {
	resp, err := o.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: modelKey,
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
	})
	if err != nil {
		return nil, fmt.Errorf("%s embed: %w", o.dialect.Key, err)
	}
	out := make([][]float32, 0, len(resp.Data))
	for _, item := range resp.Data {
		vector := make([]float32, len(item.Embedding))
		for i, v := range item.Embedding {
			vector[i] = float32(v)
		}
		out = append(out, vector)
	}
	return out, nil
}

func (o *OpenAICompatible) buildParams(req GenerateRequest) (openai.ChatCompletionNewParams, []option.RequestOption, error) {
	params := openai.ChatCompletionNewParams{
		Model: req.Model,
	}
	if req.MaxTokens > 0 {
		// OpenAI's own API deprecated max_tokens for max_completion_tokens, and
		// its reasoning models reject the old name; every other vendor here still
		// speaks max_tokens. Which field to use is a dialect fact, not a per-model
		// branch (dialect.go).
		if o.dialect.MaxCompletionTokens {
			params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
		} else {
			params.MaxTokens = openai.Int(int64(req.MaxTokens))
		}
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(*req.Temperature)
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			params.Messages = append(params.Messages, openai.SystemMessage(m.Content))
		case RoleUser:
			msg, err := userMessage(m)
			if err != nil {
				return params, nil, err
			}
			params.Messages = append(params.Messages, msg)
		case RoleTool:
			params.Messages = append(params.Messages, openai.ToolMessage(m.Content, m.ToolCallID))
		case RoleAssistant:
			params.Messages = append(params.Messages, o.assistantMessage(m))
		}
	}

	for _, t := range req.Tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			// A tool whose schema will not parse cannot be offered; skipping
			// it is safer than sending something the vendor rejects, which
			// would fail the whole turn.
			continue
		}
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name,
					Description: openai.String(t.Description),
					Parameters:  schema,
				},
			},
		})
	}

	var opts []option.RequestOption
	// The toggle is sent on every call, in both states. Omitting it when
	// reasoning is off would leave thinking on for the vendors that default
	// to it, and quietly bill an agent for reasoning nobody asked for.
	if toggle := o.dialect.ThinkingToggle; toggle != nil {
		value := toggle.Disabled
		if req.Reasoning {
			value = toggle.Enabled
		}
		opts = append(opts, option.WithJSONSet(toggle.Field, value))
	}
	return params, opts, nil
}

// assistantMessage rebuilds a past assistant turn, including the tool calls
// it made: the pairing between a tool call and its result is what every
// vendor validates.
func (o *OpenAICompatible) assistantMessage(m Message) openai.ChatCompletionMessageParamUnion {
	msg := openai.ChatCompletionAssistantMessageParam{}
	if m.Content != "" {
		msg.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
			OfString: param.NewOpt(m.Content),
		}
	}
	for _, call := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: call.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      call.Name,
					Arguments: string(call.Args),
				},
			},
		})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &msg}
}

// rawArgs guarantees the tool loop always receives valid JSON: a vendor that
// sends no arguments, or a fragment that never completed, must not blow up
// the unmarshal on the other side.
func rawArgs(args string) json.RawMessage {
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// extraString reads a non-standard string field from a response. Vendors add
// these freely (DeepSeek's reasoning_content is the reason this exists), and
// the SDK keeps them as raw JSON rather than dropping them.
func extraString(extra map[string]respjson.Field, key string) string {
	field, ok := extra[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal([]byte(field.Raw()), &value); err != nil {
		return ""
	}
	return value
}

// sortedKeys keeps tool calls in the order the model produced them, which is
// the order their results must be returned in.
func sortedKeys(m map[int64]*ToolCall) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// userMessage is a user message as this dialect takes it: plain text when
// nothing is attached, and a list of parts when something is.
//
// The plain-text form is kept for the common case on purpose. Several
// OpenAI-compatible servers accept only a string here, and sending everyone a
// one-element parts array to be uniform would break them for no gain.
//
// # A file goes down the channel that matches what it IS
//
// Three kinds, and the same three the Anthropic adapter already distinguishes,
// for the same reasons.
//
// An image goes as an image. A PDF goes as a file, and a PDF is the ONLY thing
// that may: this vendor's file part is a document channel that accepts one
// type, and it says so by rejecting everything else with a complaint about the
// data rather than about the type.
//
// **Text goes as text**, which is the case this used to get wrong. Sending a
// `.txt` as base64 in the file part meant the simplest file a person can attach
// was the one thing that could not be read: the vendor refused it outright
// ("unsupported MIME type 'text/plain'"), the extraction failed, and the person
// was told their file could not be read while their configuration was correct
// all along. There is also nothing to decode. A text file's bytes ARE its
// content, so a model reads it better as words in the message than as an
// encoded blob it has to unpack, which is what the Anthropic adapter has always
// said (`userBlocks`) and this one never did.
//
// Anything else is an ERROR here rather than bytes sent hopefully. Which model
// reads which file is the administrator's decision, and a decision that cannot
// work should fail where it was made: a refusal naming the file and its type is
// something somebody can act on, where a vendor's complaint about
// `content[0].file.file_data` is not.
func userMessage(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	if len(m.Files) == 0 {
		return openai.UserMessage(m.Content), nil
	}
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Files)+1)
	for _, f := range m.Files {
		switch {
		case strings.HasPrefix(f.MediaType, "image/"):
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: "data:" + f.MediaType + ";base64," + base64.StdEncoding.EncodeToString(f.Data),
			}))
		case f.MediaType == "application/pdf":
			parts = append(parts, openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
				Filename: openai.String(f.FileName),
				FileData: openai.String("data:" + f.MediaType + ";base64," +
					base64.StdEncoding.EncodeToString(f.Data)),
			}))
		case isText(f.MediaType):
			// Named, because the model is about to be shown two things at once
			// and has no other way to tell where the file ends and the question
			// begins.
			parts = append(parts, openai.TextContentPart(
				fmt.Sprintf("%s:\n\n%s", f.FileName, string(f.Data))))
		default:
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf(
				"%w: this model was given %s (%s), which it cannot read; "+
					"point that file type at a model that can",
				ErrUnsupportedFile, f.FileName, f.MediaType)
		}
	}
	if m.Content != "" {
		parts = append(parts, openai.TextContentPart(m.Content))
	}
	return openai.UserMessage(parts), nil
}

// isText reports whether a file's bytes are already the thing a model should
// read.
//
// Deliberately the same set the Anthropic adapter treats as plain text, so a
// file rule pointed at one vendor and then at another behaves the same. The
// `+json` and `+xml` suffixes are here because that is how the registered types
// for a great many formats are spelled, and a person attaching one has attached
// text whatever the prefix says.
func isText(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/yaml",
		"application/x-yaml", "application/toml", "application/csv":
		return true
	}
	return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
}

// Media is what this dialect can be asked to do with something that is not
// words.
//
// Transcription is the OpenAI audio endpoint, which is also what a local
// runtime speaking this dialect exposes, so a self-hosted whisper is reached
// through exactly this path and needs nothing of its own. Whether the vendor on
// the other end actually implements it is between the administrator and their
// deployment; what is said here is that this ADAPTER knows how to ask.
func (o *OpenAICompatible) Media() Media {
	return Media{ReadsFiles: true, Transcribes: true}
}

// Transcribe turns a recording into words.
//
// It goes through the SDK's own audio endpoint rather than a hand-built
// multipart request, so the wire format stays the SDK's problem, which is the
// same division this whole file keeps.
//
// The file NAME matters and is not decoration: this endpoint reads the
// extension to know how to decode the bytes, so a recording has to arrive
// called what it is.
func (o *OpenAICompatible) Transcribe(ctx context.Context, model, fileName string, audio []byte) (string, error) {
	res, err := o.client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(bytes.NewReader(audio), fileName, ""),
		Model: model,
	})
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		return "", fmt.Errorf("the recording produced no words")
	}
	return text, nil
}
