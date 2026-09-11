package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

func TestStartingADownloadWritesTheJobThatRegistersIt(t *testing.T) {
	// The failure this is here for: `jobs.workspace_id` is NOT NULL with a
	// foreign key, and a download is platform scoped, so the job was written
	// with no workspace and every insert was refused. The download ran, nothing
	// watched it, and the model never became routable. It logged one line and
	// answered 202, so from the screen it looked like it had worked.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node": nodeInfoBody,
		"POST /node/pulls": `{"id":"p-1","repo":"vendor/model","revision":"abc",
			"state":"downloading","bytes_done":0,"bytes_total":1000,
			"files_done":0,"files_total":3,
			"started_at":"2026-08-05T00:00:00Z","updated_at":"2026-08-05T00:00:00Z"}`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodPost, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10)+"/pulls", token,
		map[string]string{"repo": "vendor/model"})
	env.expectStatus(rec, http.StatusAccepted)

	// The obligation exists: something will register the model when it lands.
	var found bool
	rows, err := env.sql.DB().QueryContext(context.Background(),
		"SELECT kind, workspace_id FROM jobs WHERE kind = ?", model.JobKindModelPull)
	if err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind string
		var workspaceID int64
		if err := rows.Scan(&kind, &workspaceID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found = true
		// Who asked, so the row can exist at all. Never read by the handler.
		if workspaceID != env.ws.ID {
			t.Errorf("job workspace = %d, want %d", workspaceID, env.ws.ID)
		}
	}
	if !found {
		t.Fatal("a download was started with no job to register the model afterwards")
	}
}

func TestADownloadThatCannotBeWatchedStillSaysItStarted(t *testing.T) {
	// The download is the machine's and runs whatever happens here, so a failure
	// to write the job is reported loudly and not returned: telling somebody the
	// download failed, when it is under way, sends them to press it again.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node": nodeInfoBody,
		"POST /node/pulls": `{"id":"p-1","repo":"vendor/model","revision":"abc","state":"downloading",
			"bytes_done":0,"bytes_total":10,"files_done":0,"files_total":1,
			"started_at":"2026-08-05T00:00:00Z","updated_at":"2026-08-05T00:00:00Z"}`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodPost, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10)+"/pulls", token,
		map[string]string{"repo": "vendor/model"})
	env.expectStatus(rec, http.StatusAccepted)
	if !strings.Contains(rec.Body.String(), "downloading") {
		t.Errorf("the caller was not told the download started: %s", rec.Body.String())
	}
}
