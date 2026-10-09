// Package broker implements a paper broker: market orders, commission,
// slippage and lot rounding.
package broker

import (
	"fmt"
	"math"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// Side is the direction of an order.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Fill is the result of one executed (or rejected) order.
type Fill struct {
	Time       time.Time
	Symbol     string
	Side       Side
	Quantity   float64
	Price      float64
	Commission float64
	Notional   float64
	Reason     string
	Rejected   bool
	// OrderID is the exchange order identifier; empty for paper fills.
	OrderID       string
	ClientOrderID string
	Status        string
	// Uncertain means an order may exist at the exchange; never retry it blindly.
	Uncertain bool
}

// Broker is anything that can place market orders: the paper broker used by
// backtests, and the live Binance broker used by the trade loop.
type Broker interface {
	// MarketOrder places a market order at the given reference price and
	// returns the (possibly rejected) fill.
	MarketOrder(ts time.Time, symbol string, side Side, quantity, price float64, reason string) Fill
	// Fills lists every fill so far, including rejections.
	Fills() []Fill
}

// PaperBroker fills market orders with commission and slippage. Cash
// accounting is delegated to the portfolio, which keeps this type trivial to
// test.
type PaperBroker struct {
	Settings       config.Execution
	CommissionPaid float64
	Trades         []Fill
}

func New(settings config.Execution) *PaperBroker {
	return &PaperBroker{Settings: settings}
}

// Fills implements Broker; paper fills accumulate in Trades.
func (b *PaperBroker) Fills() []Fill { return b.Trades }

// MarketOrder submits a market order at the given reference price.
func (b *PaperBroker) MarketOrder(ts time.Time, symbol string, side Side, quantity, price float64, reason string) Fill {
	quantity = b.roundQuantity(quantity)
	if quantity <= 0 || math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
		return b.reject(ts, symbol, side, price, "invalid quantity or price", reason)
	}

	slip := b.Settings.SlippageBps / 10_000.0
	fillPrice := price * (1 + slip)
	if side == Sell {
		fillPrice = price * (1 - slip)
	}
	notional := quantity * fillPrice
	if notional < b.Settings.MinTradeNotional {
		return b.reject(ts, symbol, side, fillPrice,
			fmt.Sprintf("notional %.2f < min_trade_notional %.2f", notional, b.Settings.MinTradeNotional), reason)
	}

	commission := notional * (b.Settings.CommissionBps / 10_000.0)
	fill := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: quantity,
		Price: fillPrice, Commission: commission, Notional: notional, Reason: reason,
		Status: "filled",
	}
	b.CommissionPaid += commission
	b.Trades = append(b.Trades, fill)
	return fill
}

// roundQuantity floors the size to the configured lot size.
func (b *PaperBroker) roundQuantity(quantity float64) float64 {
	lot := b.Settings.LotSize
	if lot <= 0 {
		lot = 1e-9
	}
	return math.Floor(math.Abs(quantity)/lot+1e-9) * lot
}

func (b *PaperBroker) reject(ts time.Time, symbol string, side Side, price float64, message, reason string) Fill {
	full := message
	if reason != "" {
		full = fmt.Sprintf("%s (%s)", message, reason)
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		price = 0
	}
	fill := Fill{
		Time: ts, Symbol: symbol, Side: side, Price: price,
		Reason: full, Rejected: true, Status: "rejected",
	}
	b.Trades = append(b.Trades, fill)
	return fill
}
