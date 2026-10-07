package engine_test

import (
	"math"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/portfolio"
	"github.com/huijun/trading-agent-go/internal/risk"
	"github.com/huijun/trading-agent-go/internal/strategy"
	"github.com/huijun/trading-agent-go/internal/testfx"
)

// liveAgent builds a paper-broker agent plus a deterministic offline series,
// both driven by the same seed so the run is reproducible.
func liveAgent(t *testing.T, cfg config.Config, days, seed int) (*engine.Agent, model.Series) {
	t.Helper()
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatalf("build strategy: %v", err)
	}
	agent := engine.New(cfg, strat)
	series := testfx.Bars(cfg.Agent.Symbol, days, int64(seed), time.Now().UTC())
	return agent, series
}

// stepOnce feeds the full series plus a live price through one LiveStep.
func stepOnce(t *testing.T, agent *engine.Agent, series model.Series, price float64) engine.StepResult {
	t.Helper()
	res, err := agent.LiveStep(series, price, time.Now().UTC())
	if err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	return res
}

// TestLiveStepAccountingHolds runs several live cycles over a fixed series and
// checks the equity book stays consistent after every one.
func TestLiveStepAccountingHolds(t *testing.T) {
	cfg := testConfig()
	cfg.Strategy.Name = "ma_cross"
	cfg.Strategy.Params = map[string]float64{"fast": 3, "slow": 8}
	agent, series := liveAgent(t, cfg, 200, 42)

	close := series.Close()
	for i, px := range close {
		res := stepOnce(t, agent, series, px)
		last := agent.Book.Curve[len(agent.Book.Curve)-1]
		if math.Abs(last.Cash+last.MarketValue-last.Equity) > 1e-6 {
			t.Fatalf("equity broken on cycle %d: %v", i, last)
		}
		if res.Equity <= 0 {
			t.Fatalf("cycle %d: equity = %v, want > 0", i, res.Equity)
		}
	}
}

// TestLiveStepRejectsBadPrice guards the live loop against a zero or NaN price.
func TestLiveStepRejectsBadPrice(t *testing.T) {
	cfg := testConfig()
	agent, series := liveAgent(t, cfg, 50, 1)
	if _, err := agent.LiveStep(series, 0, time.Now().UTC()); err == nil {
		t.Fatal("zero price must be rejected")
	}
	if _, err := agent.LiveStep(series, math.NaN(), time.Now().UTC()); err == nil {
		t.Fatal("NaN price must be rejected")
	}
}

// TestLiveStepEmptySeries must error, not silently hold.
func TestLiveStepEmptySeries(t *testing.T) {
	cfg := testConfig()
	agent, _ := liveAgent(t, cfg, 1, 1)
	if _, err := agent.LiveStep(model.Series{}, 100, time.Now().UTC()); err == nil {
		t.Fatal("empty series must error")
	}
}

// TestLiveStepRiskHaltPersists confirms a halted risk manager blocks the step
// and flattens any open position, then stays halted.
func TestLiveStepRiskHaltPersists(t *testing.T) {
	cfg := testConfig()
	cfg.Execution.MinTradeNotional = 0 // isolate the halt path from the size guard
	agent, series := liveAgent(t, cfg, 50, 7)
	px := series.Close()[series.Len()-1]

	// Plant an open long via the persistence seam, then halt the risk manager.
	agent.RestoreState(
		agent.Book.Cash, agent.PeakEquity,
		&engine.OpenTrade{
			EntryTime: time.Now(), EntryPrice: px * 0.9, Quantity: 1,
			Side: broker.Buy, EntryFee: 0,
		}, math.NaN(), math.NaN(), riskState(),
	)
	agent.Risk.Halted = true
	agent.Risk.HaltReason = "max drawdown breached"

	res := stepOnce(t, agent, series, px)
	if res.Action != "risk_halt" {
		t.Fatalf("action = %q, want risk_halt", res.Action)
	}
	if !agent.Risk.Halted {
		t.Fatal("halt must persist through the step")
	}
	if agent.Book.Position(cfg.Agent.Symbol).IsOpen() {
		t.Fatal("a halted agent must flatten its open position")
	}
}

// TestRestoreStateRebuildsPosition confirms a restarted agent reopens its
// round-trip book and protective levels from persisted values.
func TestRestoreStateRebuildsPosition(t *testing.T) {
	cfg := testConfig()
	agent, _ := liveAgent(t, cfg, 1, 1)
	entry := time.Now().Add(-48 * time.Hour)
	open := &engine.OpenTrade{EntryTime: entry, EntryPrice: 100, Quantity: 2, Side: broker.Buy, EntryFee: 1}
	agent.RestoreState(50000, 60000, open, 90, 130, riskState())

	if got := agent.Book.Cash; got != 50000 {
		t.Fatalf("cash = %v, want 50000", got)
	}
	if got := agent.PeakEquity; got != 60000 {
		t.Fatalf("peak = %v, want 60000", got)
	}
	pos := agent.Book.Position(cfg.Agent.Symbol)
	if !pos.IsOpen() || pos.Quantity != 2 || pos.AvgPrice != 100 {
		t.Fatalf("position not rebuilt: %+v", pos)
	}
	if pos.StopPrice != 90 || pos.TakeProfitPrice != 130 {
		t.Fatalf("levels not rebuilt: stop=%v target=%v", pos.StopPrice, pos.TakeProfitPrice)
	}
	if agent.OpenTrade() == nil {
		t.Fatal("OpenTrade accessor must return the restored position")
	}
}

func riskState() risk.RiskState {
	return risk.RiskState{Halted: true, HaltReason: "carried over"}
}

// futuresAgent builds an agent around a local (no-network) futures broker so
// the short round-trip can be exercised without keys.
func futuresAgent(t *testing.T, cfg config.Config) *engine.Agent {
	t.Helper()
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatalf("build strategy: %v", err)
	}
	bk := broker.NewFutures(broker.FuturesConfig{
		Symbol: cfg.Agent.Symbol, Leverage: 5, DryRun: true, StepSize: 0.001,
	})
	book := portfolio.New(cfg.Risk.InitialCash)
	rm := risk.New(cfg.Risk)
	return engine.NewWithBroker(cfg, strat, bk, book, rm)
}

// TestRestoreStateRebuildsShortPosition confirms a restarted agent restores a
// short leg with a *signed* (negative) quantity, so the book matches the
// exchange's positionRisk on a perpetual venue.
func TestRestoreStateRebuildsShortPosition(t *testing.T) {
	cfg := testConfig()
	agent := futuresAgent(t, cfg)
	open := &engine.OpenTrade{
		EntryTime:  time.Now().Add(-24 * time.Hour),
		EntryPrice: 100, Quantity: 2, Side: broker.Sell, EntryFee: 1,
	}
	agent.RestoreState(50_000, 60_000, open, 130, 80, risk.RiskState{})

	pos := agent.Book.Position(cfg.Agent.Symbol)
	if pos.Quantity != -2 {
		t.Fatalf("position quantity = %v, want -2 (signed short)", pos.Quantity)
	}
	if pos.StopPrice != 130 || pos.TakeProfitPrice != 80 {
		t.Fatalf("levels = %v/%v, want stop 130 / target 80", pos.StopPrice, pos.TakeProfitPrice)
	}
	// A long leg still comes back positive, for the contrast.
	agent.RestoreState(50_000, 60_000,
		&engine.OpenTrade{EntryPrice: 100, Quantity: 2, Side: broker.Buy}, 90, 130, risk.RiskState{})
	if got := agent.Book.Position(cfg.Agent.Symbol).Quantity; got != 2 {
		t.Fatalf("long quantity = %v, want +2", got)
	}
}

// TestLiveStepShortProtectiveExit drives the short branch of liveExits: a
// falling price closes the short at take-profit, a rising one at the stop.
// Re-entry is blocked so the protective exit is the only thing that can move
// the position.
func TestLiveStepShortProtectiveExit(t *testing.T) {
	cases := []struct {
		name    string
		price   float64 // live price fed through the step
		want    string  // expected close reason
		flatten bool
	}{
		{"take_profit", 85, "take_profit", true}, // price <= target (90)
		{"stop_loss", 120, "stop_loss", true},    // price >= stop (110)
		{"hold", 100, "", false},                 // between the two levels
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Execution.MinTradeNotional = 0
			cfg.Backtest.WarmupBars = 100_000 // block any liveRebalance entry
			agent := futuresAgent(t, cfg)
			series := testfx.Bars(cfg.Agent.Symbol, 100, 99, time.Now().UTC())
			// Plant an open short at 100: stop above at 110, target below at 90.
			agent.RestoreState(cfg.Risk.InitialCash, cfg.Risk.InitialCash,
				&engine.OpenTrade{
					EntryTime: time.Now(), EntryPrice: 100, Quantity: 2,
					Side: broker.Sell, EntryFee: 1,
				}, 110, 90, risk.RiskState{})

			res, err := agent.LiveStep(series, tc.price, time.Now())
			if err != nil {
				t.Fatalf("LiveStep: %v", err)
			}
			pos := agent.Book.Position(cfg.Agent.Symbol)
			if tc.flatten {
				if pos.IsOpen() {
					t.Fatalf("short not flattened: quantity=%v after %s", pos.Quantity, tc.want)
				}
				if agent.OpenTrade() != nil {
					t.Fatalf("OpenTrade should be cleared after a protective close, got %+v", agent.OpenTrade())
				}
			} else {
				if !pos.IsOpen() || pos.Quantity != -2 {
					t.Fatalf("short should stay open: quantity=%v", pos.Quantity)
				}
			}
			_ = res
		})
	}
}

// countStrat is a strategy that records which seam the live loop used: the
// full-signal Generate or the single-decision LastDecision. It is flat, so no
// trades occur and the test stays focused on call counting.
type countStrat struct {
	generateCalls int
	lastCalls     int
}

func (c *countStrat) Name() string     { return "count" }
func (c *countStrat) Describe() string { return "count" }

func (c *countStrat) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	c.generateCalls++
	return strategy.Signals{Signal: make([]float64, s.Len())}, nil
}

func (c *countStrat) LastDecision(s model.Series, cfg config.Config) (float64, float64, float64) {
	c.lastCalls++
	return 0, math.NaN(), math.NaN()
}

// TestLiveStepUsesSingleDecisionForExpensiveStrategy locks the P0 guarantee:
// a strategy that exposes a single-decision path (like the LLM one) is billed
// exactly one model call per live poll, even when the runner feeds a long
// lookback. Regenerating the full per-bar signal would cost O(bars) calls.
func TestLiveStepUsesSingleDecisionForExpensiveStrategy(t *testing.T) {
	cfg := testConfig()
	// 400 bars mirrors a realistic live lookback window.
	series := testfx.Bars(cfg.Agent.Symbol, 400, 7, time.Now().UTC())

	s := &countStrat{}
	agent := engine.NewWithBroker(cfg, s,
		broker.New(cfg.Execution), portfolio.New(cfg.Risk.InitialCash), risk.New(cfg.Risk))

	price := series.Close()[series.Len()-1]
	if _, err := agent.LiveStep(series, price, time.Now()); err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	if s.lastCalls != 1 {
		t.Fatalf("LastDecision called %d times, want exactly 1 (single-decision path)", s.lastCalls)
	}
	if s.generateCalls != 0 {
		t.Fatalf("Generate called %d times; an expensive strategy must not regenerate the full signal on a live poll", s.generateCalls)
	}
}

// TestLiveStepFallsBackToGenerateForPlainStrategy confirms the cheap
// indicator strategies still go through the full-signal path (they do not
// implement the single-decision seam), so this change is strictly additive.
type plainStrat struct {
	generateCalls int
}

func (p *plainStrat) Name() string     { return "plain" }
func (p *plainStrat) Describe() string { return "plain" }

func (p *plainStrat) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	p.generateCalls++
	return strategy.Signals{Signal: make([]float64, s.Len())}, nil
}

func TestLiveStepFallsBackToGenerateForPlainStrategy(t *testing.T) {
	cfg := testConfig()
	series := testfx.Bars(cfg.Agent.Symbol, 400, 7, time.Now().UTC())

	p := &plainStrat{}
	agent := engine.NewWithBroker(cfg, p,
		broker.New(cfg.Execution), portfolio.New(cfg.Risk.InitialCash), risk.New(cfg.Risk))

	if _, err := agent.LiveStep(series, series.Close()[series.Len()-1], time.Now()); err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	if p.generateCalls != 1 {
		t.Fatalf("plain strategy: Generate called %d times, want 1 (full-signal fallback)", p.generateCalls)
	}
}
