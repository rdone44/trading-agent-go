// Live trading primitives: one decision cycle against the live market,
// reusing the protective-exit, risk and sizing logic of the backtest loop.
package engine

import (
	"fmt"
	"math"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// StepResult describes what one live cycle did.
type StepResult struct {
	Exited  bool
	Entered bool
	Action  string // human-readable outcome, e.g. "hold", "entry_long", "stop_loss"
	Reason  string // the strategy's own one-line explanation of this call; the
	// LLM strategy returns the model's words, others stay empty
	Equity    float64
	Cash      float64
	Open      *OpenTrade
	MarkPrice float64
}

func (a *Agent) liveResult(res StepResult, price float64) StepResult {
	res.Equity = a.Book.Equity(map[string]float64{a.Symbol: price})
	res.Cash, res.Open, res.MarkPrice = a.Book.AvailableCash(), a.OpenTrade(), price
	if res.Action == "" {
		if res.Open == nil {
			res.Action = "flat"
		} else {
			res.Action = "hold"
		}
	}
	return res
}

// Protect is independent of history and strategy availability. An uncertain
// order needs reconciliation, not another attempt to flatten the same book.
func (a *Agent) Protect(price float64, now time.Time) (StepResult, error) {
	if !model.Valid(price) || price <= 0 {
		return StepResult{}, fmt.Errorf("live 价格无效: %v", price)
	}
	res := StepResult{MarkPrice: price}
	if a.Risk.OrderUncertain {
		return a.liveResult(res, price), fmt.Errorf("订单待核对：%s", a.Risk.HaltReason)
	}
	before := len(a.Broker.Fills())
	if a.Book.Position(a.Symbol).IsOpen() && a.liveExits(now, price) {
		res.Exited = true
		res.Action = a.Broker.Fills()[len(a.Broker.Fills())-1].Reason
	}
	point := a.Book.Record(now, map[string]float64{a.Symbol: price})
	if point.Equity > a.PeakEquity {
		a.PeakEquity = point.Equity
	}
	a.Risk.Update(now, point.Equity, a.PeakEquity)
	if a.Risk.Halted && !a.Risk.OrderUncertain {
		if a.Book.Position(a.Symbol).IsOpen() {
			a.closePosition(now, price, "risk halt: "+a.Risk.HaltReason)
			res.Exited = !a.Book.Position(a.Symbol).IsOpen()
		}
		res.Action = "risk_halt"
	}
	res = a.liveResult(res, price)
	if len(a.Broker.Fills()) > before {
		fill := a.Broker.Fills()[len(a.Broker.Fills())-1]
		if fill.Uncertain || fill.Rejected {
			return res, fmt.Errorf("退出未确认：%s", fill.Reason)
		}
	}
	return res, nil
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
	if series.Symbol != "" && series.Symbol != a.Symbol {
		return StepResult{}, fmt.Errorf("行情交易对 %s 与持仓 %s 不一致", series.Symbol, a.Symbol)
	}
	res, err := a.Protect(price, now)
	if err != nil {
		return res, err
	}
	if res.Exited || a.Risk.Halted {
		return res, nil
	}
	return a.Decide(series, price, now, res)
}

// Decide is called only after the independent protective pass. The runner
// fetches history afterwards so a slow download cannot delay a known stop.
func (a *Agent) Decide(series model.Series, price float64, now time.Time, res StepResult) (StepResult, error) {
	if series.Len() == 0 {
		return res, fmt.Errorf("行情数据为空")
	}
	if !(price > 0) {
		return StepResult{}, fmt.Errorf("live 价格无效: %v", price)
	}
	if series.Symbol != a.Symbol {
		return res, fmt.Errorf("行情交易对 %s 与持仓 %s 不一致", series.Symbol, a.Symbol)
	}

	// Strategies that are cheap over a whole series (the indicator ones)
	// regenerate the full signal; expensive ones (the LLM) expose a
	// single-decision path so a poll bills the model once instead of O(bars)
	// times. Both yield the same (target, stop, target-price) triple.
	n := series.Len()
	var target, stop, takeProfit float64
	var reason string
	if ld, ok := a.Strategy.(strategy.LiveDecision); ok {
		var decided bool
		target, stop, takeProfit, reason, decided = ld.LastDecision(series, a.Config)
		if !decided {
			// The strategy could not answer (model missing, unreachable or
			// unparseable). Holding the current position is the only safe
			// reading: treating it as a flat target would liquidate a
			// healthy position on an outage. Protective exits already ran.
			res = a.liveResult(res, price)
			res.Action = "ai_unavailable"
			return res, nil
		}
	} else {
		signals, err := a.Strategy.Generate(series, a.Config)
		if err != nil {
			return res, err
		}
		signals = signals.Normalize(n)
		target = signals.Signal[n-1]
		stop = signals.Stop[n-1]
		takeProfit = signals.TakeProfit[n-1]
	}

	// 3) Act on the decision at the live price.
	before := len(a.Broker.Fills())
	res.Reason = reason
	a.liveRebalance(now, target, stop, takeProfit, price, n, &res)
	res = a.liveResult(res, price)
	if curve := a.Book.Curve; len(curve) > 0 {
		last := &curve[len(curve)-1]
		last.Cash, last.MarketValue, last.Equity = a.Book.Cash, a.Book.MarketValue(map[string]float64{a.Symbol: price}), res.Equity
		last.OpenPositions = len(a.Book.OpenPositions())
	}
	if len(a.Broker.Fills()) > before {
		fill := a.Broker.Fills()[len(a.Broker.Fills())-1]
		if fill.Rejected || fill.Uncertain {
			return res, fmt.Errorf("订单异常：%s", fill.Reason)
		}
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
		if model.Valid(stop) && stop > 0 && price <= stop {
			a.closePosition(now, price, "stop_loss")
			return !a.Book.Position(a.Symbol).IsOpen()
		}
		if model.Valid(target) && target > 0 && price >= target {
			a.closePosition(now, price, "take_profit")
			return !a.Book.Position(a.Symbol).IsOpen()
		}
	case pos.Quantity < 0:
		if model.Valid(stop) && stop > 0 && price >= stop {
			a.closePosition(now, price, "stop_loss")
			return !a.Book.Position(a.Symbol).IsOpen()
		}
		if model.Valid(target) && target > 0 && price <= target {
			a.closePosition(now, price, "take_profit")
			return !a.Book.Position(a.Symbol).IsOpen()
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
	if desired != 0 && seriesLen < a.Config.Backtest.WarmupBars {
		return
	}
	if current != 0 && desired != 0 {
		a.closePosition(now, price, "signal_exit")
		res.Exited = !a.Book.Position(a.Symbol).IsOpen()
		res.Action = "signal_exit"
		return
	}

	if desired == 0 {
		if pos.IsOpen() {
			a.closePosition(now, price, "signal_exit")
			res.Exited = !a.Book.Position(a.Symbol).IsOpen()
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
		a.Book.Equity(map[string]float64{a.Symbol: price}), a.Book.AvailableCash(), price, stopPrice, a.Config.Execution.LotSize,
	)
	if quantity <= 0 {
		res.Action = "entry_blocked: 无法计算下单数量（资金或风险额度不足）"
		return
	}

	reason := "entry_long"
	if side == broker.Sell {
		reason = "entry_short"
	}

	// Second-opinion gate: a nil or disabled veto is a no-op. It only ever
	// objects to a *new* entry; protective exits above are never gated, so
	// a veto can never block a stop-loss or a risk-halt flatten.
	if a.Veto != nil {
		leverage := a.Config.Risk.Leverage
		if leverage < 1 {
			leverage = 1
		}
		blocked, vetoReason := a.Veto(now, VetoContext{
			Symbol: a.Symbol, Side: side, Quantity: quantity,
			EntryPrice: price, StopPrice: stopPrice, TargetPrice: targetPrice,
			Equity: a.Book.LastEquity(), Cash: a.Book.Cash,
			Leverage: leverage, Reason: reason,
		})
		if blocked {
			res.Action = "veto_blocked: " + vetoReason
			return
		}
	}

	fill := a.Broker.MarketOrder(now, a.Symbol, side, quantity, price, reason)
	if fill.Uncertain {
		a.Risk.RequireReconciliation(fill.Reason)
	}
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
	a.open.Quantity = math.Abs(updated.Quantity)
	// Exchange-side protective orders: the position is now open on the venue,
	// so cover it. stopPrice/targetPrice are the exact levels the risk engine
	// used for sizing — a single source, never recomputed at the venue.
	// A placement failure does not make the confirmed entry fill uncertain.
	// Halt new entries and let the next protective pass flatten the position.
	// closePosition must first confirm cancellation of any partially placed
	// legs; cancellation or exit uncertainty still requires reconciliation.
	if a.Protective != nil {
		if err := a.Protective.Open(fill.Side, stopPrice, targetPrice); err != nil && !a.Risk.OrderUncertain {
			a.Risk.Halted = true
			a.Risk.HaltReason = "交易所侧保护单挂单失败：" + err.Error()
		}
	}
	res.Entered = true
	res.Action = reason
}
