package broker

import (
	"math"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
)

func testBroker(overrides config.Execution) *PaperBroker {
	settings := config.Execution{CommissionBps: 0, SlippageBps: 0, MinTradeNotional: 0, LotSize: 0.0001}
	if overrides.CommissionBps != 0 {
		settings.CommissionBps = overrides.CommissionBps
	}
	if overrides.SlippageBps != 0 {
		settings.SlippageBps = overrides.SlippageBps
	}
	if overrides.MinTradeNotional != 0 {
		settings.MinTradeNotional = overrides.MinTradeNotional
	}
	return New(settings)
}

func TestCommissionAndSlippage(t *testing.T) {
	b := testBroker(config.Execution{CommissionBps: 10, SlippageBps: 50})
	fill := b.MarketOrder(time.Now(), "AAA", Buy, 10, 100, "test")
	if math.Abs(fill.Price-100.5) > 1e-9 {
		t.Fatalf("fill price = %v, want 100.5 (50 bp slippage)", fill.Price)
	}
	wantCommission := 10 * 100.5 * 0.001
	if math.Abs(fill.Commission-wantCommission) > 1e-9 {
		t.Fatalf("commission = %v, want %v", fill.Commission, wantCommission)
	}
}

func TestSellSlippageWorksAgainstYou(t *testing.T) {
	b := testBroker(config.Execution{SlippageBps: 50})
	fill := b.MarketOrder(time.Now(), "AAA", Sell, 10, 100, "test")
	if math.Abs(fill.Price-99.5) > 1e-9 {
		t.Fatalf("sell fill price = %v, want 99.5", fill.Price)
	}
}

func TestSmallOrderRejected(t *testing.T) {
	b := testBroker(config.Execution{MinTradeNotional: 10_000})
	fill := b.MarketOrder(time.Now(), "AAA", Buy, 1, 100, "test")
	if !fill.Rejected || fill.Quantity != 0 {
		t.Fatalf("expected a rejection, got %+v", fill)
	}
}

func TestLotRounding(t *testing.T) {
	b := testBroker(config.Execution{})
	fill := b.MarketOrder(time.Now(), "AAA", Buy, 10.123456, 100, "test")
	if fill.Quantity > 10.123456 {
		t.Fatalf("quantity %v should not round up past the request", fill.Quantity)
	}
	if math.Abs(fill.Quantity*10000-math.Round(fill.Quantity*10000)) > 1e-6 {
		t.Fatalf("quantity %v is not a whole number of lots", fill.Quantity)
	}
}
