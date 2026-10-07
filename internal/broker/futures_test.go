package broker

import (
	"math"
	"strings"
	"testing"
	"time"
)

// dryFutures builds a fully-local futures broker: no keys, no network, fills
// simulated so the loop can be exercised in CI.
func dryFutures(leverage int, step float64) *FuturesBroker {
	return NewFutures(FuturesConfig{
		Symbol: "BTCUSDT", Leverage: leverage, MarginMode: "ISOLATED",
		DryRun: true, StepSize: step,
	})
}

func TestFuturesDryRunLongFill(t *testing.T) {
	b := dryFutures(5, 0.001)
	if err := b.Init(); err != nil {
		t.Fatalf("Init (dry-run, step set) = %v, want nil", err)
	}
	ts := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	fill := b.MarketOrder(ts, "BTCUSDT", Buy, 1.0, 100, "test_long")
	if fill.Rejected {
		t.Fatalf("long fill rejected: %s", fill.Reason)
	}
	// A taker long pays the 5 bp slippage on top of the reference price.
	if want := 100 * (1 + 5/10_000.0); math.Abs(fill.Price-want) > 1e-9 {
		t.Fatalf("fill price = %v, want %v", fill.Price, want)
	}
	if math.Abs(fill.Quantity-1.0) > 1e-9 {
		t.Fatalf("quantity = %v, want 1.0", fill.Quantity)
	}
	// Futures taker fee is ~0.04% of the notional.
	if want := fill.Quantity * fill.Price * 0.0004; math.Abs(fill.Commission-want) > 1e-9 {
		t.Fatalf("commission = %v, want %v", fill.Commission, want)
	}
	if !strings.HasPrefix(fill.OrderID, "fdry-") {
		t.Fatalf("dry-run order id = %q, want the fdry- prefix", fill.OrderID)
	}
}

func TestFuturesDryRunShortFill(t *testing.T) {
	b := dryFutures(5, 0.001)
	ts := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	// A sell on a perpetual is an open short: slippage works in our favour.
	fill := b.MarketOrder(ts, "BTCUSDT", Sell, 2.0, 200, "test_short")
	if fill.Rejected {
		t.Fatalf("short fill rejected: %s", fill.Reason)
	}
	if want := 200 * (1 - 5/10_000.0); math.Abs(fill.Price-want) > 1e-9 {
		t.Fatalf("short fill price = %v, want %v", fill.Price, want)
	}
}

func TestFuturesQuantityFloorsToStep(t *testing.T) {
	b := dryFutures(1, 0.001)
	fill := b.MarketOrder(time.Time{}, "BTCUSDT", Buy, 1.0049, 100, "step")
	if fill.Rejected {
		t.Fatalf("fill rejected: %s", fill.Reason)
	}
	if want := 1.004; math.Abs(fill.Quantity-want) > 1e-9 {
		t.Fatalf("quantity = %v, want %v (floored to the 0.001 step)", fill.Quantity, want)
	}
}

func TestFuturesZeroQuantityRejected(t *testing.T) {
	b := dryFutures(1, 0.001)
	// 0.0004 rounds down to 0 on a 0.001 step, so the order must be refused.
	fill := b.MarketOrder(time.Time{}, "BTCUSDT", Buy, 0.0004, 100, "tiny")
	if !fill.Rejected {
		t.Fatalf("expected a rejection for a sub-step quantity, got %+v", fill)
	}
}

func TestFuturesClientOrderIDsAreUnique(t *testing.T) {
	b := dryFutures(1, 0.001)
	first := b.nextOrderID("BTCUSDT", Buy)
	second := b.nextOrderID("BTCUSDT", Buy)
	if first == second {
		t.Fatalf("client order ids collide: %q", first)
	}
	// The id embeds the lower-cased side and the upper-cased venue symbol.
	if !strings.Contains(first, "buy") || !strings.Contains(first, "BTCUSDT") {
		t.Fatalf("client order id %q lacks the expected side/symbol", first)
	}
}

func TestFuturesAccessors(t *testing.T) {
	b := dryFutures(5, 0.001)
	if got := b.Leverage(); got != 5 {
		t.Fatalf("Leverage() = %d, want 5", got)
	}
	if got := b.MarginMode(); got != "ISOLATED" {
		t.Fatalf("MarginMode() = %q, want ISOLATED", got)
	}
	if !b.IsDryRun() {
		t.Fatal("IsDryRun() = false, want true")
	}
	// Leverage floors at 1 in the constructor.
	if got := NewFutures(FuturesConfig{Leverage: 0}).Leverage(); got != 1 {
		t.Fatalf("NewFutures floors Leverage to 1, got %d", got)
	}
}
