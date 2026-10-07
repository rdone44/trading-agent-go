package tune

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

func TestCVSplit(t *testing.T) {
	series := testfx.Bars("TEST", 303, 42, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	training, windows, err := splitCV(series, 3, 30)
	if err != nil {
		t.Fatal(err)
	}
	if training.Len() != 60 || len(windows) != 4 {
		t.Fatalf("split: training=%d windows=%v", training.Len(), windows)
	}
	next := training.Len()
	for i, w := range windows {
		if w.Start != next || w.End <= w.Start || w.Holdout != (i == len(windows)-1) {
			t.Fatalf("non-disjoint split: %+v", windows)
		}
		next = w.End
	}
	if next != series.Len() {
		t.Fatal("lost remainder")
	}
	for _, folds := range []int{-1, 33, 32} {
		if _, _, err := splitCV(series, folds, 30); err == nil {
			t.Fatalf("folds %d should fail", folds)
		}
	}
	if _, _, err := splitCV(model.Series{}, 1, 0); err == nil {
		t.Fatal("empty series accepted")
	}
}

func TestCVMajorityAndHoldout(t *testing.T) {
	windows := []WindowResult{{Valid: true, Passed: true}, {Valid: true, Passed: true}, {Valid: true, Passed: false}, {Holdout: true, Valid: true, Passed: true}}
	if !acceptsValidation(windows) {
		t.Fatal("majority with holdout should pass")
	}
	windows[1].Passed = false
	if acceptsValidation(windows) {
		t.Fatal("minority accepted")
	}
	windows[1].Passed = true
	windows[3].Passed = false
	if acceptsValidation(windows) {
		t.Fatal("failed holdout accepted")
	}
	windows[3].Passed = true
	windows[3].Valid = false
	if acceptsValidation(windows) {
		t.Fatal("undefined holdout accepted")
	}
	if acceptsValidation(windows[:3]) {
		t.Fatal("missing holdout accepted")
	}
}

func TestCVRunOfflineAndLegacyJSON(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")
	cfg := baseTestConfig()
	cfg.Agent.HistoryDays = 600
	cfg.Strategy.Params = map[string]float64{"fast": 10, "slow": 30, "min_gap_pct": 0}
	calls := 0
	cfg.LLM.BaseURL = modelStub(t, `{"params":{"fast":10,"slow":30,"min_gap_pct":0},"rationale":"unchanged"}`, &calls).URL
	series := testfx.Bars("TEST", 600, 42, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	opts := Options{Objective: "total_return", Rounds: 1}
	legacy, err := Run(cfg, series, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.CVFolds = 0
	explicit, err := Run(cfg, series, opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(legacy)
	b, _ := json.Marshal(explicit)
	if !bytes.Equal(a, b) || bytes.Contains(a, []byte("validation")) {
		t.Fatalf("legacy JSON changed: %s / %s", a, b)
	}
	opts.CVFolds = 3
	report, err := Run(cfg, series, opts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("offline model calls=%d, want 3", calls)
	}
	if report.Validation == nil || !report.Validation.Accepted || len(report.Validation.Windows) != 4 {
		t.Fatalf("validation: %+v", report.Validation)
	}
	for _, w := range report.Validation.Windows {
		if !w.Valid || !w.Passed || w.Baseline != w.Winner {
			t.Fatalf("equal parameters: %+v", w)
		}
	}
	if !reflect.DeepEqual(report.BestParams, legacy.BestParams) {
		t.Fatal("equal proposal changed winner")
	}
}

func TestCVUndefinedMetricRejectsWinner(t *testing.T) {
	cfg := baseTestConfig()
	series := testfx.Bars("TEST", 600, 42, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, windows, err := splitCV(series, 3, 30)
	if err != nil {
		t.Fatal(err)
	}
	// Prevent all entries: profit factor is undefined rather than a tie.
	cfg.Backtest.WarmupBars = 1000
	flat := map[string]float64{"fast": 200, "slow": 500, "min_gap_pct": 5}
	report := validateWinner(cfg, series, windows, flat, flat, "profit_factor")
	if report.Accepted {
		t.Fatal("undefined objective accepted")
	}
	for _, w := range report.Windows {
		if w.Valid || w.Passed {
			t.Fatalf("undefined score %+v", w)
		}
	}
}
