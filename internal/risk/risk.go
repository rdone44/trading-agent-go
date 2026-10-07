// Package risk enforces position sizing, protective stops and portfolio-level
// kill switches.
package risk

import (
	"fmt"
	"math"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// Decision is the answer to "may I open a new position?".
type Decision struct {
	Allowed bool
	Reason  string
}

// Event records a limit breach for the report.
type Event struct {
	Time   time.Time
	Type   string
	Reason string
}

// Manager holds the risk state across a run.
type Manager struct {
	Settings          config.Risk
	Halted            bool
	HaltReason        string
	Events            []Event
	day               time.Time
	dayStartEquity    float64
	entriesBlockedDay bool
}

func New(settings config.Risk) *Manager {
	return &Manager{Settings: settings}
}

// Update is called on every bar; it flips the kill switch when a limit breaks.
func (m *Manager) Update(ts time.Time, equity, peakEquity float64) {
	day := time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, ts.Location())
	if !day.Equal(m.day) {
		m.day = day
		m.dayStartEquity = equity
		m.entriesBlockedDay = false
	}

	if limit := m.Settings.MaxDrawdownPct; limit != nil && peakEquity > 0 {
		drawdown := equity/peakEquity - 1
		if drawdown <= -math.Abs(*limit) && !m.Halted {
			m.Halted = true
			m.HaltReason = "max drawdown breached (" + pct(drawdown) + ")"
			m.Events = append(m.Events, Event{Time: ts, Type: "halt", Reason: m.HaltReason})
		}
	}

	if limit := m.Settings.MaxDailyLossPct; limit != nil && m.dayStartEquity > 0 {
		dayReturn := equity/m.dayStartEquity - 1
		if dayReturn <= -math.Abs(*limit) && !m.entriesBlockedDay {
			m.entriesBlockedDay = true
			m.Events = append(m.Events, Event{
				Time: ts, Type: "daily_loss_limit",
				Reason: "daily loss " + pct(dayReturn) + ", entries paused",
			})
		}
	}
}

// CanEnter reports whether a new position may be opened.
func (m *Manager) CanEnter(openPositions int, sameSymbol bool) Decision {
	switch {
	case m.Halted:
		return Decision{false, "trading halted: " + m.HaltReason}
	case m.entriesBlockedDay:
		return Decision{false, "daily loss limit reached"}
	case sameSymbol:
		return Decision{true, ""}
	case openPositions >= max(m.Settings.MaxOpenPositions, 1):
		return Decision{false, "max open positions reached"}
	}
	return Decision{true, ""}
}

// EntriesBlockedToday exposes the daily-loss pause to the engine.
func (m *Manager) EntriesBlockedToday() bool { return m.entriesBlockedDay }

// RiskState is the serializable subset of a Manager, so a live session can
// survive a restart: a halted risk manager stays halted, and a daily-loss
// pause keeps blocking entries for the rest of that day.
type RiskState struct {
	Halted            bool
	HaltReason        string
	Day               time.Time
	DayStartEquity    float64
	EntriesBlockedDay bool
}

// Snapshot captures the manager's mutable state.
func (m *Manager) Snapshot() RiskState {
	return RiskState{
		Halted:            m.Halted,
		HaltReason:        m.HaltReason,
		Day:               m.day,
		DayStartEquity:    m.dayStartEquity,
		EntriesBlockedDay: m.entriesBlockedDay,
	}
}

// Restore puts a manager back into the state another process persisted. The
// event log is intentionally not restored: it is a per-run report artifact.
func (m *Manager) Restore(s RiskState) {
	m.Halted = s.Halted
	m.HaltReason = s.HaltReason
	m.day = s.Day
	m.dayStartEquity = s.DayStartEquity
	m.entriesBlockedDay = s.EntriesBlockedDay
}

// PositionSize implements the fixed-fractional rule, leverage-aware.
//
//	quantity = notional / price
//
// where notional is the smallest of:
//
//   - equity × max_position_pct × leverage   (the account's position budget),
//   - equity × max_risk_per_trade_pct / stop distance (fixed-fractional risk cap),
//   - cash × leverage                         (margin a balance can post).
//
// With leverage 1 the first and third terms collapse to the historical
// spot behaviour (full notional, cash-constrained), so backtests and spot
// runs are unchanged. With leverage > 1 the same risk budget controls more
// notional and a small balance can still open a full position — that is the
// leverage effect. The risk cap is expressed in dollars lost at the stop,
// so the per-trade risk does not grow just because leverage does.
func (m *Manager) PositionSize(equity, cash, price, stopPrice, lotSize float64) float64 {
	if price <= 0 || equity <= 0 {
		return 0
	}
	leverage := float64(m.Settings.Leverage)
	if leverage < 1 {
		leverage = 1
	}

	budget := equity * m.Settings.MaxPositionPct * leverage
	if stopPrice > 0 {
		riskPerShare := math.Abs(price - stopPrice)
		if riskPerShare > 0 {
			riskBudget := equity * m.Settings.MaxRiskPerTradePct
			budget = math.Min(budget, riskBudget/riskPerShare*price)
		}
	}
	affordable := math.Max(cash, 0) * 0.999
	notional := math.Min(budget, affordable*leverage)
	quantity := notional / price
	lot := lotSize
	if lot <= 0 {
		lot = 1e-9
	}
	return math.Max(math.Floor(quantity/lot)*lot, 0)
}

// StopAndTarget combines strategy levels with the fallback percentage stops.
// A zero stop/target means "the strategy did not provide one".
func (m *Manager) StopAndTarget(side string, price, strategyStop, strategyTarget float64) (float64, float64) {
	fallbackStop := 0.0
	if m.Settings.StopLossPct != nil {
		if side == "buy" {
			fallbackStop = price * (1 - *m.Settings.StopLossPct)
		} else {
			fallbackStop = price * (1 + *m.Settings.StopLossPct)
		}
	}
	fallbackTarget := 0.0
	if m.Settings.TakeProfitPct != nil {
		if side == "buy" {
			fallbackTarget = price * (1 + *m.Settings.TakeProfitPct)
		} else {
			fallbackTarget = price * (1 - *m.Settings.TakeProfitPct)
		}
	}

	stop := pick(strategyStop, fallbackStop)
	target := pick(strategyTarget, fallbackTarget)
	if side == "buy" {
		// Prefer the tighter (higher) stop and the nearer (lower) target.
		stop = tighter(stop, fallbackStop, true)
		target = tighter(target, fallbackTarget, false)
		// A gap can leave a strategy level on the wrong side of the entry
		// price. Such a level is unusable: fall back to the percentage stop,
		// otherwise the position-size risk cap would be silently skipped.
		if !(stop > 0 && stop < price) {
			stop = fallbackStop
		}
		if !(target > price) {
			target = fallbackTarget
		}
	} else {
		stop = tighter(stop, fallbackStop, false)
		target = tighter(target, fallbackTarget, true)
		if !(stop > price) {
			stop = fallbackStop
		}
		if !(target > 0 && target < price) {
			target = fallbackTarget
		}
	}
	return stop, target
}

func pick(primary, fallback float64) float64 {
	if primary > 0 {
		return primary
	}
	return fallback
}

// tighter returns the more conservative of two levels: for a long position the
// higher stop / lower target, and the mirror for a short.
func tighter(a, b float64, preferHigher bool) float64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if preferHigher {
		return math.Max(a, b)
	}
	return math.Min(a, b)
}

func pct(v float64) string {
	return fmt.Sprintf("%.2f%%", v*100)
}
