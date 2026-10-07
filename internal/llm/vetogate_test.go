package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// countingStub returns an OpenAI-compatible stub that counts endpoint hits
// and always answers approve=true. It accepts any POST path so a base_url
// with or without a /v1 prefix both work.
func countingStub(t *testing.T, hits *int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"approve": true, "reason": "ok"}`}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func cfgForStub(base string, cacheSec int) config.LLM {
	return config.LLM{BaseURL: base, VetoCacheSec: cacheSec}
}

func TestGateReusesVerdictWithinTTL(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key")
	var hits int32
	gate := NewVetoGate(cfgForStub(countingStub(t, &hits), 60))

	req := VetoRequest{Symbol: "BTCUSDT", Side: "buy", Quantity: 1.5}
	for i := 0; i < 5; i++ {
		if b, _ := gate.Decide(req); b {
			t.Fatalf("iteration %d: expected approve, got blocked", i)
		}
	}
	// Five identical proposals within the 60s TTL bill the model once.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("model hit %d times for 5 identical proposals, want 1", got)
	}
}

func TestGateRefetchesWhenProposalChanges(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key")
	var hits int32
	gate := NewVetoGate(cfgForStub(countingStub(t, &hits), 60))

	a := VetoRequest{Symbol: "BTCUSDT", Side: "buy", Quantity: 1.0}
	b := VetoRequest{Symbol: "BTCUSDT", Side: "buy", Quantity: 2.0} // different size
	gate.Decide(a)
	gate.Decide(b)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("model hit %d times for two distinct proposals, want 2", got)
	}
}

func TestGateWithDisabledCacheHitsEveryTime(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key")
	var hits int32
	gate := NewVetoGate(cfgForStub(countingStub(t, &hits), 0)) // ttl 0 disables caching
	req := VetoRequest{Symbol: "BTCUSDT", Side: "buy", Quantity: 1.0}
	for i := 0; i < 3; i++ {
		gate.Decide(req)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("cache disabled (ttl=0): model hit %d times for 3 calls, want 3", got)
	}
}

func TestGateRefetchesWhenVerdictExpires(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key")
	var hits int32
	gate := NewVetoGate(cfgForStub(countingStub(t, &hits), 1)) // 1s TTL
	req := VetoRequest{Symbol: "BTCUSDT", Side: "buy", Quantity: 1.0}

	gate.Decide(req)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("first decision should hit the model once (got %d)", got)
	}
	time.Sleep(1100 * time.Millisecond)
	gate.Decide(req)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("after TTL expiry the model must be hit again (got %d)", got)
	}
}
