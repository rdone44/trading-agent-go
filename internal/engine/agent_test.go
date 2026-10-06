package engine_test

import (
	"math"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/strategy"
	"github.com/huijun/trading-agent-go/internal/testfx"
)

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Agent.HistoryDays = 400
	cfg.Backtest.WarmupBars = 40
	return cfg
}

func run(t *testing.T, cfg config.Config) engine.Result {
	t.Helper()
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatalf("build strategy: %v", err)
	}
	series := testfx.Bars(cfg.Agent.Symbol, cfg.Agent.HistoryDays, 42, time.Now().UTC())
	agent := engine.New(cfg, strat)
	result, err := agent.RunBacktest(series)
	if err != nil {
		t.Fatalf("run backtest: %v", err)
	}
	return result
}

func TestBarsAreDeterministic(t *testing.T) {
	first := testfx.Bars("TEST", 100, 7, time.Now().UTC())
	second := testfx.Bars("TEST", 100, 7, time.Now().UTC())
	if first.Len() != second.Len() {
		t.Fatalf("lengths differ: %d vs %d", first.Len(), second.Len())
	}
	for i := range first.Bars {
		if first.Bars[i].Close != second.Bars[i].Close {
			t.Fatalf("close at %d differs: %v vs %v", i, first.Bars[i].Close, second.Bars[i].Close)
		}
	}
}

func TestBacktestAccountingHolds(t *testing.T) {
	result := run(t, testConfig())
	if result.Bars != 400 {
		t.Fatalf("bars = %d, want 400", result.Bars)
	}
	if len(result.Equity) == 0 {
		t.Fatal("no equity curve produced")
	}
	for _, p := range result.Equity {
		if math.Abs(p.Cash+p.MarketValue-p.Equity) > 1e-6 {
			t.Fatalf("equity accounting broken at %s: cash %v + mv %v != equity %v",
				p.Time, p.Cash, p.MarketValue, p.Equity)
		}
	}
	last := result.Equity[len(result.Equity)-1]
	if last.OpenPositions != 0 {
		t.Fatalf("open positions at end = %d, want 0 (liquidated)", last.OpenPositions)
	}
	if result.Metrics.FinalEquity == nil {
		t.Fatal("final equity is nil")
	}
}

func TestSignalsExecuteOnTheNextBar(t *testing.T) {
	// With warmup = 0 the very first bar must not trade: the first signal is
	// only known at the close of bar 0 and is executed on bar 1.
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 0
	cfg.Strategy.Name = "ma_cross"
	cfg.Strategy.Params = map[string]float64{"fast": 3, "slow": 8}
	result := run(t, cfg)
	if len(result.Orders) > 0 && result.Orders[0].Time.Equal(result.Start) {
		t.Fatal("an order was executed on the first bar: look-ahead bias")
	}
}

func TestRiskHaltStopsFurtherEntries(t *testing.T) {
	cfg := testConfig()
	cfg.Risk.MaxDrawdownPct = ptr(0.02)
	cfg.Risk.MaxDailyLossPct = nil
	result := run(t, cfg)
	if len(result.RiskEvents) == 0 {
		t.Fatal("expected a drawdown halt event with a 2% limit")
	}
	last := result.Equity[len(result.Equity)-1]
	if last.OpenPositions != 0 {
		t.Fatal("a halted agent must be flat at the end")
	}
}

func ptr(v float64) *float64 { return &v }
