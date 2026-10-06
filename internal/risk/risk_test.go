package risk

import (
	"math"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
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
