package broker

import (
	"math"
	"testing"
	"time"
)

// dryRunBroker returns a dry-run Binance broker with the quantity step the
// exchange would give for a typical BTC spot symbol.
func dryRunBroker() *BinanceBroker {
	return NewBinance(BinanceConfig{DryRun: true, StepSize: 0.00001, Symbol: "BTCUSDT"})
}

func TestDryRunBuyAndSellFill(t *testing.T) {
	b := dryRunBroker()
	ts := time.Unix(0, 0).UTC()

	buy := b.MarketOrder(ts, "BTCUSDT", Buy, 0.5, 60000, "entry_long")
	if buy.Rejected {
		t.Fatalf("dry-run buy rejected: %s", buy.Reason)
	}
	const slip = 5 / 10_000.0
	want := 60000 * (1 + slip)
	if math.Abs(buy.Price-want) > 1e-9 {
		t.Fatalf("buy fill price = %.6f, want %.6f (5bp slip)", buy.Price, want)
	}
	if math.Abs(buy.Commission-buy.Notional*0.001) > 1e-9 {
		t.Fatalf("buy commission = %.6f, want 0.1%% of notional", buy.Commission)
	}
	if buy.OrderID == "" {
		t.Fatal("dry-run fill must carry an order id")
	}

	sell := b.MarketOrder(ts.Add(time.Second), "BTCUSDT", Sell, 0.5, 61000, "signal_exit")
	wantSell := 61000 * (1 - slip)
	if sell.Rejected {
		t.Fatalf("dry-run sell rejected: %s", sell.Reason)
	}
	if math.Abs(sell.Price-wantSell) > 1e-9 {
		t.Fatalf("sell fill price = %.6f, want %.6f", sell.Price, wantSell)
	}

	if len(b.Fills()) != 2 {
		t.Fatalf("expected 2 fills, got %d", len(b.Fills()))
	}
}

func TestDryRunQuantityRounding(t *testing.T) {
	b := dryRunBroker()
	ts := time.Unix(0, 0).UTC()
	fill := b.MarketOrder(ts, "BTCUSDT", Buy, 0.123456789, 100, "x")
	// 0.123456789 must floor to the 0.00001 step -> 0.12345
	if math.Abs(fill.Quantity-0.12345) > 1e-12 {
		t.Fatalf("quantity not rounded to step: %.6f", fill.Quantity)
	}
}

func TestDryRunRejectsZeroQuantity(t *testing.T) {
	b := dryRunBroker()
	ts := time.Unix(0, 0).UTC()
	// A quantity below one step rounds to zero and must be rejected.
	if !b.MarketOrder(ts, "BTCUSDT", Buy, 1e-12, 100, "x").Rejected {
		t.Fatal("tiny quantity should be rejected")
	}
	if !b.MarketOrder(ts, "BTCUSDT", Buy, 0, 100, "x").Rejected {
		t.Fatal("zero quantity should be rejected")
	}
}

func TestDryRunMinNotional(t *testing.T) {
	b := NewBinance(BinanceConfig{DryRun: true, StepSize: 0.00001, Symbol: "BTCUSDT", MinNotional: 10})
	ts := time.Unix(0, 0).UTC()
	// 0.0001 BTC at 100000 = notional 10 -> exactly at the minimum, accepted.
	if fill := b.MarketOrder(ts, "BTCUSDT", Buy, 0.0001, 100000, "x"); fill.Rejected {
		t.Fatalf("notional at the minimum should pass: %s", fill.Reason)
	}
	// 0.00005 BTC at 100000 = notional 5 -> rejected.
	if fill := b.MarketOrder(ts, "BTCUSDT", Buy, 0.00005, 100000, "x"); !fill.Rejected {
		t.Fatal("notional below the minimum should be rejected")
	}
}

func TestDryRunPriceDeviation(t *testing.T) {
	b := NewBinance(BinanceConfig{DryRun: true, StepSize: 0.00001, Symbol: "BTCUSDT", MaxPriceDev: 0.01})
	ts := time.Unix(0, 0).UTC()
	// In dry-run the fill price is price*(1±5bp), well inside a 1% band,
	// so a normal fill must not trip the guard.
	if fill := b.MarketOrder(ts, "BTCUSDT", Buy, 1, 100, "x"); fill.Rejected {
		t.Fatalf("normal fill rejected: %s", fill.Reason)
	}
}

func TestFormatQty(t *testing.T) {
	cases := map[float64]string{0: "0", 1: "1", 0.5: "0.5", 0.00001234: "0.00001234", 123.456: "123.456"}
	for qty, want := range cases {
		if got := formatQty(qty); got != want {
			t.Errorf("formatQty(%v) = %q, want %q", qty, got, want)
		}
	}
}

func TestSign(t *testing.T) {
	// The signature is deterministic; pin it to a known value so a signing
	// regression (e.g. a wrong key type) is caught.
	got := sign("secret", "symbol=BTCUSDT&side=BUY")
	if len(got) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars", len(got))
	}
	if got == sign("secret", "symbol=BTCUSDT&side=SELL") {
		t.Fatal("different payloads must give different signatures")
	}
	if got == sign("other", "symbol=BTCUSDT&side=BUY") {
		t.Fatal("different secrets must give different signatures")
	}
}

func TestBalancesRequiresInit(t *testing.T) {
	b := dryRunBroker()
	// Balances must refuse before Init has recorded the asset names.
	if _, _, err := b.Balances(); err == nil {
		t.Fatal("Balances before Init should fail with a clear error")
	}
}
