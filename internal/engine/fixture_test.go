package engine_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/marketdata"
	"github.com/huijun/trading-agent-go/internal/strategy"
)

// The frozen fixture pins the whole pipeline: indicators, signals, sizing,
// fills, accounting and metrics. A change anywhere shows up here.
//
// If a change is intended, update these numbers; if it is not, it is a bug.
func TestFrozenFixtureReferenceResults(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "bars.csv")
	cases := []struct {
		strategy    string
		trades      int
		returnPct   float64
		maxDrawdown float64
	}{
		{"ma_cross", 46, 5.937481, -22.971434},
		{"rsi_reversion", 11, -0.528973, -9.697502},
		{"breakout", 33, -25.379690, -25.379690},
	}

	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agent.Symbol = "TEST"
			cfg.Strategy.Name = tc.strategy
			cfg.Strategy.Params = map[string]float64{}

			series, err := marketdata.FromCSV(path, "TEST")
			if err != nil {
				t.Fatalf("load fixture: %v", err)
			}
			strat, err := strategy.New(tc.strategy, cfg)
			if err != nil {
				t.Fatalf("build strategy: %v", err)
			}
			result, err := engine.New(cfg, strat).RunBacktest(series)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			if result.Bars != 730 {
				t.Fatalf("bars = %d, want 730", result.Bars)
			}
			if result.Metrics.NumTrades != tc.trades {
				t.Errorf("trades = %d, want %d", result.Metrics.NumTrades, tc.trades)
			}
			if got := value(result.Metrics.TotalReturnPct); math.Abs(got-tc.returnPct) > 0.01 {
				t.Errorf("total return = %v, want %v", got, tc.returnPct)
			}
			if got := value(result.Metrics.MaxDrawdownPct); math.Abs(got-tc.maxDrawdown) > 0.01 {
				t.Errorf("max drawdown = %v, want %v", got, tc.maxDrawdown)
			}
		})
	}
}

func value(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}
