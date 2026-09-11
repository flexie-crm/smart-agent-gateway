package app

import (
	"errors"
	"fmt"
	"testing"

	"flexie.io/sag/internal/mcpclient"
	"flexie.io/sag/internal/model"
)

// How a finished connection is reported to the console.
//
// This matters more than it looks. The consent runs in a browser, and in the
// desktop application that browser is a DIFFERENT application: the page that
// lands at the callback cannot reach the window that sent it away. So this
// message is the only account of the outcome the screen will ever get, and a
// screen that never hears anything is a screen that waits.

func notice(t *testing.T, m *model.MCPServer, result model.MCPSyncResult, err error) map[string]any {
	t.Helper()
	claim := mcpConnectState{ServerID: 7, WorkspaceID: 3, UserID: 11}
	envelope := mcpConnectNotice(claim, m, result, err)
	if envelope["type"] != "mcp_connection" {
		t.Fatalf("the console filters on the type: got %v", envelope["type"])
	}
	payload, ok := envelope["payload"].(map[string]any)
	if !ok {
		t.Fatalf("no payload: %#v", envelope)
	}
	return payload
}

func TestAFinishedConnectionSaysWhichOneAndWhatCameOfIt(t *testing.T) {
	server := &model.MCPServer{ID: 7, Name: "Flexie CRM"}
	payload := notice(t, server, model.MCPSyncResult{Offered: 4}, nil)

	if payload["connected"] != true {
		t.Errorf("connected: got %v, want true", payload["connected"])
	}
	if payload["id"] != int64(7) {
		t.Errorf("id: got %v, want 7", payload["id"])
	}
	if payload["name"] != "Flexie CRM" {
		t.Errorf("name: got %v", payload["name"])
	}
	if payload["detail"] != "Its tools are now available to grant." {
		t.Errorf("detail: got %v", payload["detail"])
	}
}

// Authorized, and the remote offered nothing yet. Reporting this as a plain
// success would leave somebody looking at a Tools screen that did not change,
// with no idea whether the connection worked.
func TestAConnectionThatFoundNoToolsSaysThatInstead(t *testing.T) {
	payload := notice(t, &model.MCPServer{ID: 7, Name: "Docs"}, model.MCPSyncResult{}, nil)

	if payload["connected"] != true {
		t.Errorf("it IS connected: got %v", payload["connected"])
	}
	if payload["detail"] != "No tools were found yet; use Sync." {
		t.Errorf("detail: got %v", payload["detail"])
	}
}

// The remote's own words survive the trip. A refusal that explains itself is
// the answer; a generic failure printed over it is a step backwards.
func TestARefusalIsReportedInTheRemotesOwnWords(t *testing.T) {
	refused := fmt.Errorf("exchange: %w",
		&mcpclient.RemoteOAuthError{Reason: "the user is not eligible for MCP access"})
	payload := notice(t, nil, model.MCPSyncResult{}, refused)

	if payload["connected"] != false {
		t.Errorf("connected: got %v, want false", payload["connected"])
	}
	if payload["detail"] != "the user is not eligible for MCP access" {
		t.Errorf("detail: got %v", payload["detail"])
	}
	// Nothing was loaded, so there is no name to give. The id still is: it is
	// what lets a screen know which row this was about.
	if _, named := payload["name"]; named {
		t.Errorf("a connection that never loaded has no name to report: %v", payload["name"])
	}
	if payload["id"] != int64(7) {
		t.Errorf("id: got %v, want 7", payload["id"])
	}
}

// A failure with nothing to quote still has to say what to do next, and it must
// not leak the shape of the machinery that failed.
func TestAFailureWithNoReasonStillSaysWhatToDo(t *testing.T) {
	payload := notice(t, nil, model.MCPSyncResult{}, errors.New("dial tcp 10.0.0.4:443: connect: refused"))

	if payload["connected"] != false {
		t.Errorf("connected: got %v", payload["connected"])
	}
	detail, _ := payload["detail"].(string)
	if detail != "The connection could not be completed. Start it again from the console." {
		t.Errorf("detail: got %q", detail)
	}
}

// An empty reason is not a reason. The remote said nothing usable, so the
// person gets the words that at least tell them what to do.
func TestAnEmptyRemoteReasonFallsBackRatherThanShowingNothing(t *testing.T) {
	payload := notice(t, nil, model.MCPSyncResult{}, &mcpclient.RemoteOAuthError{Reason: ""})

	if payload["detail"] == "" {
		t.Fatal("a refusal reported with no words at all")
	}
	if payload["detail"] != "The connection could not be completed. Start it again from the console." {
		t.Errorf("detail: got %v", payload["detail"])
	}
}
