package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"flexie.io/sag/internal/model"
)

// A model id is typed by a person, and a person mistypes. The vendor is the only
// authority on whether the thing they typed exists, so these tests are about one
// question: does the adapter actually go and ask, and does it come back with an
// answer that can be told apart from silence?

func TestAnthropicListsWhatItOffers(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "claude-opus-4-8", "display_name": "Claude Opus 4.8",
				 "type": "model", "created_at": "2026-01-01T00:00:00Z"},
				{"id": "claude-sonnet-5", "display_name": "Claude Sonnet 5",
				 "type": "model", "created_at": "2026-01-01T00:00:00Z"}
			],
			"has_more": false
		}`))
	}))
	defer srv.Close()

	models, err := NewAnthropic("key", srv.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}

	if path != "/v1/models" {
		t.Errorf("asked %q, want /v1/models", path)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	// The id is what gets stored and sent; the name is what a person reads.
	if models[0].ID != "claude-opus-4-8" || models[0].Name != "Claude Opus 4.8" {
		t.Errorf("first model is %+v", models[0])
	}
	if models[1].ID != "claude-sonnet-5" {
		t.Errorf("second model is %+v", models[1])
	}
}

func TestOpenAICompatibleListsWhatItOffers(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "deepseek-chat", "object": "model", "created": 1, "owned_by": "deepseek"},
				{"id": "deepseek-reasoner", "object": "model", "created": 1, "owned_by": "deepseek"}
			]
		}`))
	}))
	defer srv.Close()

	models, err := mustAdapter(t, model.VendorDeepSeek, "key", srv.URL).
		ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}

	if path != "/models" {
		t.Errorf("asked %q, want /models", path)
	}
	if len(models) != 2 || models[0].ID != "deepseek-chat" || models[1].ID != "deepseek-reasoner" {
		t.Fatalf("got %+v", models)
	}
	// This family publishes ids and no names. Inventing one would be a lie about
	// what the vendor said.
	if models[0].Name != "" {
		t.Errorf("name was invented: %q", models[0].Name)
	}
}

// Azure is the case the whole three-valued answer exists for. What you name in
// Azure is a deployment you created, not a model Azure publishes, so no catalog
// can confirm or deny it. Saying "unsupported" and saying "offers nothing" would
// be the same sentence to a caller that only looked at the list, and one of them
// would silently refuse every model an Azure customer ever tried to add.
func TestAzureSaysItCannotBeAskedRatherThanOfferingNothing(t *testing.T) {
	adapter := mustAdapter(t, model.VendorAzureOpenAI, "key", "https://acme.openai.azure.com")

	models, err := adapter.ListModels(context.Background())
	if !errors.Is(err, ErrListingUnsupported) {
		t.Fatalf("got (%+v, %v), want ErrListingUnsupported", models, err)
	}
	if models != nil {
		t.Errorf("an unanswerable question returned a list: %+v", models)
	}
}

// A vendor that is down tells us nothing about whether a model exists. The
// adapter must report that as a failure and never as an empty catalog, because
// an empty catalog is a positive claim and this is the absence of one.
func TestAVendorThatWillNotAnswerIsAnErrorAndNotAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	models, err := mustAdapter(t, model.VendorMistral, "key", srv.URL).
		ListModels(context.Background())
	if err == nil {
		t.Fatalf("a broken vendor returned %+v and no error", models)
	}
	if len(models) != 0 {
		t.Errorf("a broken vendor returned models: %+v", models)
	}
}
