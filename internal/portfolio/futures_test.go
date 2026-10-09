package portfolio

import (
	"math"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
)

func TestFuturesMarginLongAndShortRoundTrip(t *testing.T) {
	for _, side := range []broker.Side{broker.Buy, broker.Sell} {
		t.Run(string(side), func(t *testing.T) {
			p := New(100, 5)
			p.ApplyFill(broker.Fill{Symbol: "TEST", Side: side, Quantity: 4, Price: 100, Commission: 1})
			if p.Cash != 99 || p.AvailableCash() != 19 {
				t.Fatalf("wallet/margin wrong: %v/%v", p.Cash, p.AvailableCash())
			}
			price := 110.0
			exit := broker.Sell
			if side == broker.Sell {
				price = 90
				exit = broker.Buy
			}
			if p.Equity(map[string]float64{"TEST": price}) != 139 {
				t.Fatal("unrealized pnl is not added to wallet")
			}
			p.ApplyFill(broker.Fill{Symbol: "TEST", Side: exit, Quantity: 4, Price: price, Commission: 1})
			if p.Cash != 138 || p.AvailableCash() != 138 || p.Position("TEST").IsOpen() {
				t.Fatalf("bad round trip: %+v", p)
			}
		})
	}
}

// A perpetual taker fee is charged once, in USDT, out of the wallet. The
// retired spot venue's base-asset fee (reduce the received inventory instead
// of charging the quote twice) has no counterpart here: the exchange holds
// the inventory, so there is no inventory for a fee to reduce.
func TestCommissionChargedOnceInQuote(t *testing.T) {
	p := New(10000, 1)
	p.ApplyFill(broker.Fill{Symbol: "BTCUSDT", Side: broker.Buy, Quantity: .1, Price: 80000, Commission: 8})
	if p.Cash != 9992 {
		t.Fatalf("wallet = %v, want 9992 (one 8 USDT fee)", p.Cash)
	}
	if math.Abs(p.Position("BTCUSDT").Quantity-.1) > 1e-10 {
		t.Fatalf("quantity = %v, want the full 0.1 (no base fee to deduct)", p.Position("BTCUSDT").Quantity)
	}
	// Flat equity is the wallet plus unrealized PnL: the position is open, so
	// the mark-to-market gain is what the account is worth on top of the cash.
	if math.Abs(p.Equity(map[string]float64{"BTCUSDT": 81000})-(9992+100)) > 1e-8 {
		t.Fatalf("equity = %v, want wallet + unrealized", p.Equity(map[string]float64{"BTCUSDT": 81000}))
	}
}
