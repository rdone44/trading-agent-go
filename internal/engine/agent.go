// Package engine runs the trading agent: one event loop shared by backtests
// and paper trading.
//
// Decision flow for every bar:
//
//	bar open    -> execute the target decided on the previous close
//	bar range   -> protective stop / take-profit (intrabar)
//	bar close   -> mark to market, update risk, compute the next target
//
// Deciding on the close and executing on the next open avoids look-ahead bias.
package engine

import (
	"fmt"
	"math"
	"time"

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/metrics"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/portfolio"
	"github.com/huijun/trading-agent-go/internal/risk"
	"github.com/huijun/trading-agent-go/internal/strategy"
)

// Trade is one closed round trip.
type Trade struct {
	EntryTime  time.Time
	ExitTime   time.Time
	Side       broker.Side
	Quantity   float64
	EntryPrice float64
	ExitPrice  float64
	GrossPnL   float64
	Commission float64
	PnL        float64
	ReturnPct  float64
	Reason     string
}

// PnLValue and FeeValue let the metrics package consume trades without
// importing this package.
func (t Trade) PnLValue() float64 { return t.PnL }
func (t Trade) FeeValue() float64 { return t.Commission }

// Result is everything a run produces.
type Result struct {
	Symbol     string
	Strategy   string
	DataSource string
	Start      time.Time
	End        time.Time
	Bars       int
	Metrics    metrics.Metrics
	Equity     []portfolio.EquityPoint
	Trades     []Trade
	Orders     []broker.Fill
	RiskEvents []risk.Event
}

// OpenTrade is one open position, tracked for the round-trip PnL report. It
// is exported so the CLI layer can persist and restore it across restarts.
type OpenTrade struct {
	EntryTime  time.Time
	EntryPrice float64
	Quantity   float64
	Side       broker.Side
	EntryFee   float64
}

// VetoContext is what a second-opinion gate is shown when deciding whether
// to let a new entry through. It is deliberately a value type so a gate can
// be wired in without holding a reference to the agent.
type VetoContext struct {
	Symbol      string
	Side        broker.Side
	Quantity    float64
	EntryPrice  float64
	StopPrice   float64
	TargetPrice float64
	Equity      float64
	Cash        float64
	Leverage    int
	Reason      string // the strategy's stated reason for the entry
}

// Veto is a second-opinion gate on a new entry. It returns blocked=true only
// when the gate actively objects; any error inside the gate is the caller's
// to treat, and the engine's contract is fail-open: a gate that cannot
// answer lets the entry through. A nil Veto is a no-op gate.
type Veto func(now time.Time, ctx VetoContext) (blocked bool, reason string)

// Agent wires a strategy to the broker, portfolio and risk manager.
type Agent struct {
	Config   config.Config
	Strategy strategy.Strategy
	Symbol   string
	Broker   broker.Broker
	Book     *portfolio.Portfolio
	Risk     *risk.Manager
	// Veto, when set, is consulted before every new entry in the live loop.
	// Backtests leave it nil so historical runs stay deterministic.
	Veto Veto

	open   *OpenTrade
	trades []Trade
	// PeakEquity is the running equity high-water mark used by the risk
	// manager; it survives restarts through state persistence.
	PeakEquity float64
}

// New builds an agent with a paper broker — the shape used by backtests and
// the default trade loop (which simulates fills, no real orders).
func New(cfg config.Config, strat strategy.Strategy) *Agent {
	symbol := cfg.Agent.Symbol
	return &Agent{
		Config:   cfg,
		Strategy: strat,
		Symbol:   symbol,
		Broker:   broker.New(cfg.Execution),
		Book:     portfolio.New(cfg.Risk.InitialCash),
		Risk:     risk.New(cfg.Risk),
	}
}

// NewWithBroker builds an agent around a caller-supplied broker (e.g. the
// live Binance broker) but reuses the given portfolio and risk manager. Used
// by the trade loop so a real broker and a paper one run the identical
// decision path.
func NewWithBroker(cfg config.Config, strat strategy.Strategy, bk broker.Broker, book *portfolio.Portfolio, rm *risk.Manager) *Agent {
	return &Agent{
		Config:   cfg,
		Strategy: strat,
		Symbol:   cfg.Agent.Symbol,
		Broker:   bk,
		Book:     book,
		Risk:     rm,
	}
}

// RunBacktest replays a series bar by bar.
func (a *Agent) RunBacktest(series model.Series) (Result, error) {
	if series.Len() == 0 {
		return Result{}, fmt.Errorf("行情数据为空")
	}
	a.Symbol = series.Symbol

	signals, err := a.Strategy.Generate(series, a.Config)
	if err != nil {
		return Result{}, err
	}
	signals = signals.Normalize(series.Len())
	// A signal observed at the close of bar t is executed at the open of bar t+1.
	targets := shift(signals.Signal, 1)
	stops := shift(signals.Stop, 1)
	takeProfits := shift(signals.TakeProfit, 1)

	warmup := a.Config.Backtest.WarmupBars
	last := series.Len() - 1
	if a.PeakEquity <= 0 {
		a.PeakEquity = a.Book.InitialCash
	}

	for i, bar := range series.Bars {
		// 1) Intrabar protective exits. Stops are checked first: the
		//    conservative assumption when both would trigger.
		if pos := a.Book.Position(a.Symbol); pos.IsOpen() {
			a.checkExits(bar)
		}

		// 2) Execute yesterday's target at today's open.
		if err := a.rebalance(bar, targets[i], stops[i], takeProfits[i], i, warmup); err != nil {
			return Result{}, err
		}

		// 3) Mark to market and update the risk limits.
		prices := map[string]float64{a.Symbol: bar.Close}
		point := a.Book.Record(bar.Time, prices)
		if point.Equity > a.PeakEquity {
			a.PeakEquity = point.Equity
		}
		a.Risk.Update(bar.Time, point.Equity, a.PeakEquity)

		if a.Risk.Halted {
			a.closePosition(bar.Time, bar.Close, "risk halt: "+a.Risk.HaltReason)
		}

		// 4) Close whatever is still open so runs stay comparable.
		if i == last && a.Config.Execution.LiquidateAtEnd {
			a.closePosition(bar.Time, bar.Close, "end of backtest")
			// Refresh the final point now that the position is flat.
			if len(a.Book.Curve) > 0 {
				final := &a.Book.Curve[len(a.Book.Curve)-1]
				final.Cash = a.Book.Cash
				final.MarketValue = a.Book.MarketValue(prices)
				final.Equity = final.Cash + final.MarketValue
				final.RealizedPnL = a.Book.RealizedPnL
				final.OpenPositions = 0
			}
		}
	}

	result := Result{
		Symbol:     series.Symbol,
		Strategy:   a.Strategy.Describe(),
		DataSource: series.Source,
		Start:      series.Bars[0].Time,
		End:        series.Bars[last].Time,
		Bars:       series.Len(),
		Equity:     a.Book.Curve,
		Trades:     a.trades,
		Orders:     a.Broker.Fills(),
		RiskEvents: a.Risk.Events,
	}
	result.Metrics = metrics.Compute(
		a.Book.Curve, a.trades, a.Book.InitialCash,
		a.Config.BarsPerYear(),
	)
	return result, nil
}

// checkExits applies stop-loss and take-profit against the bar's range.
func (a *Agent) checkExits(bar model.Bar) {
	pos := a.Book.Position(a.Symbol)
	stop, target := pos.StopPrice, pos.TakeProfitPrice

	if pos.Quantity > 0 {
		if model.Valid(stop) && bar.Low <= stop {
			a.closePosition(bar.Time, math.Min(bar.Open, stop), "stop_loss")
			return
		}
		if model.Valid(target) && bar.High >= target {
			a.closePosition(bar.Time, math.Max(bar.Open, target), "take_profit")
		}
		return
	}
	if model.Valid(stop) && bar.High >= stop {
		a.closePosition(bar.Time, math.Max(bar.Open, stop), "stop_loss")
		return
	}
	if model.Valid(target) && bar.Low <= target {
		a.closePosition(bar.Time, math.Min(bar.Open, target), "take_profit")
	}
}

// rebalance moves the position towards the target (-1, 0 or 1).
func (a *Agent) rebalance(bar model.Bar, target, strategyStop, strategyTarget float64, index, warmup int) error {
	pos := a.Book.Position(a.Symbol)
	desired := 0.0
	switch {
	case target > 0:
		desired = 1
	case target < 0:
		if a.Config.Risk.AllowShort {
			desired = -1
		}
	}

	current := 0.0
	if pos.IsOpen() {
		current = math.Copysign(1, pos.Quantity)
	}
	if desired == current {
		return nil
	}

	if desired == 0 {
		if pos.IsOpen() {
			a.closePosition(bar.Time, bar.Open, "signal_exit")
		}
		return nil
	}
	if index < warmup {
		return nil
	}

	decision := a.Risk.CanEnter(len(a.Book.OpenPositions()), pos.IsOpen())
	if !decision.Allowed {
		return nil
	}

	side := broker.Buy
	sideName := "buy"
	if desired < 0 {
		side = broker.Sell
		sideName = "sell"
	}
	stop, targetPrice := a.Risk.StopAndTarget(sideName, bar.Open, strategyStop, strategyTarget)

	quantity := a.Risk.PositionSize(
		a.Book.LastEquity(), a.Book.Cash, bar.Open, stop, a.Config.Execution.LotSize,
	)
	if quantity <= 0 {
		return nil
	}

	reason := "entry_long"
	if side == broker.Sell {
		reason = "entry_short"
	}
	fill := a.Broker.MarketOrder(bar.Time, a.Symbol, side, quantity, bar.Open, reason)
	if fill.Rejected {
		return nil
	}

	a.Book.ApplyFill(fill)
	updated := a.Book.Position(a.Symbol)
	updated.StopPrice = stop
	updated.TakeProfitPrice = targetPrice
	updated.OpenedAt = bar.Time
	a.open = &OpenTrade{
		EntryTime: bar.Time, EntryPrice: fill.Price,
		Quantity: fill.Quantity, Side: fill.Side, EntryFee: fill.Commission,
	}
	return nil
}

func (a *Agent) closePosition(ts time.Time, price float64, reason string) {
	pos := a.Book.Position(a.Symbol)
	if !pos.IsOpen() {
		return
	}
	side := broker.Sell
	if pos.Quantity < 0 {
		side = broker.Buy
	}
	fill := a.Broker.MarketOrder(ts, a.Symbol, side, math.Abs(pos.Quantity), price, reason)
	if fill.Rejected {
		return
	}
	a.Book.ApplyFill(fill)
	a.recordClose(ts, fill, reason)
}

func (a *Agent) recordClose(ts time.Time, fill broker.Fill, reason string) {
	if a.open == nil {
		return
	}
	direction := 1.0
	if a.open.Side == broker.Sell {
		direction = -1.0
	}
	gross := (fill.Price - a.open.EntryPrice) * a.open.Quantity * direction
	fees := a.open.EntryFee + fill.Commission
	notional := a.open.EntryPrice * a.open.Quantity
	returnPct := 0.0
	if notional != 0 {
		returnPct = (gross - fees) / notional * 100
	}
	a.trades = append(a.trades, Trade{
		EntryTime:  a.open.EntryTime,
		ExitTime:   ts,
		Side:       a.open.Side,
		Quantity:   a.open.Quantity,
		EntryPrice: a.open.EntryPrice,
		ExitPrice:  fill.Price,
		GrossPnL:   gross,
		Commission: fees,
		PnL:        gross - fees,
		ReturnPct:  returnPct,
		Reason:     reason,
	})
	a.open = nil
}

// OpenTrade returns a copy of the currently open position, or nil when flat.
// It is the persistence seam: the CLI saves it to disk so a restart can
// resume round-trip PnL accounting.
func (a *Agent) OpenTrade() *OpenTrade {
	if a.open == nil {
		return nil
	}
	c := *a.open
	return &c
}

// Trades returns the closed round trips recorded this run, for the post-mortem
// reviewer and the CLI. It is a copy so a caller cannot mutate the agent's log.
func (a *Agent) Trades() []Trade {
	out := make([]Trade, len(a.trades))
	copy(out, a.trades)
	return out
}

// ResultSnapshot builds a Result view of the agent's current state so the
// post-mortem reviewer and the live loop can summarise a session the same way
// a backtest is summarized. Equity metrics are recomputed from the book curve
// that has accumulated during the session; a session with no recorded bars
// produces a zero-bars result whose metric pointer fields are null.
func (a *Agent) ResultSnapshot() Result {
	m := metrics.Compute(a.Book.Curve, a.trades, a.Book.InitialCash, a.Config.BarsPerYear())
	var start, end time.Time
	if len(a.Book.Curve) > 0 {
		start = a.Book.Curve[0].Time
		end = a.Book.Curve[len(a.Book.Curve)-1].Time
	}
	return Result{
		Symbol:     a.Symbol,
		Strategy:   a.Strategy.Describe(),
		DataSource: a.Config.Data.Provider,
		Start:      start,
		End:        end,
		Bars:       len(a.Book.Curve),
		Metrics:    m,
		Trades:     a.Trades(),
		Orders:     a.Broker.Fills(),
		RiskEvents: a.Risk.Events,
	}
}

// RestoreState rebuilds the agent's mutable state from a persisted session:
// cash, the equity high-water mark, the open position with its protective
// levels, and the risk manager's halt/daily-pause state. Called on startup
// so a restarted trade loop does not reopen a position it already holds or
// re-trigger a risk limit it already tripped.
func (a *Agent) RestoreState(cash, peak float64, open *OpenTrade, stop, target float64, riskState risk.RiskState) {
	a.Book.Cash = cash
	a.PeakEquity = peak
	if open != nil {
		// Rebuild the portfolio position from the persisted round trip. A
		// short leg comes back signed negative, so the book matches the
		// exchange's signed position on a futures venue.
		quantity := open.Quantity
		if open.Side == broker.Sell {
			quantity = -open.Quantity
		}
		pos := a.Book.Position(a.Symbol)
		pos.Quantity = quantity
		pos.AvgPrice = open.EntryPrice
		pos.OpenedAt = open.EntryTime
		pos.StopPrice = stop
		pos.TakeProfitPrice = target
		// Cash already reflects the entry fill (it was persisted after the
		// fill was applied), so the position's cost is not re-debited.
		a.open = open
	} else {
		a.open = nil
	}
	a.Risk.Restore(riskState)
}

// shift moves values forward by n bars, leaving NaN at the front.
func shift(values []float64, n int) []float64 {
	out := make([]float64, len(values))
	for i := range out {
		out[i] = math.NaN()
		if i >= n {
			out[i] = values[i-n]
		}
	}
	return out
}
