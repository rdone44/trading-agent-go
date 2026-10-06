package portfolio

import (
	"math"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/broker"
)

func fill(side broker.Side, qty, price float64) broker.Fill {
	return broker.Fill{
		Time: time.Now(), Symbol: "AAA", Side: side,
		Quantity: qty, Price: price, Notional: qty * price,
	}
}

func TestBuyThenSellRealizesPnL(t *testing.T) {
	p := New(10_000)
	p.ApplyFill(fill(broker.Buy, 10, 100))
	if p.Cash != 9_000 {
		t.Fatalf("cash = %v, want 9000", p.Cash)
	}
	if got := p.Position("AAA").Quantity; got != 10 {
		t.Fatalf("quantity = %v, want 10", got)
	}

	p.ApplyFill(fill(broker.Sell, 10, 110))
	if p.Cash != 10_100 {
		t.Fatalf("cash = %v, want 10100", p.Cash)
	}
	if p.Position("AAA").IsOpen() {
		t.Fatal("position should be flat")
	}
	if math.Abs(p.RealizedPnL-100) > 1e-9 {
		t.Fatalf("realized pnl = %v, want 100", p.RealizedPnL)
	}
}

func TestShortThenCover(t *testing.T) {
	p := New(10_000)
	p.ApplyFill(fill(broker.Sell, 5, 200))
	if p.Cash != 11_000 {
		t.Fatalf("cash = %v, want 11000", p.Cash)
	}
	if got := p.Position("AAA").Quantity; got != -5 {
		t.Fatalf("quantity = %v, want -5", got)
	}
	p.ApplyFill(fill(broker.Buy, 5, 190))
	if p.Position("AAA").IsOpen() {
		t.Fatal("position should be flat")
	}
	if math.Abs(p.RealizedPnL-50) > 1e-9 {
		t.Fatalf("realized pnl = %v, want 50", p.RealizedPnL)
	}
}

func TestEquityEqualsCashPlusMarketValue(t *testing.T) {
	p := New(10_000)
	p.ApplyFill(fill(broker.Buy, 10, 100))
	prices := map[string]float64{"AAA": 120}
	if got, want := p.Equity(prices), 9_000.0+1_200.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("equity = %v, want %v", got, want)
	}
}

func TestRejectedFillIsIgnored(t *testing.T) {
	p := New(10_000)
	p.ApplyFill(broker.Fill{Symbol: "AAA", Side: broker.Buy, Quantity: 0, Rejected: true})
	if p.Cash != 10_000 || len(p.OpenPositions()) != 0 {
		t.Fatal("a rejected fill must not change the portfolio")
	}
}
