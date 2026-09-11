package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/model"
)

// The admin surface for native custom tools, over HTTP: discover the templates
// and a driver's form, create a query tool, test it, see it in the list, and
// delete it.
func TestCustomToolAdminEndpoints(t *testing.T) {
	dsn := os.Getenv("SAG_TEST_DSN")
	if dsn == "" {
		t.Skip("SAG_TEST_DSN not set; skipping custom-tool API suite")
	}
	parsed, _ := mysql.ParseDSN(dsn)
	host, portStr, _ := strings.Cut(parsed.Addr, ":")
	port, _ := strconv.Atoi(portStr)

	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// The templates, and the query template's MySQL form.
	var templates []map[string]any
	env.decode(env.do(http.MethodGet, "/v1/tools/templates", token, nil), &templates)
	if !containsName(templates, "query") {
		t.Fatalf("the query template was not offered: %+v", templates)
	}
	rec := env.do(http.MethodGet, "/v1/tools/templates/query/fields?variant=mysql", token, nil)
	env.expectStatus(rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"password"`) || !strings.Contains(rec.Body.String(), `"access"`) {
		t.Fatalf("the driver form is missing fields: %s", rec.Body.String())
	}

	settings := map[string]any{
		"access": "read", "host": host, "port": port,
		"database": "information_schema", "username": parsed.User, "password": parsed.Passwd,
		"tls.mode": "disable",
	}

	// Test the connection before saving.
	var testRes map[string]any
	env.decode(env.do(http.MethodPost, "/v1/tools/custom/test", token, map[string]any{
		"template": "query", "variant": "mysql", "settings": settings,
	}), &testRes)
	if testRes["ok"] != true {
		t.Fatalf("the connection test failed: %+v", testRes)
	}

	// Create it.
	rec = env.do(http.MethodPost, "/v1/tools/custom", token, map[string]any{
		"template": "query", "variant": "mysql", "alias": "local", "settings": settings,
	})
	env.expectStatus(rec, http.StatusCreated)
	var created toolBody
	env.decode(rec, &created)
	if created.Name != "query_local" || created.Kind != "custom" || created.Template != "query" {
		t.Fatalf("the created tool is wrong: %+v", created)
	}

	// It shows up in the tool list.
	var list []toolBody
	env.decode(env.do(http.MethodGet, "/v1/tools", token, nil), &list)
	if !containsTool(list, "query_local") {
		t.Fatalf("the custom tool is not in the list: %+v", list)
	}

	// The detail endpoint prefills the edit form: the driver, the settings with
	// the secret blanked, the guide, and the parameter descriptions.
	id := strconv.FormatInt(created.ID, 10)
	var detail struct {
		Description       string            `json:"description"`
		Variant           string            `json:"variant"`
		Guide             string            `json:"guide"`
		Settings          map[string]any    `json:"settings"`
		ParamDescriptions map[string]string `json:"param_descriptions"`
	}
	env.decode(env.do(http.MethodGet, "/v1/tools/"+id, token, nil), &detail)
	if detail.Variant != "mysql" {
		t.Fatalf("the edit view has the wrong driver: %+v", detail)
	}
	if detail.Settings["password"] != "" {
		t.Fatalf("the secret was handed back to the form: %v", detail.Settings["password"])
	}
	if detail.Settings["host"] != host {
		t.Fatalf("a non-secret setting did not come back: %v", detail.Settings["host"])
	}
	if detail.ParamDescriptions["sql"] == "" {
		t.Fatalf("the parameter descriptions were not returned: %+v", detail.ParamDescriptions)
	}

	// Edit the tool: change the presentation, leave the secret blank (unchanged).
	edit := detail.Settings
	edit["password"] = ""
	env.expectStatus(env.do(http.MethodPut, "/v1/tools/custom/"+id, token, map[string]any{
		"settings":           edit,
		"description":        "Edited over the API.",
		"guide":              "Edited guide.",
		"param_descriptions": map[string]string{"sql": "Edited sql description."},
	}), http.StatusOK)

	// The change is reflected, and the tool still connects (the secret survived).
	env.decode(env.do(http.MethodGet, "/v1/tools/"+id, token, nil), &detail)
	if detail.Description != "Edited over the API." || detail.Guide != "Edited guide." {
		t.Fatalf("the edit did not persist: %+v", detail)
	}
	if detail.ParamDescriptions["sql"] != "Edited sql description." {
		t.Fatalf("the parameter description did not persist: %+v", detail.ParamDescriptions)
	}

	// Testing the edit with the secret left blank still connects: the stored
	// password is carried forward rather than sent empty.
	var editTest map[string]any
	env.decode(env.do(http.MethodPost, "/v1/tools/custom/"+id+"/test", token, map[string]any{"settings": detail.Settings}), &editTest)
	if editTest["ok"] != true {
		t.Fatalf("testing the edit with a blank secret failed: %+v", editTest)
	}

	// Delete it.
	env.expectStatus(env.do(http.MethodDelete, "/v1/tools/"+strconv.FormatInt(created.ID, 10), token, nil), http.StatusNoContent)
	env.decode(env.do(http.MethodGet, "/v1/tools", token, nil), &list)
	if containsTool(list, "query_local") {
		t.Fatal("the custom tool survived deletion")
	}
}

// A bad alias is a clean 400, not a 500.
func TestCreateCustomToolRejectsBadInput(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	rec := env.do(http.MethodPost, "/v1/tools/custom", token, map[string]any{
		"template": "query", "variant": "mysql", "alias": "Bad Alias",
		"settings": map[string]any{"access": "read", "host": "h", "database": "d", "username": "u"},
	})
	env.expectStatus(rec, http.StatusBadRequest)
}

func containsName(items []map[string]any, name string) bool {
	for _, i := range items {
		if i["name"] == name {
			return true
		}
	}
	return false
}

func containsTool(items []toolBody, name string) bool {
	for _, i := range items {
		if i.Name == name {
			return true
		}
	}
	return false
}
