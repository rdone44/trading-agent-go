package risk

import (
	"math"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

func ptr(v float64) *float64 { return &v }

func TestPositionSizeRespectsRiskBudget(t *testing.T) {
	m := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 0.01})
	// 1% of 100k = 1000 risked, 10 dollars between entry and stop -> 100 shares.
	got := m.PositionSize(100_000, 100_000, 100, 90, 0.0001)
	if math.Abs(got-100) > 0.01 {
		t.Fatalf("quantity = %v, want 100", got)
	}
}

func TestPositionSizeCappedByPositionLimit(t *testing.T) {
	m := New(config.Risk{MaxPositionPct: 0.5, MaxRiskPerTradePct: 1})
	got := m.PositionSize(10_000, 10_000, 100, 99, 0.0001)
	if math.Abs(got-50) > 0.01 {
		t.Fatalf("quantity = %v, want 50", got)
	}
}

func TestDailyLossLimitBlocksEntries(t *testing.T) {
	m := New(config.Risk{MaxDailyLossPct: ptr(0.05), MaxDrawdownPct: ptr(0.5), MaxOpenPositions: 1})
	ts := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	m.Update(ts, 100_000, 100_000)
	m.Update(ts, 94_000, 100_000)
	if decision := m.CanEnter(0, false); decision.Allowed {
		t.Fatalf("entries should be blocked, got %+v", decision)
	}
}

func TestMaxDrawdownHaltsTrading(t *testing.T) {
	m := New(config.Risk{MaxDrawdownPct: ptr(0.10), MaxOpenPositions: 1})
	ts := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	m.Update(ts, 100_000, 100_000)
	m.Update(ts, 85_000, 100_000)
	if !m.Halted {
		t.Fatal("expected the kill switch to fire")
	}
	if m.CanEnter(0, false).Allowed {
		t.Fatal("halted manager must reject new entries")
	}
}

func TestStopAndTargetForLong(t *testing.T) {
	m := New(config.Risk{StopLossPct: ptr(0.05), TakeProfitPct: ptr(0.10)})
	stop, target := m.StopAndTarget("buy", 100, 97, 0)
	if stop != 97 {
		t.Fatalf("stop = %v, want the tighter strategy stop 97", stop)
	}
	if math.Abs(target-110) > 1e-9 {
		t.Fatalf("target = %v, want the fallback 110", target)
	}
}

// TestLeverageSizingScalesWithMultiplier checks that a larger leverage
// multiplier lets the same balance control more notional. The stop sits close
// to entry and the per-trade risk cap is set wide (5%) so the binding
// constraint at 1x is the posted margin, not the risk cap; the 5x run is then
// allowed to post ~5x the notional from the same balance.
func TestLeverageSizingScalesWithMultiplier(t *testing.T) {
	equity, cash, price, stop, lot := 100_000.0, 100_000.0, 100.0, 99.0, 0.0001
	// 5% risk cap -> 100_000*0.05/|100-99|*100 = 5_000_000 notional, wide open,
	// so the margin (cash*L) and the position budget are the real limits.

	one := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 0.05}).PositionSize(equity, cash, price, stop, lot)
	// 1x: notional = min(100_000, 99_900) = 99_900 -> 999 units.
	if want := 999.0; math.Abs(one-want) > 1.0 {
		t.Fatalf("1x quantity = %v, want %v", one, want)
	}

	five := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 0.05, Leverage: 5}).PositionSize(equity, cash, price, stop, lot)
	// 5x: notional = min(500_000, 99_900*5) = 499_500 -> 4995 units, ~5x.
	if want := 4995.0; math.Abs(five-want) > 1.0 {
		t.Fatalf("5x quantity = %v, want %v", five, want)
	}
	if five <= one {
		t.Fatalf("5x quantity %v must exceed 1x quantity %v", five, one)
	}
}

// TestLeverageUnlocksAMarginCappedPosition: with little free balance the
// 1x run is capped by cash*1, but the 5x run is capped by cash*5, so the
// same balance opens a five-fold larger position.
func TestLeverageUnlocksAMarginCappedPosition(t *testing.T) {
	equity, price, stop, lot := 100_000.0, 100.0, 90.0, 0.0001
	cash := 1_000.0 // a thin balance: 1x notional ~ 999, 5x ~ 4995

	one := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 1}).PositionSize(equity, cash, price, stop, lot)
	five := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 1, Leverage: 5}).PositionSize(equity, cash, price, stop, lot)

	// 1x is margin-capped at ~ cash*0.999 / price.
	if want := cash * 0.999 / price; math.Abs(one-want) > 1e-3 {
		t.Fatalf("1x quantity = %v, want ~%v (margin capped)", one, want)
	}
	// 5x is capped ~5x higher: the leverage effect.
	if want := cash * 0.999 * 5 / price; math.Abs(five-want) > 1e-3 {
		t.Fatalf("5x quantity = %v, want ~%v (leverage unlocks margin)", five, want)
	}
}

// TestShortPositionSizeCappedByRiskOnShortStop: for a short the stop sits
// above the entry; the risk cap still bounds the notional the same way.
func TestShortPositionSizeCappedByRiskOnShortStop(t *testing.T) {
	// entry 100, short stop 110 -> 10 dollars of adverse move per unit.
	// 2% of 100k = 2000 at risk -> 200 units, identical to the long mirror.
	m := New(config.Risk{MaxPositionPct: 1, MaxRiskPerTradePct: 0.02, Leverage: 5})
	got := m.PositionSize(100_000, 100_000, 100, 110, 0.0001)
	if math.Abs(got-200) > 1.0 {
		t.Fatalf("short quantity = %v, want 200 (risk capped by the upper stop)", got)
	}
}
