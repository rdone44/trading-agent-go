package tune

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/metrics"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/testfx"
)

func ptrf(v float64) *float64 { return &v }

func TestObjectiveValue(t *testing.T) {
	m := metrics.Metrics{
		Sharpe: ptrf(1.5), Sortino: ptrf(2.0), TotalReturnPct: ptrf(42.0),
		ProfitFactor: ptrf(1.3), WinRatePct: ptrf(61.0),
	}
	cases := map[string]float64{
		"sharpe": 1.5, "sortino": 2.0, "total_return": 42.0,
		"profit_factor": 1.3, "win_rate": 61.0,
	}
	for name, want := range cases {
		got, ok := ObjectiveValue(m, name)
		if !ok {
			t.Fatalf("ObjectiveValue(%q) ok=false, want %v", name, want)
		}
		if got != want {
			t.Fatalf("ObjectiveValue(%q) = %v, want %v", name, got, want)
		}
	}
	if _, ok := ObjectiveValue(m, "nonsense"); ok {
		t.Fatal("ObjectiveValue(unknown) must be ok=false")
	}
}

func TestObjectiveValueNilMetric(t *testing.T) {
	if _, ok := ObjectiveValue(metrics.Metrics{}, "sharpe"); ok {
		t.Fatal("ObjectiveValue with a null metric must be ok=false")
	}
}

func TestFormatParamsIsDeterministic(t *testing.T) {
	params := map[string]float64{"slow": 30, "fast": 10, "min_gap_pct": 0}
	got := FormatParams(params)
	want := "fast=10 min_gap_pct=0 slow=30"
	if got != want {
		t.Fatalf("FormatParams = %q, want %q (sorted by key)", got, want)
	}
	if FormatParams(map[string]float64{}) != "" {
		t.Fatal("FormatParams(empty) must be an empty string")
	}
}

func TestMetricsSummaryRendersNulls(t *testing.T) {
	s := MetricsSummary(metrics.Metrics{})
	for _, token := range []string{"total=n/a", "sharpe=n/a", "trades=0"} {
		if !contains(s, token) {
			t.Fatalf("MetricsSummary(all-null) missing %q:\n%s", token, s)
		}
	}
}

// modelStub stands in for an OpenAI-compatible /chat/completions endpoint and
// returns the same proposal for every call; it counts the calls it serves.
func modelStub(t *testing.T, proposalJSON string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		*calls++
		out := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": proposalJSON}},
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseTestConfig() config.Config {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Agent.HistoryDays = 300
	cfg.Backtest.WarmupBars = 30
	cfg.Strategy.Name = "ma_cross"
	cfg.Strategy.Params = map[string]float64{}
	return cfg
}

func deterministicSeries(cfg config.Config) model.Series {
	return testfx.Bars(cfg.Agent.Symbol, cfg.Agent.HistoryDays, 42, time.Now().UTC())
}

// TestRunClampsOutOfRangeProposals verifies the guardrail: a wild model
// suggestion (fast=999, slow=999, min_gap_pct=50) is clamped into ma_cross's
// legal ranges (fast<=200, slow<=500, min_gap_pct<=5) before the backtest.
func TestRunClampsOutOfRangeProposals(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")

	var calls int
	proposal := `{"params":{"fast":999,"slow":999,"min_gap_pct":50},"rationale":"clamp test"}`
	cfg := baseTestConfig()
	cfg.LLM.BaseURL = modelStub(t, proposal, &calls).URL

	report, err := Run(cfg, deterministicSeries(cfg), Options{
		Objective: "total_return",
		Rounds:    1,
		Stall:     0,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model called %d times, want 1", calls)
	}
	// The only proposal round must carry the clamped values, not the raw ones.
	if len(report.Rounds) < 2 {
		t.Fatalf("rounds = %d, want baseline + proposal", len(report.Rounds))
	}
	got := report.Rounds[1].Params
	if got["fast"] != 200 || got["slow"] != 500 || got["min_gap_pct"] != 5 {
		t.Fatalf("clamped proposal = %v, want fast=200 slow=500 min_gap_pct=5", got)
	}
	if report.LLMEnabled != true {
		t.Fatal("expected an enabled model in this scenario")
	}
}

// TestRunNoModelRunsBaselineOnly verifies the degraded path: with no key the
// loop still runs the baseline, reports the model as disabled, and stops with
// a "skipped" round instead of calling anything.
func TestRunNoModelRunsBaselineOnly(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	cfg := baseTestConfig()
	report, err := Run(cfg, deterministicSeries(cfg), Options{
		Objective: "total_return",
		Rounds:    3,
		Stall:     3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.LLMEnabled {
		t.Fatal("model must be disabled when no key is set")
	}
	// Baseline plus a single skipped-round note; no proposal was backtested.
	if len(report.Rounds) != 2 || report.Rounds[1].Note == "" {
		t.Fatalf("expected baseline + skipped note, got %+v", report.Rounds)
	}
	if report.BestParams == nil {
		t.Fatal("best params must fall back to the baseline params")
	}
}

// TestRunStallGuardStopsEarly verifies the stall guard fires: with a model
// that proposes the same (non-improving) params every round, the loop stops
// after Stall consecutive no-improvement rounds, not after all of Rounds.
func TestRunStallGuardStopsEarly(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")

	var calls int
	// Every proposal equals the current params, so no round can improve.
	proposal := `{"params":{"fast":10,"slow":30,"min_gap_pct":0},"rationale":"no change"}`
	cfg := baseTestConfig()
	cfg.Strategy.Params = map[string]float64{"fast": 10, "slow": 30, "min_gap_pct": 0}
	cfg.LLM.BaseURL = modelStub(t, proposal, &calls).URL

	report, err := Run(cfg, deterministicSeries(cfg), Options{
		Objective: "total_return",
		Rounds:    6,
		Stall:     2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.EarlyStopped {
		t.Fatal("expected the stall guard to stop the loop early")
	}
	// Two non-improving rounds (Stall=2) then a stop marker: 1 baseline + 2
	// rounds + 1 marker = 4 entries. The model is called once per actual
	// proposal round, so 2 calls, not the 6 that were requested.
	if calls != 2 {
		t.Fatalf("model called %d times, want 2 (stall guard)", calls)
	}
}

// contains avoids pulling strings into the import list for one check.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
