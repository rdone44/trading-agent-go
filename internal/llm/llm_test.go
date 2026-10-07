package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// stubServer stands in for an OpenAI-compatible /chat/completions endpoint.
// The caller picks the assistant content each request receives.
func stubServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		// Echo back a minimal but valid completion for the requested content.
		out := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func testClient(t *testing.T, content string) *Client {
	t.Helper()
	srv := stubServer(t, content)
	t.Cleanup(srv.Close)
	cfg := config.LLM{BaseURL: srv.URL}
	c := New(cfg)
	c.APIKey = "test-key" // bypass env resolution; Enabled() only checks this.
	return c
}

func TestCompleteReturnsAssistantContent(t *testing.T) {
	c := testClient(t, "hello from the model")
	got, err := c.Complete("system", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "hello from the model" {
		t.Fatalf("Complete = %q, want the stubbed content", got)
	}
}

func TestCompleteRequiresKey(t *testing.T) {
	cfg := config.LLM{BaseURL: "http://127.0.0.1:0"}
	c := New(cfg) // no key
	if _, err := c.Complete("s", "u"); err == nil {
		t.Fatal("Complete with no key: want error, got nil")
	}
}

func TestCompleteJSONUnwrapsFencedAnswer(t *testing.T) {
	// Models often wrap JSON in markdown fences; the client must find the object.
	c := testClient(t, "```json\n{\"approve\": true, \"reason\": \"ok\"}\n```")
	var out struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := c.CompleteJSON("system", "user", &out); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if !out.Approve || out.Reason != "ok" {
		t.Fatalf("parsed %+v, want approve=true reason=ok", out)
	}
}

func TestCompleteJSONFailsOnNoKey(t *testing.T) {
	c := New(config.LLM{})
	if err := c.CompleteJSON("s", "u", &map[string]any{}); err == nil {
		t.Fatal("CompleteJSON with no key: want error, got nil")
	}
}

// Veto is fail-open: a missing key, a transport error, or an unparseable
// answer all let the entry through (blocked=false). Only an explicit model
// refusal (approve=false) blocks.
func TestVetoFailOpenWithoutKey(t *testing.T) {
	blocked, _ := VetoDecision(config.LLM{}, VetoRequest{})
	if blocked {
		t.Fatal("veto with no key must fail open (blocked=false)")
	}
}

func TestVetoApprovePasses(t *testing.T) {
	blocked, reason := VetoDecision(cfgPointingAt(t, `{"approve": true, "reason": "fine"}`), VetoRequest{Side: "buy"})
	if blocked || reason != "" {
		t.Fatalf("approved entry blocked=%v reason=%q, want pass", blocked, reason)
	}
}

func TestVetoRefusalBlocks(t *testing.T) {
	blocked, reason := VetoDecision(cfgPointingAt(t, `{"approve": false, "reason": "too much size"}`), VetoRequest{Side: "buy"})
	if !blocked {
		t.Fatal("model refused (approve=false) but entry was not blocked")
	}
	if reason == "" {
		t.Fatal("a blocking refusal should carry a reason")
	}
}

func TestVetoGarbageAnswerFailsOpen(t *testing.T) {
	blocked, _ := VetoDecision(cfgPointingAt(t, "not json at all"), VetoRequest{})
	if blocked {
		t.Fatal("unparseable veto answer must fail open (blocked=false)")
	}
}

// Review errors when there is no key; otherwise it returns the model text.
func TestReviewWithoutKeyErrors(t *testing.T) {
	if _, err := Review(config.LLM{}, "facts"); err == nil {
		t.Fatal("Review with no key: want error, got nil")
	}
}

func TestReviewReturnsModelText(t *testing.T) {
	got, err := Review(cfgPointingAt(t, "模型复盘正文"), "facts")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got != "模型复盘正文" {
		t.Fatalf("Review = %q", got)
	}
}

// Propose: fail-loud without a key; parses params when present; rejects NaN.
func TestProposeWithoutKeyErrors(t *testing.T) {
	if _, err := Propose(config.LLM{}, "sharpe", "rsi_reversion", map[string]float64{}, "m"); err == nil {
		t.Fatal("Propose with no key: want error, got nil")
	}
}

func TestProposeParsesParams(t *testing.T) {
	content := `{"params": {"period": 21, "lower": 25}, "rationale": "wider RSI"}`
	p, err := Propose(cfgPointingAt(t, content), "sharpe", "rsi_reversion", map[string]float64{}, "m")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := map[string]float64{"period": 21, "lower": 25}
	if !reflect.DeepEqual(p.Params, want) {
		t.Fatalf("params = %v, want %v", p.Params, want)
	}
	if p.Rationale != "wider RSI" {
		t.Fatalf("rationale = %q", p.Rationale)
	}
}

func TestProposeRejectsNaN(t *testing.T) {
	content := `{"params": {"period": 0}, "rationale": ""}`
	// 0 is finite; the NaN guard is exercised by feeding a non-numeric token.
	content = `{"params": {"period": "NaN"}, "rationale": ""}`
	if _, err := Propose(cfgPointingAt(t, content), "sharpe", "rsi", map[string]float64{}, "m"); err == nil {
		t.Fatal("Propose with a non-numeric param: want error, got nil")
	}
}

// cfgPointingAt returns a config whose LLM points at a stub server carrying the
// given assistant content, with the key set — so the package-level functions
// (which build their own client) reach a live endpoint.
func cfgPointingAt(t *testing.T, content string) config.LLM {
	t.Helper()
	srv := stubServer(t, content)
	t.Cleanup(srv.Close)
	// The package functions call New(cfg) and then read config.LLMAPIKey();
	// set the env so the resolved key is non-empty, matching production flow.
	t.Setenv("LLM_API_KEY", "test-key")
	return config.LLM{BaseURL: srv.URL}
}

// TestExtractJSONBalance covers the bracket matcher directly.
func TestExtractJSONBalance(t *testing.T) {
	cases := map[string]string{
		`prefix {"a":1} suffix`: `{"a":1}`,
		`{"a":{"b":2}}`:         `{"a":{"b":2}}`,
		`no braces`:             "",
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			// json round-trip for the object cases; string compare is fine here.
			_ = fmt.Sprint(got)
			t.Fatalf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}
