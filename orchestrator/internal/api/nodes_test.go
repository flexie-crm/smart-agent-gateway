package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"flexie.io/sag/internal/model"
)

// The node screens, driven against a fake machine.
//
// Fake here rather than the real node because what these assert is the seam:
// that the vendor row becomes a client, that the key is sent, that the machine's
// own refusals reach the browser in the machine's own words, and that deleting a
// model takes its registry row with it. Whether the machine itself behaves is
// the node's own suite, and it is proved against real weights.

// fakeNode stands up a machine and returns its address plus a record of what it
// was asked.
type fakeNode struct {
	url     string
	calls   atomic.Int64
	lastKey atomic.Value
}

// startFakeNode stands one up under the machine name it will be registered as.
//
// It serves TLS with a certificate from this deployment's own authority, and
// demands one back, because that is what a machine is now: there is no
// unencrypted way to reach one, and a fake that could be reached over plain
// HTTP would be testing a path that no longer exists.
func startFakeNode(t *testing.T, env *testEnv, name string, routes map[string]string) *fakeNode {
	t.Helper()
	node := &fakeNode{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node.calls.Add(1)
		node.lastKey.Store(r.Header.Get("Authorization"))

		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such thing"}}`))
			return
		}
		if after, refusal := strings.CutPrefix(body, "!"); refusal {
			// "!<status> <body>" is a refusal, in the machine's own words.
			code, payload, _ := strings.Cut(after, " ")
			status, err := strconv.Atoi(code)
			if err != nil {
				t.Errorf("fake node route %q has no status", body)
				status = http.StatusInternalServerError
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	server.TLS = env.machineIdentity(name)
	server.StartTLS()
	t.Cleanup(server.Close)
	node.url = server.URL
	return node
}

// machineIdentity issues the fake a certificate the orchestrator will accept:
// named for the machine, from the deployment's authority, and demanding a
// client certificate from the same one.
func (e *testEnv) machineIdentity(name string) *tls.Config {
	e.t.Helper()
	authority, err := e.app.Authority(context.Background())
	if err != nil {
		e.t.Fatalf("authority: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		e.t.Fatalf("key: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: machineNodeID(name)}}, key)
	if err != nil {
		e.t.Fatalf("certificate request: %v", err)
	}
	certPEM, err := authority.Issue(csr, machineNodeID(name), []string{"127.0.0.1"}, time.Now())
	if err != nil {
		e.t.Fatalf("issue: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		e.t.Fatalf("encode key: %v", err)
	}
	identity, err := tls.X509KeyPair(certPEM,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		e.t.Fatalf("identity: %v", err)
	}
	return authority.ServerTLS(identity)
}

// machineNodeID is how a machine's row and its certificate agree on a name.
func machineNodeID(name string) string { return "nd-" + name }

// aMachine registers a machine the way a joining node would, with its key
// sealed. Platform scope: no workspace anywhere in it.
func (e *testEnv) aMachine(name, baseURL, key string) *model.InferenceNode {
	e.t.Helper()
	sealed, err := e.app.SealCredentials(key)
	if err != nil {
		e.t.Fatalf("seal: %v", err)
	}
	node := &model.InferenceNode{
		NodeID:  machineNodeID(name),
		Name:    name,
		BaseURL: baseURL + "/v1",
		Key:     sealed,
	}
	if err := e.app.Store.Nodes().Create(context.Background(), node); err != nil {
		e.t.Fatalf("create machine: %v", err)
	}
	return node
}

func (e *testEnv) adminToken() string {
	e.t.Helper()
	e.createUser("nodes@test", "password1234", model.PermMachinesView,
		model.PermMachinesCreate, model.PermMachinesEdit, model.PermMachinesDelete)
	token, _ := e.login("nodes@test", "password1234")
	return token
}

const nodeInfoBody = `{"name":"gpu-1","can_infer":true,"version":"0.1.0",
	"uptime_seconds":10,"models":1,"resident":1,"downloads_active":0,
	"machine":{"memory_total":100,"memory_free":50,"disk_total":200,"disk_free":150,"processors":8}}`

const nodeModelsBody = `[{"uid":"u-1","repo":"Qwen/Qwen3-0.6B","revision":"abc",
	"name":"Qwen3-0.6B","handle":"Qwen3-0.6B","kind":"chat",
	"facts":{"context_length":40960,"files":7,"size_bytes":1519182365},
	"settings":{},"resident":true,"residency":"resident",
	"added_at":"2026-08-04T22:37:31Z"}]`

func TestNodesListsAMachineAndWhatItSaidAboutItself(t *testing.T) {
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{"GET /node": nodeInfoBody})
	env.aMachine("gpu-1", node.url, "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodGet, "/v1/nodes", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Nodes []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Reachable bool   `json:"reachable"`
			Problem   string `json:"problem"`
			Info      *struct {
				Name     string `json:"name"`
				CanInfer bool   `json:"can_infer"`
			} `json:"info"`
		} `json:"nodes"`
	}
	env.decode(rec, &got)

	if len(got.Nodes) != 1 {
		t.Fatalf("got %d machines", len(got.Nodes))
	}
	n := got.Nodes[0]
	if !n.Reachable || n.Info == nil || n.Info.Name != "gpu-1" || !n.Info.CanInfer {
		t.Fatalf("machine read wrong: %+v", n)
	}
	// The key the vendor row holds, sealed, reached the machine.
	if key, _ := node.lastKey.Load().(string); key != "Bearer node-key" {
		t.Errorf("the machine was asked with %q", key)
	}
}

func TestAMachineThatIsOffIsStillOnTheListSayingSo(t *testing.T) {
	// A row missing from the list would leave somebody wondering whether they
	// imagined configuring it. It says it is down instead.
	env := newTestEnv(t)
	env.aMachine("gpu-off", "http://127.0.0.1:1", "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodGet, "/v1/nodes", token, nil)
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Nodes []struct {
			Name      string `json:"name"`
			Reachable bool   `json:"reachable"`
			Problem   string `json:"problem"`
		} `json:"nodes"`
	}
	env.decode(rec, &got)
	if len(got.Nodes) != 1 {
		t.Fatalf("got %d machines", len(got.Nodes))
	}
	if got.Nodes[0].Reachable {
		t.Error("a machine that is off read as reachable")
	}
	if got.Nodes[0].Problem == "" {
		t.Error("a machine that is off did not say why")
	}
}

func TestOneMachinesScreenSaysWhichWorkspacesMayUseEachModel(t *testing.T) {
	// The point of the screen: the weights sit on one disk, and what varies is
	// who may route to them. That is invisible anywhere else.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":            nodeInfoBody,
		"GET /node/models":     nodeModelsBody,
		"GET /node/models/u-1": strings.Trim(nodeModelsBody, "[]"),
		"GET /node/pulls":      `[]`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodGet, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10), token, nil)
	env.expectStatus(rec, http.StatusOK)

	var got struct {
		Machine struct {
			Reachable bool `json:"reachable"`
			Models    []struct {
				UID        string `json:"uid"`
				Handle     string `json:"handle"`
				Workspaces []struct {
					WorkspaceID int64  `json:"workspace_id"`
					Name        string `json:"name"`
					ModelID     int64  `json:"model_id"`
				} `json:"workspaces"`
			} `json:"models"`
		} `json:"machine"`
		Workspaces []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"workspaces"`
	}
	env.decode(rec, &got)
	if !got.Machine.Reachable || len(got.Machine.Models) != 1 {
		t.Fatalf("screen read wrong: %+v", got)
	}
	if len(got.Machine.Models[0].Workspaces) != 0 {
		t.Errorf("a model nobody was given claimed a workspace: %+v", got.Machine.Models[0])
	}
	// The workspaces it COULD be given to ride on the same answer, because the
	// screen cannot draw the one without the other.
	if len(got.Workspaces) == 0 {
		t.Error("the screen was not told which workspaces exist")
	}

	// Give it to the workspace, and the same screen should say so.
	shared, err := env.app.AttachNodeModel(context.Background(), machine.ID, env.ws.ID, "u-1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	rec = env.do(http.MethodGet, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10), token, nil)
	env.decode(rec, &got)
	reach := got.Machine.Models[0].Workspaces
	if len(reach) != 1 || reach[0].WorkspaceID != env.ws.ID || reach[0].ModelID != shared.ID {
		t.Errorf("the workspace that was given the model is not shown: %+v", reach)
	}
}

func TestGivingOneModelToTwoWorkspacesDownloadsNothingTwice(t *testing.T) {
	// The property the whole platform-scoped shape exists for: the weights are
	// on the machine, loaded once, answering on one port. A second workspace
	// costs rows, not gigabytes.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":            nodeInfoBody,
		"GET /node/models":     nodeModelsBody,
		"GET /node/models/u-1": strings.Trim(nodeModelsBody, "[]"),
		"GET /node/pulls":      `[]`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")

	other := &model.Workspace{Slug: "beta", Name: "Beta"}
	if err := env.app.Store.Workspaces().Create(context.Background(), other); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	before := node.calls.Load()
	for _, ws := range []int64{env.ws.ID, other.ID} {
		if _, err := env.app.AttachNodeModel(context.Background(), machine.ID, ws, "u-1"); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}

	// Both workspaces can route to it, through their own pointer at the machine.
	for _, ws := range []int64{env.ws.ID, other.ID} {
		pointer, err := env.app.Store.Vendors().ByNodeID(context.Background(), ws, machine.ID)
		if err != nil {
			t.Fatalf("workspace %d has no route: %v", ws, err)
		}
		// The pointer carries no address and no key of its own: both are read
		// off the machine, stored once.
		if pointer.BaseURL != node.url+"/v1" {
			t.Errorf("the pointer did not resolve the machine's address: %q", pointer.BaseURL)
		}
		if !pointer.HasCredentials() {
			t.Error("the pointer did not resolve the machine's key")
		}
		models, _ := env.app.Store.AIModels().List(context.Background(), ws)
		if len(models) != 1 || models[0].ModelKey != "Qwen3-0.6B" {
			t.Errorf("workspace %d models: %+v", ws, models)
		}
	}

	// Nothing was asked of the machine beyond reading the model, and certainly
	// no download was started.
	if started := node.calls.Load() - before; started > 2 {
		t.Errorf("sharing a model made %d calls to the machine", started)
	}
}

func TestAMachinesRefusalReachesTheBrowserInItsOwnWords(t *testing.T) {
	// The machine is the only side that knows why: that a repository offers
	// four compression levels, that there is not enough room, that the model is
	// already there. Flattening that would throw away the only account there is.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node": nodeInfoBody,
		"POST /node/pulls": `!409 {"error":{"code":"conflict",` +
			`"message":"this node already has Qwen3-0.6B at this version"}}`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	token := env.adminToken()

	rec := env.do(http.MethodPost, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10)+"/pulls", token,
		map[string]string{"repo": "Qwen/Qwen3-0.6B"})
	env.expectStatus(rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "already has Qwen3-0.6B") {
		t.Errorf("the machine's words were lost: %s", rec.Body.String())
	}
}

func TestAWrongKeyIsOursAndNotReportedAsTheAdministratorBeingSignedOut(t *testing.T) {
	// Passing the machine's 401 through would tell somebody they are not signed
	// in, when what happened is that this server holds the wrong key for it.
	env := newTestEnv(t)
	const refused = `!401 {"error":{"code":"unauthorised","message":"not authorised"}}`
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		// A machine holding a different key refuses everything, not just the
		// probe, so the route the test drives has to refuse too.
		"GET /node":                refused,
		"GET /node/library/search": refused,
	})
	machine := env.aMachine("gpu-1", node.url, "wrong-key")
	token := env.adminToken()

	rec := env.do(http.MethodGet, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10)+"/library/search?q=x", token, nil)
	env.expectStatus(rec, http.StatusBadGateway)
	if !strings.Contains(rec.Body.String(), "node_key") {
		t.Errorf("the failure was not named as ours: %s", rec.Body.String())
	}
}

func TestDeletingAModelTakesTheRowThatRoutedToItAsWell(t *testing.T) {
	// A row pointing at weights that are gone is a model an agent can still be
	// assigned to, failing in front of somebody asking a question rather than
	// in front of the administrator who deleted it.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":               nodeInfoBody,
		"GET /node/models/u-1":    strings.Trim(nodeModelsBody, "[]"),
		"DELETE /node/models/u-1": `{"uid":"u-1","name":"Qwen3-0.6B","freed_bytes":1519182365}`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	row, err := env.app.AttachNodeModel(context.Background(), machine.ID, env.ws.ID, "u-1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	token := env.adminToken()

	rec := env.do(http.MethodDelete, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10)+"/models/u-1", token, nil)
	env.expectStatus(rec, http.StatusOK)

	models, err := env.app.Store.AIModels().List(context.Background(), env.ws.ID)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	for _, m := range models {
		if m.ID == row.ID {
			t.Fatal("the route to the deleted model is still there")
		}
	}
}

func TestEveryNodeRouteRefusesSomebodyWithoutThePermission(t *testing.T) {
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{"GET /node": nodeInfoBody})
	machine := env.aMachine("gpu-1", node.url, "node-key")

	env.createUser("nobody@test", "password1234")
	token, _ := env.login("nobody@test", "password1234")
	id := strconv.FormatInt(machine.ID, 10)

	guarded := []struct{ method, path string }{
		{http.MethodGet, "/v1/nodes"},
		{http.MethodGet, "/v1/nodes/" + id},
		{http.MethodGet, "/v1/nodes/" + id + "/library/search?q=x"},
		{http.MethodGet, "/v1/nodes/" + id + "/library/describe?repo=v/m"},
		{http.MethodPost, "/v1/nodes/" + id + "/pulls"},
		{http.MethodDelete, "/v1/nodes/" + id + "/pulls/p1"},
		{http.MethodGet, "/v1/nodes/" + id + "/models/u-1/form"},
		{http.MethodPut, "/v1/nodes/" + id + "/models/u-1/workspaces"},
		{http.MethodGet, "/v1/nodes/join-token"},
		{http.MethodPut, "/v1/nodes/" + id + "/models/u-1/settings"},
		{http.MethodPost, "/v1/nodes/" + id + "/models/u-1/load"},
		{http.MethodPost, "/v1/nodes/" + id + "/models/u-1/unload"},
		{http.MethodDelete, "/v1/nodes/" + id + "/models/u-1"},
	}
	for _, route := range guarded {
		rec := env.do(route.method, route.path, token, map[string]string{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s answered %d without the permission", route.method, route.path, rec.Code)
		}
	}
	if node.calls.Load() != 0 {
		t.Error("the machine was asked something on behalf of somebody who may not")
	}
}

func TestAMachineIdThatIsNotOneIsRefusedBeforeAnythingIsContacted(t *testing.T) {
	env := newTestEnv(t)
	token := env.adminToken()
	for _, bad := range []string{"abc", "0", "-1"} {
		rec := env.do(http.MethodGet, "/v1/nodes/"+bad, token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("machine id %q answered %d", bad, rec.Code)
		}
	}
}
