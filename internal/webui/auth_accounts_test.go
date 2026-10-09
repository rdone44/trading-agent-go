package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/auth"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// newAuthServer is a dashboard with the account vault enabled: an offline
// server (as newTestServer builds) plus a credential store, so the guarded
// routes and the key-injection path can be exercised without any exchange or
// model calls.
func newAuthServer(t *testing.T) (*webui.Server, *auth.Service) {
	t.Helper()
	server, dir := newTestServer(t)
	vault, err := auth.New(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	server.Auth = vault
	return server, vault
}

// register opens a session for the account and returns the session cookie.
func register(t *testing.T, server *webui.Server, username, password string) string {
	t.Helper()
	rec := postJSONCookie(t, server, "/api/auth/register", map[string]string{"username": username, "password": password}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register %q = %d: %s", username, rec.Code, rec.Body.String())
	}
	return sessionCookie(rec)
}

func postJSONCookie(t *testing.T, server *webui.Server, path string, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ta_session" {
			return c.Value
		}
	}
	return ""
}

// With the vault enabled the credential-bearing routes must 401 when no
// session is present, while the public routes (config, symbols, healthz) stay
// open so the login card can render.
func TestAuthGuardsSessionRoutes(t *testing.T) {
	server, _ := newAuthServer(t)

	for _, path := range []string{"/api/session", "/api/runs", "/api/run"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want 401", path, rec.Code)
		}
	}

	for _, path := range []string{"/api/config", "/api/symbols", "/healthz"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s public = %d, want 200", path, rec.Code)
		}
	}
}

// The account lifecycle: register, session verify, store credentials, and a
// status that exposes presence flags but never the secret values.
func TestAuthLifecycle(t *testing.T) {
	server, _ := newAuthServer(t)
	cookie := register(t, server, "trader", "s3cret-pass")
	if cookie == "" {
		t.Fatal("no session cookie on register")
	}

	// /api/auth/me with the cookie reports the account and empty slots.
	rec := postJSONCookie(t, server, "/api/auth/me", map[string]string{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	var me struct {
		Username      string `json:"username"`
		BinanceAPI    bool   `json:"binance_api"`
		BinanceSecret bool   `json:"binance_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Username != "trader" || me.BinanceAPI {
		t.Errorf("me = %+v, want trader with no credentials yet", me)
	}

	// Anonymous /me is a 401.
	if rec := postJSONCookie(t, server, "/api/auth/me", map[string]string{}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous me = %d, want 401", rec.Code)
	}

	// Store credentials; the response carries presence flags only.
	rec = postJSONCookie(t, server, "/api/auth/credentials", map[string]string{
		"binance_api_key": "VK", "binance_secret_key": "VS",
		"llm_base_url": "https://llm.example/v1", "llm_api_key": "sk-x",
	}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("credentials = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"binance_api":true`) || !strings.Contains(body, `"llm_key":true`) {
		t.Errorf("credentials response lacks presence flags: %s", body)
	}
	// The secret values must never appear; the LLM Base URL is public state
	// (the frontend displays "which AI endpoint is configured").
	for _, secret := range []string{"VK", "VS", "sk-x"} {
		if strings.Contains(body, secret) {
			t.Errorf("credentials response leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "llm.example") {
		t.Errorf("credentials response should expose the LLM base url: %s", body)
	}

	// A wrong password must not open a session.
	rec = postJSONCookie(t, server, "/api/auth/login", map[string]string{"username": "trader", "password": "wrong"}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login wrong password = %d, want 401", rec.Code)
	}
	// A correct one does.
	if rec := postJSONCookie(t, server, "/api/auth/login", map[string]string{"username": "trader", "password": "s3cret-pass"}, ""); rec.Code != http.StatusOK {
		t.Errorf("login correct = %d, want 200", rec.Code)
	}
}

// The whole point of the vault: execute=true must get past the key gate using
// the stored credentials, with no environment keys set. The start will still
// fail later (the offline fixture cannot reconcile a real exchange), but it
// must NOT fail with the "missing BINANCE_API_KEY" message — that only
// appears when neither the vault nor the environment has keys.
func TestStoredCredentialsPassKeyGate(t *testing.T) {
	server, _ := newAuthServer(t)
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")
	t.Setenv("TA_ALLOW_LIVE", "1")

	cookie := register(t, server, "live", "s3cret-pass")
	if rec := postJSONCookie(t, server, "/api/auth/credentials", map[string]string{
		"binance_api_key": "VK", "binance_secret_key": "VS",
	}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("set credentials = %d: %s", rec.Code, rec.Body.String())
	}

	rec := postJSONCookie(t, server, "/api/session/start", map[string]any{
		"symbol": "TEST", "execute": true, "confirm": "确认实盘",
	}, cookie)
	out := rec.Body.String()
	if strings.Contains(out, "BINANCE_API_KEY 与 BINANCE_SECRET_KEY") {
		t.Errorf("key gate still blocked with stored credentials: %s", out)
	}

	// Control: a fresh keyless account on the same server must hit the gate.
	cookie2 := register(t, server, "nokeys", "s3cret-pass")
	rec2 := postJSONCookie(t, server, "/api/session/start", map[string]any{
		"symbol": "TEST", "execute": true, "confirm": "确认实盘",
	}, cookie2)
	if !strings.Contains(rec2.Body.String(), "密钥") {
		t.Errorf("expected the key-gate message for a keyless user, got: %s", rec2.Body.String())
	}
}

// Accounts mode must not let a caller choose the ledger path: an arbitrary
// path could point a session at another account's state file. The server
// assigns the path itself, under the account's own directory.
func TestAccountsRejectCustomStatePath(t *testing.T) {
	server, _ := newAuthServer(t)
	cookie := register(t, server, "paths", "s3cret-pass")

	rec := postJSONCookie(t, server, "/api/session/start", map[string]any{
		"symbol": "TEST", "state_path": filepath.Join(server.OutputDir, "other-user.json"),
	}, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "状态文件路径") {
		t.Fatalf("custom state_path = %d %s, want a 400 rejection", rec.Code, rec.Body.String())
	}

	// Without the override the server assigns an account-scoped path.
	if rec := postJSONCookie(t, server, "/api/session/start",
		map[string]any{"symbol": "TEST", "interval_seconds": 3600}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("start without state_path = %d: %s", rec.Code, rec.Body.String())
	}
	defer server.Session("paths").Stop()
	want := filepath.Join(server.OutputDir, "accounts", "paths", "sessions", "paper-futures-TEST.json")
	if got := getSessionAs(t, server, cookie).StatePath; got != want {
		t.Fatalf("state path = %q, want the account-scoped %q", got, want)
	}
}

// The historical build (no vault) keeps every route open and the auth
// endpoints simply absent.
func TestAuthDisabledHistorical(t *testing.T) {
	server, _ := newTestServer(t)
	for _, path := range []string{"/api/session", "/api/runs", "/healthz"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (auth disabled)", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"username":"xx","password":"yyyyyy"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/api/auth/register with auth disabled = %d, want 404", rec.Code)
	}
	// /api/config reports auth as disabled so the login card can hide.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var view struct {
		Auth struct {
			Enabled bool `json:"enabled"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Auth.Enabled {
		t.Error("auth.enabled should be false when the vault is not set")
	}
}
