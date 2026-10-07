package engine_test

import (
	"math"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

func TestStopRunsBeforeBrokenStrategyAndDoesNotReenter(t *testing.T) {
	cfg := config.Default()
	cfg.Execution.MinTradeNotional = 0
	cfg.Strategy.Params = map[string]float64{"fast": 30, "slow": 10}
	a := engine.New(cfg, strategy.MovingAverageCross{})
	a.RestoreState(99900, 100000, &engine.OpenTrade{EntryPrice: 100, Quantity: 1, Side: broker.Buy}, 90, 130, risk.RiskState{})
	res, err := a.LiveStep(model.Series{Symbol: cfg.Agent.Symbol, Bars: []model.Bar{{Close: 100}}}, 80, time.Now())
	if err != nil || a.Book.Position(a.Symbol).IsOpen() || !res.Exited || res.MarkPrice != 80 {
		t.Fatalf("stop lost to strategy failure: %+v err=%v", res, err)
	}
	if len(a.Broker.Fills()) != 1 {
		t.Fatal("stop reopened on the same cycle")
	}
}

type uncertainBroker struct{ calls int }

func (b *uncertainBroker) MarketOrder(ts time.Time, s string, side broker.Side, q, p float64, reason string) broker.Fill {
	b.calls++
	return broker.Fill{Rejected: true, Uncertain: true, Status: "unknown", Reason: "timeout"}
}
func (b *uncertainBroker) Fills() []broker.Fill {
	return []broker.Fill{{Rejected: true, Uncertain: true, Reason: "timeout"}}
}

func TestUnknownExitIsNotRetriedBlindly(t *testing.T) {
	cfg := config.Default()
	a := engine.New(cfg, strategy.MovingAverageCross{})
	b := &uncertainBroker{}
	a.Broker = b
	a.RestoreState(99900, 100000, &engine.OpenTrade{EntryPrice: 100, Quantity: 1, Side: broker.Buy}, 90, math.NaN(), risk.RiskState{})
	a.Protect(80, time.Now())
	a.Protect(80, time.Now())
	if b.calls != 1 || !a.Risk.OrderUncertain {
		t.Fatalf("unknown order retried %d times", b.calls)
	}
}
