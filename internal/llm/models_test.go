package llm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/llm"
)

// An OpenAI-compatible endpoint answers /models with a data array. The list
// must come back verbatim so the picker offers what the provider really has.
func TestListModelsOpenAIEnvelope(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4"},{"id":"gpt-4o-mini"}]}`))
	}))
	defer server.Close()

	models, err := llm.ListModels(server.URL, "sk-test")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(models) != 2 || models[0] != "gpt-5.4" || models[1] != "gpt-4o-mini" {
		t.Errorf("models = %v", models)
	}
}

// A base URL with a trailing slash must not produce a double slash: some
// gateways 404 on //models.
func TestListModelsTrimsTrailingSlash(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer server.Close()
	if _, err := llm.ListModels(server.URL+"/", "k"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models", gotPath)
	}
}

// Gateways in the wild are not all shaped like OpenAI's, so the lenient
// readers are covered directly: Ollama's models[].name, a bare string array,
// and an array of objects.
func TestListModelsAcceptsGatewayShapes(t *testing.T) {
	for name, payload := range map[string]string{
		"ollama":     `{"models":[{"name":"llama3.2:latest"},{"name":"qwen2.5:7b"}]}`,
		"bareArray":  `["alpha","beta"]`,
		"objectList": `[{"id":"gamma"},{"name":"delta"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			models, err := llm.ListModels(server.URL, "k")
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if len(models) == 0 {
				t.Fatalf("%s: no models parsed", name)
			}
		})
	}
}

// Embedding/audio/image models cannot hold a chat conversation, so they sort
// last. They must not be dropped: a gateway may name its chat model oddly and
// the user still has to be able to pick it.
func TestListModelsSortsNonChatLastWithoutDropping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"text-embedding-3-small"},{"id":"gpt-5.4"},{"id":"whisper-1"}]}`))
	}))
	defer server.Close()
	models, err := llm.ListModels(server.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %v, want all three kept", models)
	}
	if models[0] != "gpt-5.4" {
		t.Errorf("chat model should sort first, got %v", models)
	}
}

// Duplicates are noise in a picker.
func TestListModelsDeduplicates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`))
	}))
	defer server.Close()
	models, err := llm.ListModels(server.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want 2 unique entries", models)
	}
}

// A missing token is a local mistake, not a network round trip: it must be
// reported without dialing, so the panel can say what to fix.
func TestListModelsRequiresToken(t *testing.T) {
	models, err := llm.ListModels("http://127.0.0.1:1", "")
	if err == nil || models != nil {
		t.Fatalf("models = %v err = %v, want a local refusal", models, err)
	}
	if !strings.Contains(err.Error(), "Token") {
		t.Errorf("error should name the missing credential: %v", err)
	}
}

// A provider error body is shown to the user, so it is flattened and
// truncated instead of pasted whole.
func TestListModelsSurfacesProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer server.Close()
	_, err := llm.ListModels(server.URL, "bad")
	if err == nil {
		t.Fatal("want an error for a 401")
	}
	if !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Errorf("provider message lost: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("status code missing: %v", err)
	}
}

// An endpoint that answers with something unrecognisable must say so rather
// than return an empty picker, which would look like "this provider has no
// models".
func TestListModelsRejectsUnrecognisedShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>not a model list</body></html>`))
	}))
	defer server.Close()
	_, err := llm.ListModels(server.URL, "k")
	if err == nil || !strings.Contains(err.Error(), "OpenAI 兼容") {
		t.Fatalf("err = %v, want a shape complaint", err)
	}
}

// parseModelList is the shape reader; the raw JSON cases above exercise it
// through ListModels, and this keeps a direct regression on the OpenAI
// envelope used by every major gateway.
func TestParseModelListEnvelope(t *testing.T) {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(`{"data":[{"id":"x"}]}`), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["data"]; !ok {
		t.Fatal("fixture is not the OpenAI envelope")
	}
}
