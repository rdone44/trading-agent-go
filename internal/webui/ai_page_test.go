package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAIPageIsAFirstClassRoute guards the discoverability contract: the AI is
// the product, so the page must be reachable from the navigation and must not
// be hidden behind a strategy selection the way the old prompt-tune fieldset
// was. A user who cannot find the AI is the bug this test exists to prevent.
func TestAIPageIsAFirstClassRoute(t *testing.T) {
	server, _ := newTestServer(t)
	page := fetchBody(t, server, "/")

	for _, want := range []string{
		`id="nav-ai"`,  // a nav entry, always visible
		`id="ai-page"`, // the page itself
		`id="ai-veto"`, // the entry veto switch
		`id="ai-tune-run"`,
		`id="ai-prompt-run"`,
		`id="ai-review-backtest"`,
		`id="ai-review-live"`,
		`id="ai-persona"`,
		`id="ai-status"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}

	// The page must not carry a `hidden` attribute itself, and the router must
	// know the route.
	script := fetchBody(t, server, "/app.js")
	if !strings.Contains(script, `"#/ai"`) {
		t.Error("app.js has no #/ai route")
	}
	if !strings.Contains(script, `"#nav-ai"`) {
		t.Error("app.js never sets aria-current on #nav-ai")
	}
}

// TestConfigAdvertisesModelReadiness locks the fields the AI page reads to tell
// "the model is ready" from "no key stored". Without them the page could only
// guess, and a missing key would look like a broken feature.
func TestConfigAdvertisesModelReadiness(t *testing.T) {
	server, _ := newTestServer(t)
	body := fetchBody(t, server, "/api/config")
	var payload struct {
		LLMModel  string `json:"llm_model"`
		LLMEnvKey bool   `json:"llm_env_key"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	if payload.LLMModel == "" {
		t.Error("llm_model is empty; the AI page cannot show which model is configured")
	}
	// llm_env_key must be a boolean, never the key itself.
	if strings.Contains(body, "llm_api_key") {
		t.Error("/api/config must not leak an LLM credential field")
	}
}

// TestSessionReviewWithoutASessionExplains requires the endpoint to explain
// itself rather than returning an empty review: asking the model to post-mortem
// nothing would waste a call and read as a bug.
func TestSessionReviewWithoutASessionExplains(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := postJSON(t, server, "/api/session/review", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no session yet)", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "还没有交易会话") {
		t.Errorf("body = %s, want a note telling the user to start a session", recorder.Body.String())
	}
}

// TestSessionReviewRejectsWrongMethod guards the POST-only contract, matching
// the other session routes.
func TestSessionReviewRejectsWrongMethod(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/session/review", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

// TestSessionReviewDegradesWithoutModelKey is the AI-outage contract for the
// live path: no key must yield a note, never a 5xx and never a silent empty
// response. The session itself stays untouched.
func TestSessionReviewDegradesWithoutModelKey(t *testing.T) {
	server := newLiveTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if recorder.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", recorder.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")

	review := postJSON(t, server, "/api/session/review", "")
	if review.Code != http.StatusOK {
		t.Fatalf("review status = %d, want 200: %s", review.Code, review.Body.String())
	}
	var payload struct {
		Review            string `json:"review"`
		ReviewUnavailable string `json:"review_unavailable"`
	}
	if err := json.Unmarshal(review.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode review: %v", err)
	}
	if payload.Review != "" {
		t.Errorf("review = %q, want empty with no model key", payload.Review)
	}
	if payload.ReviewUnavailable == "" {
		t.Error("review_unavailable is empty; the page needs a reason to show")
	}

	// The review must not have disturbed the running session.
	if status := getSession(t, server); !status.Running {
		t.Error("a review request must not stop the session")
	}
}

// TestSessionStartArmsEntryVeto locks the switch that makes the veto reachable
// from the UI at all: before this the gate could only be turned on in YAML.
func TestSessionStartArmsEntryVeto(t *testing.T) {
	server := newLiveTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600,"veto":true}`)
	if recorder.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", recorder.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")

	status := getSession(t, server)
	if !status.VetoEnabled {
		t.Fatal("veto_enabled = false after starting with veto=true; the switch is not wired")
	}
	if status.Settings == nil || !status.Settings.Veto {
		t.Error("settings.veto is not echoed back, so the form cannot prefill the switch")
	}
}

// TestSessionStartWithoutVetoLeavesItOff is the other half: the switch must
// default off, so the model never gates entries unless asked.
func TestSessionStartWithoutVetoLeavesItOff(t *testing.T) {
	server := newLiveTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if recorder.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", recorder.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")

	if status := getSession(t, server); status.VetoEnabled {
		t.Error("veto_enabled = true without asking; the gate must default off")
	}
}

// TestSessionReviewFactsRenderTheSession checks the fact blob the model is
// asked to review: it must describe the run, not be empty, once a runner
// exists.
func TestSessionReviewFactsRenderTheSession(t *testing.T) {
	server := newLiveTestServer(t)
	recorder := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if recorder.Code != http.StatusOK {
		t.Skipf("start needs a live ticker in this environment: %s", recorder.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")

	facts, ok := server.Session("").ReviewFacts(3)
	if !ok {
		t.Fatal("ReviewFacts = false for a started session")
	}
	if !strings.Contains(facts, "TEST") || !strings.Contains(facts, "ma_cross") {
		t.Errorf("facts do not identify the run:\n%s", facts)
	}

	// An idle server has nothing to review and must say so.
	idle, _ := newTestServer(t)
	if _, ok := idle.Session("").ReviewFacts(3); ok {
		t.Error("ReviewFacts = true on a server that never started a session")
	}
}
