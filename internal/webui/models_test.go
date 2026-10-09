package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/webui"
)

// newModelServer is a dashboard whose /api/models is answered by a stub, so
// the suite never calls a real provider.
func newModelServer(t *testing.T, list func(baseURL, apiKey string) ([]string, error)) *webui.Server {
	t.Helper()
	server, _ := newTestServer(t)
	server.ModelList = list
	return server
}

// The panel can ask for the list before saving anything: the URL and token
// typed in the form are what get used, so a model can be picked first and
// saved afterwards.
func TestModelsUsesFormValuesBeforeSave(t *testing.T) {
	var gotURL, gotKey string
	server := newModelServer(t, func(baseURL, apiKey string) ([]string, error) {
		gotURL, gotKey = baseURL, apiKey
		return []string{"gpt-5.4", "gpt-4o-mini"}, nil
	})
	rec := postJSON(t, server, "/api/models", `{"llm_base_url":"https://llm.example/v1","llm_api_key":"sk-typed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotURL != "https://llm.example/v1" || gotKey != "sk-typed" {
		t.Errorf("stub saw url=%q key=%q", gotURL, gotKey)
	}
	var payload struct {
		BaseURL string   `json:"base_url"`
		Models  []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 2 || payload.Models[0] != "gpt-5.4" {
		t.Errorf("models = %v", payload.Models)
	}
}

// When the form is empty the stored credentials are used instead, so the
// button keeps working after a save.
func TestModelsFallsBackToStoredCredentials(t *testing.T) {
	// A desktop server is the single-user shape whose settings panel stores
	// credentials locally; that is where "already saved" comes from.
	server, _ := newDesktopServer(t)
	server.ModelList = func(baseURL, apiKey string) ([]string, error) {
		if baseURL != "https://stored.example/v1" || apiKey != "sk-stored" {
			t.Errorf("stub saw url=%q key=%q, want the stored pair", baseURL, apiKey)
		}
		return []string{"m"}, nil
	}
	if rec := postJSON(t, server, "/api/local/credentials",
		`{"llm_base_url":"https://stored.example/v1","llm_api_key":"sk-stored"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, server, "/api/models", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

// Without any token there is nothing to ask with, so the panel gets a clear
// instruction instead of a provider error.
func TestModelsWithoutTokenExplains(t *testing.T) {
	server := newModelServer(t, func(baseURL, apiKey string) ([]string, error) {
		t.Fatal("the loader must not be called without a token")
		return nil, nil
	})
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	rec := postJSON(t, server, "/api/models", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Token") {
		t.Errorf("error should name the missing credential: %s", rec.Body.String())
	}
}

// The token must never come back in the response, and neither must it leak
// into /api/config after being used.
func TestModelsNeverEchoesTheToken(t *testing.T) {
	server := newModelServer(t, func(baseURL, apiKey string) ([]string, error) {
		return []string{"m"}, nil
	})
	rec := postJSON(t, server, "/api/models", `{"llm_base_url":"https://llm.example/v1","llm_api_key":"sk-secret"}`)
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Errorf("/api/models echoed the token: %s", rec.Body.String())
	}
	if strings.Contains(fetchBody(t, server, "/api/config"), "sk-secret") {
		t.Error("/api/config leaked the token used for the model list")
	}
}

// A provider failure is a 502 with the reason, so the panel can show it
// inline instead of silently offering an empty picker.
func TestModelsPropagatesProviderFailure(t *testing.T) {
	server := newModelServer(t, func(baseURL, apiKey string) ([]string, error) {
		return nil, errStub("连接模型服务失败: dial tcp: connection refused")
	})
	rec := postJSON(t, server, "/api/models", `{"llm_base_url":"https://x/v1","llm_api_key":"k"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("reason lost: %s", rec.Body.String())
	}
}

// GET is not how a credential-bearing query is asked, matching the other
// POST-only routes.
func TestModelsRejectsGet(t *testing.T) {
	server := newModelServer(t, func(baseURL, apiKey string) ([]string, error) { return nil, nil })
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
