package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"flexie.io/sag/internal/model"
)

// A pointer at a machine is not a vendor, and a local model is not typed in.

func TestAPointerAtAMachineIsNotOfferedAsAVendor(t *testing.T) {
	// It is a ROUTE, written when a workspace was given a model. Showing it here
	// would offer an administrator a vendor they never made, whose name means
	// nothing, whose endpoint cannot be changed, and whose deletion would take a
	// working model away without saying so.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":            nodeInfoBody,
		"GET /node/models/u-1": strings.Trim(nodeModelsBody, "[]"),
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")
	if _, err := env.app.AttachNodeModel(context.Background(), machine.ID, env.ws.ID, "u-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	env.createUser("v@test", "password1234", model.PermVendorsView, model.PermVendorsEdit,
		model.PermVendorsDelete, model.PermModelsView)
	token, _ := env.login("v@test", "password1234")

	rec := env.do(http.MethodGet, "/v1/vendors", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var listed struct {
		Vendors []struct {
			Name string `json:"name"`
		} `json:"vendors"`
		Kinds []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"kinds"`
	}
	env.decode(rec, &listed)
	for _, v := range listed.Vendors {
		if v.Name == "gpu-1" {
			t.Fatal("a route to a machine was listed as a vendor account")
		}
	}

	// The self-hosted kind is still offered, and is named for the server people
	// point it at rather than for "local": OUR local is a machine that joins
	// itself, and this is somebody else's server, which cannot.
	var offered bool
	for _, k := range listed.Kinds {
		if k.Key == model.VendorOpenAICompatible {
			offered = true
			if k.Name == "Local" {
				t.Error("the third-party endpoint is still called Local, which is now our machines")
			}
		}
	}
	if !offered {
		t.Error("a third-party endpoint can no longer be added at all")
	}

	// The pointer cannot be reached through the vendor routes either. Saying
	// "no such vendor" is the truth: it is not one.
	pointer, err := env.app.Store.Vendors().ByNodeID(context.Background(), env.ws.ID, machine.ID)
	if err != nil {
		t.Fatalf("no pointer: %v", err)
	}
	id := "/v1/vendors/" + strconv.FormatInt(pointer.ID, 10)
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, id},
		{http.MethodPut, id},
		{http.MethodDelete, id},
		{http.MethodDelete, id + "/credentials"},
	} {
		rec := env.do(call.method, call.path, token, map[string]string{"name": "hijacked"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d", call.method, call.path, rec.Code)
		}
	}
}

func TestALocalModelIsPickedAndNotTypedIn(t *testing.T) {
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":            nodeInfoBody,
		"GET /node/models":     nodeModelsBody,
		"GET /node/models/u-1": strings.Trim(nodeModelsBody, "[]"),
		"GET /node/pulls":      `[]`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")

	env.createUser("m@test", "password1234", model.PermModelsView, model.PermModelsCreate,
		model.PermMachinesView)
	token, _ := env.login("m@test", "password1234")

	// The dialog's one request: every machine and what is on it.
	rec := env.do(http.MethodGet, "/v1/models/machines", token, nil)
	env.expectStatus(rec, http.StatusOK)
	var choices struct {
		Machines []struct {
			ID     int64 `json:"id"`
			Models []struct {
				UID           string `json:"uid"`
				Handle        string `json:"handle"`
				ContextLength int    `json:"context_length"`
				Here          bool   `json:"here"`
			} `json:"models"`
		} `json:"machines"`
	}
	env.decode(rec, &choices)
	if len(choices.Machines) != 1 || len(choices.Machines[0].Models) != 1 {
		t.Fatalf("choices read wrong: %+v", choices)
	}
	if choices.Machines[0].Models[0].Here {
		t.Error("a model this workspace does not have was marked as taken")
	}

	// Adding it types nothing: the name, kind and context come off the weights.
	rec = env.do(http.MethodPost, "/v1/models/local", token,
		map[string]any{"machine_id": machine.ID, "uid": "u-1"})
	env.expectStatus(rec, http.StatusCreated)
	var created struct {
		ModelKey      string  `json:"model_key"`
		Type          string  `json:"type"`
		ContextWindow int     `json:"context_window"`
		InputPrice    float64 `json:"input_price_per_1m"`
	}
	env.decode(rec, &created)
	if created.ModelKey != "Qwen3-0.6B" || created.Type != "chat" || created.ContextWindow != 40960 {
		t.Errorf("the model was not read off the machine: %+v", created)
	}
	// Hardware you own has no per-token price.
	if created.InputPrice != 0 {
		t.Errorf("a local model was given a price: %v", created.InputPrice)
	}

	// Asked again, it is marked as already here rather than offered twice.
	rec = env.do(http.MethodGet, "/v1/models/machines", token, nil)
	env.decode(rec, &choices)
	if !choices.Machines[0].Models[0].Here {
		t.Error("a model this workspace has was offered again")
	}

	// And the models screen names the machine as its source.
	rec = env.do(http.MethodGet, "/v1/models", token, nil)
	var listed struct {
		Models []struct {
			Machine string `json:"machine"`
		} `json:"models"`
	}
	env.decode(rec, &listed)
	if len(listed.Models) != 1 || listed.Models[0].Machine != "gpu-1" {
		t.Errorf("the model did not say which machine it is on: %+v", listed.Models)
	}
}

func TestAddingALocalModelNeedsThePermission(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("none@test", "password1234", model.PermModelsView)
	token, _ := env.login("none@test", "password1234")

	env.expectStatus(env.do(http.MethodGet, "/v1/models/machines", token, nil), http.StatusForbidden)
	env.expectStatus(
		env.do(http.MethodPost, "/v1/models/local", token, map[string]any{"machine_id": 1, "uid": "u-1"}),
		http.StatusForbidden)
}

func TestAListEndpointAnswersAListAndNeverNull(t *testing.T) {
	// A nil slice in Go marshals as `null`, and a browser reading a field its
	// type promises is a list then crashes on `.filter` or `.length`. This
	// asserts the RAW body, because decoding into a struct turns `null` back
	// into an empty slice and hides exactly the bug it is here for.
	//
	// It shipped twice: a machine with nothing on it answered `"models":null`,
	// and a model no workspace had answered `"workspaces":null`. Both fixtures
	// in the suites used `[]`, so both suites passed.
	env := newTestEnv(t)
	node := startFakeNode(t, env, "gpu-1", map[string]string{
		"GET /node":        nodeInfoBody,
		"GET /node/models": nodeModelsBody,
		"GET /node/pulls":  `[]`,
	})
	machine := env.aMachine("gpu-1", node.url, "node-key")

	env.createUser("nulls@test", "password1234", model.PermModelsView, model.PermMachinesView)
	token, _ := env.login("nulls@test", "password1234")

	// A machine that has a model no workspace has been given.
	rec := env.do(http.MethodGet, "/v1/nodes/"+strconv.FormatInt(machine.ID, 10), token, nil)
	env.expectStatus(rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"workspaces":null`) {
		t.Errorf("a model nobody has answered null instead of an empty list:\n%s", rec.Body.String())
	}

	// And the add-a-local-model dialog, against a machine with nothing on it.
	bare := startFakeNode(t, env, "gpu-empty", map[string]string{
		"GET /node":        nodeInfoBody,
		"GET /node/models": `[]`,
		"GET /node/pulls":  `[]`,
	})
	env.aMachine("gpu-empty", bare.url, "node-key")

	rec = env.do(http.MethodGet, "/v1/models/machines", token, nil)
	env.expectStatus(rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"models":null`) {
		t.Errorf("a machine with nothing on it answered null instead of an empty list:\n%s", rec.Body.String())
	}
}
