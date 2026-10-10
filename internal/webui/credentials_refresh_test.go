package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/testfx"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// TestSavingCredentialsRefreshesRunningSession is the end-to-end regression
// test for "切换模型有问题": a running session used to keep its startup
// config, so changing the model in the AI page did nothing until a restart.
// A fake model server records the model name it is asked with, so the test
// proves the refresh reached the decision path (not just the status view):
// before the save the server is asked with the startup model, after the save
// the very next cycle asks with the new one — while the session itself never
// stopped and kept its start-time settings (days).
func TestSavingCredentialsRefreshesRunningSession(t *testing.T) {
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"position\":0,\"stop\":null,\"target\":null,\"reason\":\"等待信号\",\"confidence\":0.5}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	server, vault := newAuthServer(t)
	server.SeriesLoader = fxLoader
	cookie := register(t, server, "trader", "s3cret-pass")
	// The session is keyed by account username in vault mode; the offline
	// price loader must ride on THAT session, not the anonymous one.
	server.Session("trader").SeriesLoader = fxLoader
	server.Session("trader").PriceLoader = func(symbol string) (float64, time.Time, error) {
		series := testfx.Bars(symbol, 5, 42, time.Now().UTC())
		last := series.Len() - 1
		return series.Bars[last].Close, series.Bars[last].Time, nil
	}
	// The account starts with the old model stored in the vault; the session
	// picks it up at start, exactly like a real first save.
	if err := vault.SetCredentials("trader", "", "", srv.URL, "old-model", "", "k1"); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	start := postJSONCookie(t, server, "/api/session/start", map[string]any{
		"symbol": "TEST", "strategy": "llm", "days": 120,
		"interval_seconds": 3600, "initial_cash": 10000,
	}, cookie)
	if start.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", start.Code, start.Body.String())
	}
	defer postJSONCookie(t, server, "/api/session/stop", "", cookie)

	if rec := postJSONCookie(t, server, "/api/session/step", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("step = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(models) == 0 || models[len(models)-1] != "old-model" {
		mu.Unlock()
		t.Fatalf("model requests before save = %v, want the startup model old-model", models)
	}
	mu.Unlock()

	save := postJSONCookie(t, server, "/api/auth/credentials", map[string]any{
		"llm_model": "new-model", "llm_api_key": "k1",
	}, cookie)
	if save.Code != http.StatusOK {
		t.Fatalf("credentials save = %d: %s", save.Code, save.Body.String())
	}

	status := sessionStatusWithCookie(t, server, cookie)
	if status.AIModel != "new-model" {
		t.Errorf("ai_model after save = %q, want new-model (running session must see it without a restart)", status.AIModel)
	}
	// The refresh is a credential merge, not a restart: start-time settings survive.
	if status.Settings.Days != 120 {
		t.Errorf("days after save = %d, want 120 (session start-time settings must survive a credential save)", status.Settings.Days)
	}

	mu.Lock()
	before := len(models)
	mu.Unlock()
	if rec := postJSONCookie(t, server, "/api/session/step", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("step after save = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if before >= len(models) {
		t.Fatal("the cycle after the save never reached the model server")
	}
	if got := models[len(models)-1]; got != "new-model" {
		t.Errorf("model request after save = %q, want new-model (the next cycle must use the saved model)", got)
	}
}

// sessionStatusWithCookie polls /api/session with the account cookie: with
// the vault enabled the session routes are guarded, so the poll must
// authenticate.
func sessionStatusWithCookie(t *testing.T, server *webui.Server, cookie string) webui.SessionStatus {
	t.Helper()
	rec := getWithCookie(t, server, "/api/session", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/session = %d, want 200", rec.Code)
	}
	var status webui.SessionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return status
}

// getWithCookie issues a GET carrying the session cookie.
func getWithCookie(t *testing.T, server *webui.Server, path, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}
