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

type openTrade struct {
	entryTime  time.Time
	entryPrice float64
	quantity   float64
	side       broker.Side
	entryFee   float64
}

// Agent wires a strategy to the broker, portfolio and risk manager.
type Agent struct {
	Config   config.Config
	Strategy strategy.Strategy
	Symbol   string
	Broker   *broker.PaperBroker
	Book     *portfolio.Portfolio
	Risk     *risk.Manager

	open   *openTrade
	trades []Trade
}

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
	peakEquity := a.Book.InitialCash
	last := series.Len() - 1

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
		if point.Equity > peakEquity {
			peakEquity = point.Equity
		}
		a.Risk.Update(bar.Time, point.Equity, peakEquity)

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
		Orders:     a.Broker.Trades,
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
	a.open = &openTrade{
		entryTime: bar.Time, entryPrice: fill.Price,
		quantity: fill.Quantity, side: fill.Side, entryFee: fill.Commission,
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
	if a.open.side == broker.Sell {
		direction = -1.0
	}
	gross := (fill.Price - a.open.entryPrice) * a.open.quantity * direction
	fees := a.open.entryFee + fill.Commission
	notional := a.open.entryPrice * a.open.quantity
	returnPct := 0.0
	if notional != 0 {
		returnPct = (gross - fees) / notional * 100
	}
	a.trades = append(a.trades, Trade{
		EntryTime:  a.open.entryTime,
		ExitTime:   ts,
		Side:       a.open.side,
		Quantity:   a.open.quantity,
		EntryPrice: a.open.entryPrice,
		ExitPrice:  fill.Price,
		GrossPnL:   gross,
		Commission: fees,
		PnL:        gross - fees,
		ReturnPct:  returnPct,
		Reason:     reason,
	})
	a.open = nil
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
