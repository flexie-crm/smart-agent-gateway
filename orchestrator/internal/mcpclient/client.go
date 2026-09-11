// Package mcpclient is SAG as the MCP client: the gateway connecting OUT to
// third-party tool servers, the inverse of the MCP surface we expose. It
// speaks the wire protocol and nothing else; what a remote tool becomes in
// the registry, and who may call it, is the app layer's business.
package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RemoteTool is one tool as the remote states it, plus the hash that makes a
// redefinition a visible event.
type RemoteTool struct {
	Name        string
	Title       string
	Description string
	InputSchema json.RawMessage
	// DefinitionHash covers the name, the description, and the input schema:
	// everything the model reads. A remote that rewrites any of it produces a
	// different hash, and the projection treats that as a reset of trust.
	DefinitionHash string
}

// Session is one live conversation with a remote server. Callers close it.
type Session struct {
	cs *mcp.ClientSession
}

// dialTimeout bounds the whole connect-and-initialize exchange: a remote that
// does not answer must fail a sync, not hang it.
const dialTimeout = 30 * time.Second

// Dial connects and initializes against a remote MCP server over streamable
// HTTP. The bearer token, when given, authenticates every request (a static
// API key and a fresh OAuth access token travel the same way).
func Dial(ctx context.Context, endpoint, bearer string) (*Session, error) {
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: authedHTTPClient(bearer),
		// Sessions here are one errand long: sync or call, then close. The
		// standalone notification stream belongs to a resident connection
		// manager, which is a later phase.
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "sag",
		Title:   "SAG",
		Version: "1.0",
	}, nil)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	cs, err := client.Connect(dialCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to mcp server: %w", err)
	}
	return &Session{cs: cs}, nil
}

func (s *Session) Close() error { return s.cs.Close() }

// ListTools reads the remote's whole tool list, pagination included.
func (s *Session) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var tools []RemoteTool
	for t, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list remote tools: %w", err)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("remote tool %s: unusable schema: %w", t.Name, err)
		}
		tools = append(tools, RemoteTool{
			Name:           t.Name,
			Title:          t.Title,
			Description:    t.Description,
			InputSchema:    schema,
			DefinitionHash: definitionHash(t.Name, t.Description, schema),
		})
	}
	return tools, nil
}

// CallTool invokes one remote tool and flattens the answer to something a
// model can read. A remote error is an answer, not a Go error: the loop's
// contract is that tool failures reach the model as content.
func (s *Session) CallTool(ctx context.Context, name string, args json.RawMessage) (content string, isError bool, err error) {
	params := &mcp.CallToolParams{Name: name}
	if len(args) > 0 {
		params.Arguments = args
	}
	result, err := s.cs.CallTool(ctx, params)
	if err != nil {
		return "", false, fmt.Errorf("call remote tool %s: %w", name, err)
	}
	return flatten(result), result.IsError, nil
}

// flatten turns a tool result into one text block. Structured content wins
// when present; otherwise the text parts are joined. Non-text parts are
// named rather than dropped silently.
func flatten(result *mcp.CallToolResult) string {
	if result.StructuredContent != nil {
		if raw, err := json.Marshal(result.StructuredContent); err == nil {
			return string(raw)
		}
	}
	var parts []string
	for _, c := range result.Content {
		switch content := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, content.Text)
		case *mcp.ImageContent:
			parts = append(parts, "[the service answered with an image]")
		case *mcp.AudioContent:
			parts = append(parts, "[the service answered with audio]")
		default:
			parts = append(parts, "[the service answered with content this channel cannot carry]")
		}
	}
	return strings.Join(parts, "\n")
}

// definitionHash covers everything the model reads about a tool.
func definitionHash(name, description string, schema json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(description))
	h.Write([]byte{0})
	h.Write(schema)
	return hex.EncodeToString(h.Sum(nil))
}

// authedHTTPClient carries the bearer token on every request the transport
// makes, initialize and SSE streams included.
func authedHTTPClient(bearer string) *http.Client {
	client := &http.Client{Timeout: 5 * time.Minute}
	if bearer != "" {
		client.Transport = &bearerTransport{token: bearer, next: http.DefaultTransport}
	}
	return client
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before writing: a RoundTripper must not mutate the caller's request.
	authed := req.Clone(req.Context())
	authed.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(authed)
}
