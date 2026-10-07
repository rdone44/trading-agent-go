package engine_test

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

// recProtective is a fake engine.Protective that records every call so a test
// can assert the wiring: entry opens protective orders, a flatten cancels them
// first, and a failing cancel/open degrades safely rather than stalling.
type recProtective struct {
	mu        sync.Mutex
	opens     []openCall
	cancels   int
	hasCalls  int
	openErr   error
	cancelErr error
	has       bool
}

type openCall struct {
	side   broker.Side
	stop   float64
	target float64
}

func (r *recProtective) Open(side broker.Side, stop, target float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens = append(r.opens, openCall{side, stop, target})
	return r.openErr
}

func (r *recProtective) Cancel() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
	return r.cancelErr
}

func (r *recProtective) Has() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hasCalls++
	return r.has, nil
}

// entryLong drives a long entry through the single-decision seam, leaving the
// stop/target to the risk fallback so the protective levels are the risk
// engine's own values — the single source the whole P0 guarantee rests on.
type entryLong struct{}

func (entryLong) Name() string     { return "entrylong" }
func (entryLong) Describe() string { return "entrylong" }

func (entryLong) Generate(s model.Series, cfg config.Config) (strategy.Signals, error) {
	n := s.Len()
	return strategy.Signals{
		Signal:     make([]float64, n),
		Stop:       make([]float64, n),
		TakeProfit: make([]float64, n),
	}, nil
}

func (entryLong) LastDecision(s model.Series, cfg config.Config) (float64, float64, float64) {
	return 1, math.NaN(), math.NaN() // long; let the risk engine pick the levels
}

// newAgentWithBroker wires a caller-supplied broker, a fake Protective and a
// strategy so the two protective call sites can be observed.
func newAgentWithBroker(t *testing.T, cfg config.Config, bk broker.Broker, prot engine.Protective, strat strategy.Strategy) *engine.Agent {
	t.Helper()
	a := engine.NewWithBroker(cfg, strat, bk, portfolio.New(cfg.Risk.InitialCash), risk.New(cfg.Risk))
	a.Protective = prot
	return a
}

// A protective exit (stop hit) must cancel the venue's protective orders before
// the local market flatten, so the exchange cannot double-close a position the
// book already flattened.
func TestProtectiveCancelBeforeFlatten(t *testing.T) {
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0
	rec := &recProtective{}
	agent := newAgentWithBroker(t, cfg, broker.New(cfg.Execution), rec, entryLong{})

	// Plant an open long: entry 100, stop 90, target 130.
	agent.RestoreState(cfg.Risk.InitialCash, cfg.Risk.InitialCash,
		&engine.OpenTrade{EntryTime: time.Now(), EntryPrice: 100, Quantity: 2, Side: broker.Buy, EntryFee: 0},
		90, 130, risk.RiskState{})

	// A price at/below the stop triggers the protective exit.
	if _, err := agent.LiveStep(model.Series{}, 85, time.Now()); err != nil {
		t.Fatalf("LiveStep (stop hit): %v", err)
	}
	if agent.Book.Position(cfg.Agent.Symbol).IsOpen() {
		t.Fatal("position should be flattened after a stop hit")
	}
	if rec.cancels == 0 {
		t.Fatal("expected Cancel() on the protective exit, got zero calls")
	}
}

// When a position is opened the venue's protective orders must be placed, and
// the stop/target passed to Open must be the same levels the risk engine stored
// on the position — a single source, never a second, venue-local computation.
func TestProtectiveOpenedOnEntry(t *testing.T) {
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0
	rec := &recProtective{}
	agent := newAgentWithBroker(t, cfg, broker.New(cfg.Execution), rec, entryLong{})

	series := testfx.Bars(cfg.Agent.Symbol, 200, 42, time.Now().UTC())
	price := series.Close()[series.Len()-1]
	if _, err := agent.LiveStep(series, price, time.Now()); err != nil {
		t.Fatalf("LiveStep: %v", err)
	}
	pos := agent.Book.Position(cfg.Agent.Symbol)
	if !pos.IsOpen() {
		t.Fatalf("expected an open long after the entry cycle, got quantity=%v", pos.Quantity)
	}
	if len(rec.opens) != 1 {
		t.Fatalf("expected exactly one protective open on entry, got %d", len(rec.opens))
	}
	c := rec.opens[0]
	if c.side != broker.Buy {
		t.Errorf("open side = %v, want Buy", c.side)
	}
	if c.stop != pos.StopPrice {
		t.Errorf("open stop = %v, position stop = %v (must be the risk engine's single source)", c.stop, pos.StopPrice)
	}
	if c.target != pos.TakeProfitPrice {
		t.Errorf("open target = %v, position target = %v", c.target, pos.TakeProfitPrice)
	}
}

// A failing protective cancel is fail-safe: the flatten is refused (position
// stays open), the risk manager flags the order uncertain, and the *next* step
// refuses to trade until the protective state is reconciled.
func TestProtectiveCancelFailureIsFailSafe(t *testing.T) {
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0
	rec := &recProtective{cancelErr: errors.New("撤单网络超时")}
	agent := newAgentWithBroker(t, cfg, broker.New(cfg.Execution), rec, entryLong{})

	agent.RestoreState(cfg.Risk.InitialCash, cfg.Risk.InitialCash,
		&engine.OpenTrade{EntryTime: time.Now(), EntryPrice: 100, Quantity: 2, Side: broker.Buy, EntryFee: 0},
		90, 130, risk.RiskState{})

	// The stop hit cannot complete: cancel fails, so the position is preserved.
	if _, err := agent.LiveStep(model.Series{}, 85, time.Now()); err != nil {
		t.Fatalf("the protective-exit step itself must not hard-error: %v", err)
	}
	if !agent.Book.Position(cfg.Agent.Symbol).IsOpen() {
		t.Fatal("on a failed cancel the position must stay open, not flatten")
	}
	if !agent.Risk.OrderUncertain {
		t.Fatal("a failed cancel must flag the order uncertain for reconciliation")
	}
	// The next step refuses to act until the protective state is resolved.
	if _, err := agent.LiveStep(model.Series{}, 85, time.Now()); err == nil {
		t.Fatal("with the order uncertain the loop must stop, not keep trading")
	}
}

// A failing protective open degrades to local-only protection: the entry still
// completes (the local exits remain active) and the uncertainty is flagged, so
// a venue hiccup on the protective order never blocks a legitimate trade.
func TestProtectiveOpenFailureIsFailOpen(t *testing.T) {
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0
	rec := &recProtective{openErr: errors.New("挂单被拒")}
	agent := newAgentWithBroker(t, cfg, broker.New(cfg.Execution), rec, entryLong{})

	series := testfx.Bars(cfg.Agent.Symbol, 200, 42, time.Now().UTC())
	price := series.Close()[series.Len()-1]
	res, err := agent.LiveStep(series, price, time.Now())
	if err != nil {
		t.Fatalf("a failed protective open must not fail the entry: %v", err)
	}
	if !res.Entered || !agent.Book.Position(cfg.Agent.Symbol).IsOpen() {
		t.Fatalf("entry should still complete on a protective-open failure: %+v", res)
	}
	if !agent.Risk.OrderUncertain {
		t.Fatal("a failed protective open must flag the order uncertain")
	}
}

// TestNoProtectiveIsNoOp confirms the historical/backtest path is unchanged:
// with Protective nil (paper, dry-run, backtest) a stop hit still flattens and
// nothing on the protective side is invoked.
func TestNoProtectiveIsNoOp(t *testing.T) {
	cfg := testConfig()
	cfg.Backtest.WarmupBars = 2
	cfg.Execution.MinTradeNotional = 0

	agent := newAgentWithBroker(t, cfg, broker.New(cfg.Execution), nil, entryLong{})
	agent.RestoreState(cfg.Risk.InitialCash, cfg.Risk.InitialCash,
		&engine.OpenTrade{EntryTime: time.Now(), EntryPrice: 100, Quantity: 2, Side: broker.Buy, EntryFee: 0},
		90, 130, risk.RiskState{})

	if _, err := agent.LiveStep(model.Series{}, 85, time.Now()); err != nil {
		t.Fatalf("nil protective path must flatten cleanly: %v", err)
	}
	if agent.Book.Position(cfg.Agent.Symbol).IsOpen() {
		t.Fatal("a stop hit must flatten even without a protective venue")
	}
}
