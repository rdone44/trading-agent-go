package engine_test

import (
	"math"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// failingStrategy is a LiveDecision strategy whose model is unreachable: it
// reports ok=false, exactly as strategy.LLM does on a timeout, HTTP error or
// unparseable answer.
type failingStrategy struct{}

func (failingStrategy) Name() string     { return "failing" }
func (failingStrategy) Describe() string { return "failing" }

func (failingStrategy) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	return strategy.Signals{Signal: make([]float64, s.Len())}, nil
}

func (failingStrategy) LastDecision(s model.Series, cfg config.Config) (float64, float64, float64, string, bool) {
	return 0, math.NaN(), math.NaN(), "", false
}

// Regression: an LLM outage used to surface as a flat target, which the live
// loop executed as signal_exit and liquidated a healthy position. A model
// failure must hold the current position and report it as such.
func TestAIModelOutageHoldsPositionInsteadOfLiquidating(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0

	book := portfolio.New(10_000)
	agent := engine.NewWithBroker(cfg, failingStrategy{}, broker.New(cfg.Execution), book, risk.New(cfg.Risk))
	// A healthy long: entry 100, stop 90, target 130, marked at 100.
	agent.RestoreState(10_000, 10_000,
		&engine.OpenTrade{EntryTime: time.Now(), EntryPrice: 100, Quantity: 1, Side: broker.Buy},
		90, 130, risk.RiskState{})

	series := model.Series{Symbol: "TEST"}
	for i := 0; i < 60; i++ {
		series.Bars = append(series.Bars, model.Bar{Close: 100})
	}

	res, err := agent.LiveStep(series, 100, time.Now())
	if err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	if res.Exited {
		t.Fatal("an AI outage liquidated the position")
	}
	if !book.Position("TEST").IsOpen() {
		t.Fatal("the position must survive an AI outage")
	}
	if res.Action != "ai_unavailable" {
		t.Fatalf("action = %q, want ai_unavailable so the console shows the degradation", res.Action)
	}
}
