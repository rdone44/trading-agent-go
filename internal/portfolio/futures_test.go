package portfolio

import (
	"math"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
)

func TestFuturesMarginLongAndShortRoundTrip(t *testing.T) {
	for _, side := range []broker.Side{broker.Buy, broker.Sell} {
		t.Run(string(side), func(t *testing.T) {
			p := NewFutures(100, 5)
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

func TestSpotBaseFeeReducesInventoryNotCashTwice(t *testing.T) {
	p := New(10000)
	p.ApplyFill(broker.Fill{Symbol: "BTCUSDT", Side: broker.Buy, Quantity: .1, Price: 80000, Commission: 8, BaseCommission: .0001})
	if p.Cash != 2000 || math.Abs(p.Position("BTCUSDT").Quantity-.0999) > 1e-10 || math.Abs(p.Equity(map[string]float64{"BTCUSDT": 80000})-9992) > 1e-8 {
		t.Fatalf("base commission double counted: %+v", p)
	}
}
