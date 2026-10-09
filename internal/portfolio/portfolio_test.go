package portfolio

import (
	"math"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
)

func fill(side broker.Side, qty, price float64) broker.Fill {
	return broker.Fill{
		Time: time.Now(), Symbol: "AAA", Side: side,
		Quantity: qty, Price: price, Notional: qty * price,
	}
}

func TestBuyThenSellRealizesPnL(t *testing.T) {
	p := New(10_000, 1)
	p.ApplyFill(fill(broker.Buy, 10, 100))
	// A perpetual entry does not spend the wallet: it posts margin. The cash
	// stays put, and the margin is what the sizing layer may no longer use.
	if p.Cash != 10_000 {
		t.Fatalf("cash = %v, want 10000 (margin is reserved, not spent)", p.Cash)
	}
	if got := p.AvailableCash(); got != 9_000 {
		t.Fatalf("available cash = %v, want 9000 (1000 posted as margin)", got)
	}
	if got := p.Position("AAA").Quantity; got != 10 {
		t.Fatalf("quantity = %v, want 10", got)
	}

	p.ApplyFill(fill(broker.Sell, 10, 110))
	if p.Cash != 10_100 {
		t.Fatalf("cash = %v, want 10100", p.Cash)
	}
	if p.AvailableCash() != 10_100 || p.MarginUsed() != 0 {
		t.Fatalf("closing the trade must release the margin: available=%v margin=%v",
			p.AvailableCash(), p.MarginUsed())
	}
	if p.Position("AAA").IsOpen() {
		t.Fatal("position should be flat")
	}
	if math.Abs(p.RealizedPnL-100) > 1e-9 {
		t.Fatalf("realized pnl = %v, want 100", p.RealizedPnL)
	}
}

func TestShortThenCover(t *testing.T) {
	p := New(10_000, 1)
	p.ApplyFill(fill(broker.Sell, 5, 200))
	// Shorting a perpetual is the same trade as long: margin in, no wallet
	// movement. The proceeds of a spot sale do not exist here.
	if p.Cash != 10_000 {
		t.Fatalf("cash = %v, want 10000", p.Cash)
	}
	if got := p.AvailableCash(); got != 9_000 {
		t.Fatalf("available cash = %v, want 9000", got)
	}
	if got := p.Position("AAA").Quantity; got != -5 {
		t.Fatalf("quantity = %v, want -5", got)
	}
	p.ApplyFill(fill(broker.Buy, 5, 190))
	if p.Position("AAA").IsOpen() {
		t.Fatal("position should be flat")
	}
	if p.Cash != 10_050 {
		t.Fatalf("cash = %v, want 10050 (50 of realized pnl, margin released)", p.Cash)
	}
	if math.Abs(p.RealizedPnL-50) > 1e-9 {
		t.Fatalf("realized pnl = %v, want 50", p.RealizedPnL)
	}
}

func TestEquityEqualsCashPlusMarketValue(t *testing.T) {
	p := New(10_000, 1)
	p.ApplyFill(fill(broker.Buy, 10, 100))
	prices := map[string]float64{"AAA": 120}
	// Wallet plus unrealized PnL: 10000 + 10*(120-100). The margin posted at
	// entry is still in the wallet, so it is not counted twice.
	if got, want := p.Equity(prices), 10_000.0+200.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("equity = %v, want %v", got, want)
	}
}

func TestRejectedFillIsIgnored(t *testing.T) {
	p := New(10_000, 1)
	p.ApplyFill(broker.Fill{Symbol: "AAA", Side: broker.Buy, Quantity: 0, Rejected: true})
	if p.Cash != 10_000 || len(p.OpenPositions()) != 0 {
		t.Fatal("a rejected fill must not change the portfolio")
	}
}
