// sag-payload prints what one turn actually sends to the vendor.
//
// Not a description of it and not a reconstruction: it resolves the profile
// from THIS installation's database exactly as a chat turn does (the layered
// configuration, the agent, its tools, the assembled prompt), builds the same
// request the loop builds, and hands it to the same adapter. The adapter posts
// it to a listener here instead of to the vendor, so what is printed is the
// bytes that would have gone out.
//
// It sends nothing anywhere and writes nothing down: no conversation is
// created, no turn is recorded, and the vendor is never called.
//
//	go run ./cmd/sag-payload -workspace 1 -user 17 -device <id> -prompt "hello"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/rs/zerolog"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
	"flexie.io/sag/internal/link"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store/sqlstore"
	"flexie.io/sag/internal/tool"
	"flexie.io/sag/internal/tools/machine"
)

// presentMachine stands in for the person's chat application being connected.
// Nothing is ever run through it: what it answers is which tools would be
// OFFERED, which is the only part of a link this dump needs.
type presentMachine struct {
	versions map[string]int
	device   string
}

func (m presentMachine) Call(
	context.Context, int64, int64, string, string, json.RawMessage, string,
) (link.Result, error) {
	return link.Result{}, fmt.Errorf("this dump never runs a tool")
}

func (m presentMachine) Runs(_, _ int64, deviceID string) map[string]int {
	if deviceID != m.device {
		return nil
	}
	return m.versions
}

func main() {
	workspace := flag.Int64("workspace", 1, "which workspace")
	user := flag.Int64("user", 0, "which person (their user id)")
	device := flag.String("device", "", "which installation of the chat application, for the tools that run on it")
	modelID := flag.Int64("model", 0, "a model to force; 0 uses whatever the configuration resolves to")
	prompt := flag.String("prompt", "hello", "the message the person would have typed")
	linked := flag.Bool("linked", true, "count the chat application as connected, so the tools that run on the person's own computer are in the payload as they are on a real turn")
	flag.Parse()

	if err := run(*workspace, *user, *device, *modelID, *prompt, *linked); err != nil {
		fmt.Fprintln(os.Stderr, "sag-payload:", err)
		os.Exit(1)
	}
}

func run(workspaceID, userID int64, deviceID string, modelID int64, prompt string, linked bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("read the configuration: %w", err)
	}
	log := zerolog.New(io.Discard)
	ctx0 := context.Background()
	st, err := sqlstore.Open(ctx0, cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer st.Close()

	a, err := app.New(cfg, log, st)
	if err != nil {
		return fmt.Errorf("build the application: %w", err)
	}

	// A link is a live socket held by the RUNNING gateway, and this is another
	// process entirely. Left alone, every tool that runs on the person's own
	// computer is filtered out of this dump and the payload printed is a
	// smaller one than the product actually sends. So their application's
	// presence is stood in for, declaring exactly what this build speaks.
	if linked && deviceID != "" {
		a.Machines = presentMachine{versions: machine.Versions(), device: deviceID}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Exactly what a chat turn asks for.
	req := app.ProfileRequest{
		WorkspaceID:      workspaceID,
		UserID:           userID,
		DeviceID:         deviceID,
		Channel:          model.ChannelChat,
		PreferredModelID: modelID,
	}
	profile, loadout, err := a.Resolve(ctx, req)
	if err != nil {
		return fmt.Errorf("resolve the profile: %w", err)
	}

	resolved, err := a.Gateway.Resolve(ctx, workspaceID, profile.ModelID)
	if err != nil {
		return fmt.Errorf("resolve the model: %w", err)
	}

	// The same two messages a first turn carries, and the same tool list.
	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: profile.SystemPrompt},
		{Role: provider.RoleUser, Content: prompt},
	}
	tools := make([]provider.ToolDef, 0, len(loadout.Schemas))
	for _, s := range loadout.Schemas {
		tools = append(tools, provider.ToolDef{
			Name: s.Name, Description: s.Description, InputSchema: s.InputSchema,
		})
	}

	outgoing := resolved.Prepare(provider.GenerateRequest{
		Messages:  messages,
		Tools:     tools,
		Reasoning: profile.Reasoning,
		Settings:  profile.Settings,
	})

	summary(profile, loadout, resolved, outgoing)
	// The instruction as TEXT, before the payload shows it as an escaped string.
	// It is the longest single thing in the request and the one a person most
	// wants to read, and JSON is not a way to read prose.
	fmt.Println("═══ THE SYSTEM INSTRUCTION, AS THE MODEL READS IT ═══")
	fmt.Println(profile.SystemPrompt)
	fmt.Println()

	return theBytes(ctx, resolved, outgoing)
}

// summary is what a person wants to read before the payload itself: which
// model, how it was decided, and what it is being offered.
func summary(profile *model.Profile, loadout tool.Loadout, resolved *provider.Resolved, req provider.GenerateRequest) {
	fmt.Println("═══ WHAT THIS TURN RESOLVED TO ═══")
	fmt.Printf("vendor        %s (%s)\n", resolved.Vendor.Name, resolved.Vendor.VendorKey)
	fmt.Printf("model         %s\n", req.Model)
	fmt.Printf("reasoning     %v\n", req.Reasoning)
	fmt.Printf("tools offered %d\n", len(req.Tools))
	for _, t := range req.Tools {
		fmt.Printf("  - %s\n", t.Name)
	}
	if len(loadout.Schemas) != len(req.Tools) {
		fmt.Printf("(%d schemas loaded)\n", len(loadout.Schemas))
	}
	fmt.Println()
}

// theBytes runs the real adapter against a listener here, so what is printed is
// what the vendor would have received, in the vendor's own shape.
func theBytes(ctx context.Context, resolved *provider.Resolved, req provider.GenerateRequest) error {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		fmt.Println("═══ THE REQUEST, AS THE VENDOR WOULD RECEIVE IT ═══")
		fmt.Printf("%s %s\n", r.Method, r.URL.Path)
		for name := range r.Header {
			if name == "Authorization" || name == "X-Api-Key" {
				fmt.Printf("%s: (a credential, not printed)\n", name)
				continue
			}
			fmt.Printf("%s: %s\n", name, r.Header.Get(name))
		}
		fmt.Println()
		var pretty any
		if err := json.Unmarshal(body, &pretty); err == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(out))
		} else {
			fmt.Println(string(body))
		}
		// Enough of an answer that the adapter does not report a failure.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	// The vendor's own dialect, so the body is shaped the way that vendor
	// receives it, pointed at the listener above instead of at the vendor.
	dialect, ok := provider.DialectFor(resolved.Vendor.VendorKey)
	if !ok {
		return fmt.Errorf("this vendor speaks a dialect this dump cannot build (%q): "+
			"Anthropic has its own adapter", resolved.Vendor.VendorKey)
	}
	adapter, err := provider.NewOpenAICompatible(dialect, "not-a-real-key", server.URL, server.Client())
	if err != nil {
		return fmt.Errorf("build the adapter: %w", err)
	}
	events, err := adapter.Stream(ctx, req)
	if err != nil {
		return fmt.Errorf("ask the adapter to send it: %w", err)
	}
	for range events { //nolint:revive // drained on purpose: the answer is discarded
	}
	if len(captured) == 0 {
		return fmt.Errorf("the adapter sent nothing")
	}
	return nil
}
