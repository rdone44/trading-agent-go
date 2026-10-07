package risk

import (
	"math"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// A gap can leave a strategy stop above the entry price. That level is
// unusable, and using it would skip the position-size risk cap entirely -
// which is exactly the bug a cross-language comparison caught (414 shares
// became 1180). The stop must fall back to the percentage stop.
func TestStopAboveEntryFallsBackToPercentageStop(t *testing.T) {
	m := New(config.Risk{StopLossPct: ptr(0.06), TakeProfitPct: ptr(0.18)})
	entry, strategyStop := 100.0, 120.0

	stop, _ := m.StopAndTarget("buy", entry, strategyStop, 0)

	if !(stop < entry) {
		t.Fatalf("stop = %v must sit below the entry price %v", stop, entry)
	}
	if math.Abs(stop-94.0) > 1e-9 {
		t.Fatalf("stop = %v, want the 6%% fallback at 94", stop)
	}
	// And the risk cap must actually bite now that the stop is usable.
	size := m.PositionSize(100_000, 100_000, entry, stop, 0.0001)
	if size > 17_000 {
		t.Fatalf("position size = %v, unexpectedly large: the risk cap was skipped", size)
	}
}

func TestTargetBelowEntryFallsBack(t *testing.T) {
	m := New(config.Risk{StopLossPct: ptr(0.06), TakeProfitPct: ptr(0.18)})
	_, target := m.StopAndTarget("buy", 100, 0, 90)
	if math.Abs(target-118.0) > 1e-9 {
		t.Fatalf("target = %v, want the 18%% fallback at 118", target)
	}
}

func TestShortLevelsFallBackWhenInverted(t *testing.T) {
	m := New(config.Risk{StopLossPct: ptr(0.06), TakeProfitPct: ptr(0.18)})
	stop, target := m.StopAndTarget("sell", 100, 80, 0) // stop below entry: unusable
	if !(stop > 100) {
		t.Fatalf("short stop = %v must sit above the entry price", stop)
	}
	if !(target < 100) {
		t.Fatalf("short target = %v must sit below the entry price", target)
	}
}
