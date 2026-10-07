// Live trading primitives: one decision cycle against the live market,
// reusing the protective-exit, risk and sizing logic of the backtest loop.
package engine

import (
	"fmt"
	"math"
	"time"

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/model"
)

// StepResult describes what one live cycle did.
type StepResult struct {
	Exited  bool
	Entered bool
	Action  string // human-readable outcome, e.g. "hold", "entry_long", "stop_loss"
	Equity  float64
	Cash    float64
	Open    *OpenTrade
}

// LiveStep runs one decision cycle:
//
//  1. check the open position's stop/target against the live price,
//  2. mark to market and refresh the risk limits,
//  3. act on the strategy's decision for the last closed bar, at the live
//     price.
//
// The state that carries between calls (peak equity, open trade, risk halt,
// daily loss pause) lives on the agent, so the CLI layer can persist it to
// disk between cycles and after a restart.
func (a *Agent) LiveStep(series model.Series, price float64, now time.Time) (StepResult, error) {
	if series.Len() == 0 {
		return StepResult{}, fmt.Errorf("行情数据为空")
	}
	if !(price > 0) {
		return StepResult{}, fmt.Errorf("live 价格无效: %v", price)
	}
	a.Symbol = series.Symbol

	signals, err := a.Strategy.Generate(series, a.Config)
	if err != nil {
		return StepResult{}, err
	}
	signals = signals.Normalize(series.Len())
	n := series.Len()
	target := signals.Signal[n-1]
	stop := signals.Stop[n-1]
	takeProfit := signals.TakeProfit[n-1]

	res := StepResult{}

	// 1) Protective exits against the live price.
	if pos := a.Book.Position(a.Symbol); pos.IsOpen() {
		if did := a.liveExits(now, price); did {
			res.Exited = true
			res.Action = "protective_exit"
		}
	}

	// 2) Mark to market and refresh the risk limits.
	prices := map[string]float64{a.Symbol: price}
	point := a.Book.Record(now, prices)
	if point.Equity > a.PeakEquity {
		a.PeakEquity = point.Equity
	}
	a.Risk.Update(now, point.Equity, a.PeakEquity)

	if a.Risk.Halted {
		// Same kill switch as the backtest: flatten once, then block.
		if a.Book.Position(a.Symbol).IsOpen() {
			a.closePosition(now, price, "risk halt: "+a.Risk.HaltReason)
			res.Exited = true
		}
		res.Action = "risk_halt"
		res.Equity, res.Cash = point.Equity, a.Book.Cash
		res.Open = a.OpenTrade()
		return res, nil
	}

	// 3) Act on the decision at the live price.
	a.liveRebalance(now, target, stop, takeProfit, price, n, &res)

	res.Equity = a.Book.LastEquity()
	res.Cash = a.Book.Cash
	res.Open = a.OpenTrade()
	if res.Action == "" {
		res.Action = "hold"
	}
	return res, nil
}

// liveExits closes the position when the live price hits a protective level.
// It returns true when a position was closed. The long and short cases are
// mirrors: a long stops out when price falls to its (lower) stop and pays
// out when price rises to its (upper) target; a short is the opposite.
func (a *Agent) liveExits(now time.Time, price float64) bool {
	pos := a.Book.Position(a.Symbol)
	stop, target := pos.StopPrice, pos.TakeProfitPrice
	switch {
	case pos.Quantity > 0:
		if model.Valid(stop) && price <= stop {
			a.closePosition(now, price, "stop_loss")
			return true
		}
		if model.Valid(target) && price >= target {
			a.closePosition(now, price, "take_profit")
			return true
		}
	case pos.Quantity < 0:
		if model.Valid(stop) && price >= stop {
			a.closePosition(now, price, "stop_loss")
			return true
		}
		if model.Valid(target) && price <= target {
			a.closePosition(now, price, "take_profit")
			return true
		}
	}
	return false
}

// liveRebalance moves the position towards the target at the live price.
func (a *Agent) liveRebalance(now time.Time, target, strategyStop, strategyTarget, price float64, seriesLen int, res *StepResult) {
	pos := a.Book.Position(a.Symbol)
	desired := 0.0
	switch {
	case target > 0:
		desired = 1
	case target < 0:
		// Spot accounts cannot short. With AllowShort on (paper research
		// mode only) the engine keeps the mirror logic; otherwise the
		// signal is a no-op.
		if a.Config.Risk.AllowShort {
			desired = -1
		}
	}

	current := 0.0
	if pos.IsOpen() {
		current = math.Copysign(1, pos.Quantity)
	}
	if desired == current {
		return
	}

	if desired == 0 {
		if pos.IsOpen() {
			a.closePosition(now, price, "signal_exit")
			res.Exited = true
			res.Action = "signal_exit"
		}
		return
	}
	if seriesLen < a.Config.Backtest.WarmupBars {
		// Not enough history to open; the strategy is still warming up.
		return
	}

	decision := a.Risk.CanEnter(len(a.Book.OpenPositions()), pos.IsOpen())
	if !decision.Allowed {
		res.Action = "entry_blocked: " + decision.Reason
		return
	}

	side := broker.Buy
	sideName := "buy"
	if desired < 0 {
		side = broker.Sell
		sideName = "sell"
	}
	stopPrice, targetPrice := a.Risk.StopAndTarget(sideName, price, strategyStop, strategyTarget)

	quantity := a.Risk.PositionSize(
		a.Book.LastEquity(), a.Book.Cash, price, stopPrice, a.Config.Execution.LotSize,
	)
	if quantity <= 0 {
		res.Action = "entry_blocked: 无法计算下单数量（资金或风险额度不足）"
		return
	}

	reason := "entry_long"
	if side == broker.Sell {
		reason = "entry_short"
	}
	fill := a.Broker.MarketOrder(now, a.Symbol, side, quantity, price, reason)
	if fill.Rejected {
		res.Action = "order_rejected: " + fill.Reason
		return
	}

	a.Book.ApplyFill(fill)
	updated := a.Book.Position(a.Symbol)
	updated.StopPrice = stopPrice
	updated.TakeProfitPrice = targetPrice
	updated.OpenedAt = now
	a.open = &OpenTrade{
		EntryTime: now, EntryPrice: fill.Price,
		Quantity: fill.Quantity, Side: fill.Side, EntryFee: fill.Commission,
	}
	res.Entered = true
	res.Action = reason
}
