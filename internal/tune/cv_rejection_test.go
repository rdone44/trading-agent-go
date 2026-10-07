package tune

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

func TestCVRejectedWinnerRestoresBaseline(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")
	cfg := baseTestConfig()
	cfg.Strategy.Params = map[string]float64{"fast": 10, "slow": 30, "min_gap_pct": 0}
	training := testfx.Bars("TEST", 300, 42, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	evaluate := func(p map[string]float64) float64 {
		clone := cfg
		clone.Strategy.Params = p
		strat, err := strategy.New(clone.Strategy.Name, clone)
		if err != nil {
			t.Fatal(err)
		}
		res, err := engine.New(clone, strat).RunBacktest(training)
		if err != nil {
			t.Fatal(err)
		}
		value, ok := ObjectiveValue(res.Metrics, "profit_factor")
		if !ok {
			return -1
		}
		return value
	}
	base := evaluate(cfg.Strategy.Params)
	var candidate map[string]float64
	for _, fast := range []float64{3, 5, 8, 15, 20} {
		for _, slow := range []float64{20, 24, 40, 60, 90} {
			if fast >= slow {
				continue
			}
			p := map[string]float64{"fast": fast, "slow": slow, "min_gap_pct": 0}
			if evaluate(p) > base {
				candidate = p
				break
			}
		}
		if candidate != nil {
			break
		}
	}
	if candidate == nil {
		t.Fatal("fixture has no improved training candidate")
	}
	// Flat unseen windows have no trades and undefined profit factor.
	series := training
	bars := append([]model.Bar(nil), training.Bars...)
	last := bars[len(bars)-1]
	for i := 0; i < 1200; i++ {
		last.Time = last.Time.Add(24 * time.Hour)
		last.Open = last.Close
		last.High = last.Close
		last.Low = last.Close
		bars = append(bars, last)
	}
	series.Bars = bars
	proposal, err := json.Marshal(map[string]any{"params": candidate, "rationale": "training winner"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	cfg.LLM.BaseURL = modelStub(t, string(proposal), &calls).URL
	report, err := Run(cfg, series, Options{Objective: "profit_factor", Rounds: 1, CVFolds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || report.Validation == nil || report.Validation.Accepted {
		t.Fatalf("report: %+v", report)
	}
	if report.Rounds[1].ObjectiveValue <= report.Baseline {
		t.Fatal("fixture did not improve training score")
	}
	if report.BestValue != report.Baseline || !reflect.DeepEqual(report.BestParams, report.Rounds[0].Params) || report.Rounds[1].Improved {
		t.Fatalf("rejected winner not restored: %+v", report)
	}
}
