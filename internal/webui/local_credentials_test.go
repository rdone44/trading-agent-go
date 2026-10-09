package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/localcreds"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// newDesktopServer is the desktop edition in tests: no account vault, a local
// credential file instead, and the live gate wired to that file exactly as
// cmd/trading-agent-desktop wires it.
func newDesktopServer(t *testing.T) (*webui.Server, *localcreds.Store) {
	t.Helper()
	server, dir := newTestServer(t)
	store, err := localcreds.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("local creds: %v", err)
	}
	server.Desktop = true
	server.LocalCreds = store
	return server, store
}

// The desktop page must be able to reach the credential form: /api/config
// reports local single-user mode with presence flags, and the account-only
// fields (login, logout) are not part of it.
func TestDesktopConfigAdvertisesLocalCredentials(t *testing.T) {
	server, _ := newDesktopServer(t)
	body := fetchBody(t, server, "/api/config")
	var payload struct {
		Desktop       bool `json:"desktop"`
		LocalSettings bool `json:"local_settings"`
		Auth          struct {
			Enabled   bool   `json:"enabled"`
			Local     bool   `json:"local"`
			Username  string `json:"username"`
			BinAPI    bool   `json:"binance_api"`
			LLMKey    bool   `json:"llm_key"`
			AllowLive bool   `json:"allow_live"`
			Path      string `json:"path"`
		} `json:"auth"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	if !payload.Desktop || !payload.LocalSettings {
		t.Errorf("desktop/local_settings = %v/%v, want true/true", payload.Desktop, payload.LocalSettings)
	}
	if !payload.Auth.Enabled || !payload.Auth.Local {
		t.Errorf("auth = %+v, want enabled local single-user mode", payload.Auth)
	}
	if payload.Auth.Path == "" {
		t.Error("auth.path is empty; the settings panel cannot tell the user where keys are stored")
	}
	if payload.Auth.BinAPI || payload.Auth.LLMKey || payload.Auth.AllowLive {
		t.Errorf("fresh install already reports credentials: %+v", payload.Auth)
	}
}

// Saving from the desktop settings panel writes the file and flips the
// presence flags, without ever echoing a secret back.
func TestDesktopSaveCredentialsWritesFileWithoutEcho(t *testing.T) {
	server, store := newDesktopServer(t)
	rec := postJSON(t, server, "/api/local/credentials", `{
		"binance_api_key":"DK","binance_secret_key":"DS",
		"llm_base_url":"https://llm.example/v1","llm_model":"gpt-5.4","llm_api_key":"sk-d"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"DK", "DS", "sk-d"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"binance_api":true`) || !strings.Contains(body, `"llm_key":true`) {
		t.Errorf("response lacks presence flags: %s", body)
	}
	st := store.Status()
	if !st.BinAPIKey || !st.BinSecretKey || !st.LLMAPIKey || st.LLMModel != "gpt-5.4" {
		t.Errorf("stored status = %+v", st)
	}

	// A later /api/config reflects the same flags, still without secrets.
	config := fetchBody(t, server, "/api/config")
	for _, secret := range []string{"DK", "DS", "sk-d"} {
		if strings.Contains(config, secret) {
			t.Errorf("/api/config leaked %q: %s", secret, config)
		}
	}
	if !strings.Contains(config, `"llm_key":true`) {
		t.Errorf("/api/config does not report the stored model key: %s", config)
	}
}

// The saved keys must reach the session: a start request after saving uses the
// stored Binance keys instead of failing the exchange-key gate.
func TestDesktopStoredKeysReachSessionStart(t *testing.T) {
	server, _ := newDesktopServer(t)
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")
	t.Setenv("TA_ALLOW_LIVE", "1")
	if rec := postJSON(t, server, "/api/local/credentials",
		`{"binance_api_key":"DK","binance_secret_key":"DS"}`); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	rec := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","execute":true,"confirm":"确认实盘"}`)
	if strings.Contains(rec.Body.String(), "密钥") {
		t.Errorf("stored keys did not pass the key gate: %s", rec.Body.String())
	}
	defer server.Session("").Stop()
}

// A URL without a scheme would be silently prefixed by the HTTP client and
// fail at call time, which reads as "the AI is broken". Reject it at save time.
func TestDesktopRejectsSchemeLessLLMURL(t *testing.T) {
	server, _ := newDesktopServer(t)
	rec := postJSON(t, server, "/api/local/credentials", `{"llm_base_url":"api.openai.com/v1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "http") {
		t.Errorf("error should explain the required scheme: %s", rec.Body.String())
	}
}

// The local endpoint exists only on the desktop build. A server-edition
// process (no LocalCreds) must 404 it, so a network-reachable dashboard can
// never accept an anonymous credential write.
func TestServerEditionHasNoLocalCredentialEndpoint(t *testing.T) {
	server, _ := newTestServer(t)
	rec := postJSON(t, server, "/api/local/credentials", `{"llm_api_key":"sk"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on the server edition", rec.Code)
	}
}

// The desktop live gate: off by default, turned on only through the local
// file, and reported to the page so the switch and the start error agree.
func TestDesktopLiveGateFromLocalFile(t *testing.T) {
	server, store := newDesktopServer(t)
	t.Setenv("TA_ALLOW_LIVE", "")

	body := fetchBody(t, server, "/api/config")
	if !strings.Contains(body, `"live_gate":false`) {
		t.Errorf("fresh desktop reports an open live gate: %s", body)
	}
	rec := postJSON(t, server, "/api/local/credentials", `{"allow_live":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("arm = %d: %s", rec.Code, rec.Body.String())
	}
	if !store.Status().AllowLive {
		t.Error("allow_live was not stored")
	}
	if body := fetchBody(t, server, "/api/config"); !strings.Contains(body, `"live_gate":true`) {
		t.Errorf("live gate not reported after arming: %s", body)
	}
	// Disarming must put it back.
	if rec := postJSON(t, server, "/api/local/credentials", `{"allow_live":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disarm = %d: %s", rec.Code, rec.Body.String())
	}
	if body := fetchBody(t, server, "/api/config"); !strings.Contains(body, `"live_gate":false`) {
		t.Errorf("live gate stayed open after disarming: %s", body)
	}
}

// A credential save must never disturb the live switch, and the switch must
// never disturb the credentials: they are separate requests by design.
func TestDesktopCredentialSaveLeavesLiveGateAlone(t *testing.T) {
	server, store := newDesktopServer(t)
	t.Setenv("TA_ALLOW_LIVE", "")
	if err := store.SetAllowLive(true); err != nil {
		t.Fatal(err)
	}
	if rec := postJSON(t, server, "/api/local/credentials", `{"llm_model":"gpt-5.4"}`); rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	st := store.Status()
	if !st.AllowLive {
		t.Error("saving a credential turned the live switch off")
	}
	if st.LLMModel != "gpt-5.4" {
		t.Errorf("model = %q", st.LLMModel)
	}
}

// The generic auth endpoints stay absent on the desktop: there are no
// accounts, and a 200 here would imply a login that does not exist.
func TestDesktopAuthEndpointsRemainAccountOnly(t *testing.T) {
	server, _ := newDesktopServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("desktop /api/auth/login = %d, want 404", recorder.Code)
	}
}
