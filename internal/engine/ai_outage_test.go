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
// reports ok=false plus a reason, exactly as strategy.LLM does on a timeout,
// HTTP error or unparseable answer.
type failingStrategy struct{}

func (failingStrategy) Name() string     { return "failing" }
func (failingStrategy) Describe() string { return "failing" }

func (failingStrategy) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	return strategy.Signals{Signal: make([]float64, s.Len())}, nil
}

func (failingStrategy) LastDecision(s model.Series, cfg config.Config) (float64, float64, float64, string, bool) {
	return 0, math.NaN(), math.NaN(), "模型网关连接超时", false
}

// Regression: an LLM outage used to surface as a flat target, which the live
// loop executed as signal_exit and liquidated a healthy position. A model
// failure must hold the current position and report it as such.
func TestAIModelOutageHoldsPositionInsteadOfLiquidating(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0

	book := portfolio.New(10_000, cfg.Risk.Leverage)
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
	// The reason is the whole point of the log row: "AI 不可用" with no detail
	// leaves the operator unable to tell a bad key from a dead endpoint.
	if res.Reason != "模型网关连接超时" {
		t.Fatalf("reason = %q, want the strategy's failure detail carried through to the log", res.Reason)
	}
}

// structuredStrategy implements the StructuredLiveDecision refinement: its
// answer carries the audit fields the console's AI strip needs.
type structuredStrategy struct {
	failing bool
}

func (structuredStrategy) Name() string     { return "structured" }
func (structuredStrategy) Describe() string { return "structured" }

func (structuredStrategy) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	return strategy.Signals{Signal: make([]float64, s.Len())}, nil
}

func (s structuredStrategy) LastDecision(s2 model.Series, cfg config.Config) (float64, float64, float64, string, bool) {
	d, ok := s.LastDecisionDetailed(s2, cfg)
	if !ok {
		return 0, math.NaN(), math.NaN(), d.Reason, false
	}
	return d.Signal, d.Stop, d.Target, d.Reason, true
}

func (s structuredStrategy) LastDecisionDetailed(model.Series, config.Config) (strategy.Decision, bool) {
	if s.failing {
		return strategy.Decision{Model: "test-model", Reason: "timeout: context deadline exceeded"}, false
	}
	return strategy.Decision{Signal: 1, Stop: 90, Target: 110, Reason: "金叉确认", Model: "test-model", Confidence: 0.75, Latency: 1200 * time.Millisecond}, true
}

// The structured path is what the LLM strategy exposes: the live loop must
// prefer it and carry its audit fields onto the StepResult, so the console
// can show which model decided, how sure it was and how long it took.
func TestLiveStepPrefersStructuredDecisionAndCarriesAuditFields(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0

	book := portfolio.New(10_000, cfg.Risk.Leverage)
	agent := engine.NewWithBroker(cfg, structuredStrategy{}, broker.New(cfg.Execution), book, risk.New(cfg.Risk))

	series := model.Series{Symbol: "TEST"}
	for i := 0; i < 60; i++ {
		series.Bars = append(series.Bars, model.Bar{Close: 100})
	}

	res, err := agent.LiveStep(series, 100, time.Now())
	if err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	if res.Reason != "金叉确认" {
		t.Fatalf("reason = %q, want the model's words from the structured decision", res.Reason)
	}
	if res.AIModel != "test-model" {
		t.Fatalf("AIModel = %q, want the model stamped on the result", res.AIModel)
	}
	if res.AIConfidence != 0.75 {
		t.Fatalf("AIConfidence = %v, want the model's own 0.75", res.AIConfidence)
	}
	if res.AILatency != 1200*time.Millisecond {
		t.Fatalf("AILatency = %v, want the measured 1.2s", res.AILatency)
	}
}

// A structured failure must keep the legacy hold-position semantics and carry
// the classified reason, so "AI 不可用" is never the whole story.
func TestLiveStepStructuredOutageStillHoldsPosition(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0

	book := portfolio.New(10_000, cfg.Risk.Leverage)
	s := structuredStrategy{failing: true}
	agent := engine.NewWithBroker(cfg, s, broker.New(cfg.Execution), book, risk.New(cfg.Risk))
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
	if res.Exited || !book.Position("TEST").IsOpen() {
		t.Fatal("a structured AI outage liquidated the position")
	}
	if res.Action != "ai_unavailable" {
		t.Fatalf("action = %q, want ai_unavailable", res.Action)
	}
	if res.Reason != "timeout: context deadline exceeded" {
		t.Fatalf("reason = %q, want the classified failure carried through", res.Reason)
	}
	if res.AIModel != "test-model" {
		t.Fatalf("AIModel = %q on a failed call, want the model name still stamped", res.AIModel)
	}
}
