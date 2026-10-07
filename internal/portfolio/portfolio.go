// Package portfolio tracks cash, positions and the mark-to-market equity curve.
package portfolio

import (
	"math"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
)

// Position is a signed holding: positive is long, negative is short.
type Position struct {
	Symbol          string
	Quantity        float64
	AvgPrice        float64
	OpenedAt        time.Time
	StopPrice       float64 // NaN when unset
	TakeProfitPrice float64 // NaN when unset
}

func (p Position) IsOpen() bool { return math.Abs(p.Quantity) > 1e-12 }

func (p Position) Unrealized(price float64) float64 {
	return p.Quantity * (price - p.AvgPrice)
}

// EquityPoint is one row of the equity curve.
type EquityPoint struct {
	Time          time.Time
	Cash          float64
	MarketValue   float64
	Equity        float64
	RealizedPnL   float64
	OpenPositions int
}

// Portfolio is the single source of truth for cash and holdings.
type Portfolio struct {
	InitialCash float64
	Cash        float64
	Positions   map[string]*Position
	RealizedPnL float64
	Curve       []EquityPoint
	Futures     bool
	Leverage    int
}

func NewFutures(initialCash float64, leverage int) *Portfolio {
	p := New(initialCash)
	p.Futures, p.Leverage = true, max(leverage, 1)
	return p
}

// Cash is the wallet for futures, not sale proceeds. Entry margin is reserved
// separately and released as the signed position shrinks.
func (p *Portfolio) AvailableCash() float64 {
	if !p.Futures {
		return math.Max(p.Cash, 0)
	}
	margin := 0.0
	for _, pos := range p.OpenPositions() {
		margin += math.Abs(pos.Quantity) * pos.AvgPrice / float64(max(p.Leverage, 1))
	}
	return math.Max(p.Cash-margin, 0)
}

func (p *Portfolio) MarginUsed() float64 {
	if !p.Futures {
		return 0
	}
	return p.Cash - p.AvailableCash()
}

func New(initialCash float64) *Portfolio {
	return &Portfolio{
		InitialCash: initialCash,
		Cash:        initialCash,
		Positions:   map[string]*Position{},
	}
}

// Position returns the (possibly empty) position for a symbol, creating it on
// first use.
func (p *Portfolio) Position(symbol string) *Position {
	pos, ok := p.Positions[symbol]
	if !ok {
		pos = &Position{Symbol: symbol, StopPrice: math.NaN(), TakeProfitPrice: math.NaN()}
		p.Positions[symbol] = pos
	}
	return pos
}

func (p *Portfolio) OpenPositions() []*Position {
	out := make([]*Position, 0, len(p.Positions))
	for _, pos := range p.Positions {
		if pos.IsOpen() {
			out = append(out, pos)
		}
	}
	return out
}

func (p *Portfolio) MarketValue(prices map[string]float64) float64 {
	total := 0.0
	for symbol, pos := range p.Positions {
		if !pos.IsOpen() {
			continue
		}
		price, ok := prices[symbol]
		if !ok {
			price = pos.AvgPrice
		}
		if p.Futures {
			total += pos.Unrealized(price)
		} else {
			total += pos.Quantity * price
		}
	}
	return total
}

func (p *Portfolio) Equity(prices map[string]float64) float64 {
	return p.Cash + p.MarketValue(prices)
}

// Record appends the current mark-to-market state to the equity curve.
func (p *Portfolio) Record(ts time.Time, prices map[string]float64) EquityPoint {
	point := EquityPoint{
		Time:          ts,
		Cash:          p.Cash,
		MarketValue:   p.MarketValue(prices),
		RealizedPnL:   p.RealizedPnL,
		OpenPositions: len(p.OpenPositions()),
	}
	point.Equity = point.Cash + point.MarketValue
	p.Curve = append(p.Curve, point)
	return point
}

// LastEquity returns the most recent recorded equity, or the initial cash.
func (p *Portfolio) LastEquity() float64 {
	if len(p.Curve) == 0 {
		return p.InitialCash
	}
	return p.Curve[len(p.Curve)-1].Equity
}

// ApplyFill updates cash, position size and realized PnL from a broker fill.
func (p *Portfolio) ApplyFill(fill broker.Fill) {
	if fill.Rejected || fill.Quantity <= 0 {
		return
	}
	pos := p.Position(fill.Symbol)
	signed := fill.Quantity
	if fill.Side == broker.Sell {
		signed = -fill.Quantity
	}
	if p.Futures {
		if !sameSign(pos.Quantity, signed) && pos.IsOpen() {
			closed := math.Min(math.Abs(signed), math.Abs(pos.Quantity))
			p.Cash += closed * (fill.Price - pos.AvgPrice) * math.Copysign(1, pos.Quantity)
		}
		p.Cash -= fill.Commission
	} else {
		p.Cash -= signed * fill.Price
		// Base-asset fees reduce received inventory instead of charging USDT twice.
		p.Cash -= math.Max(fill.Commission-fill.BaseCommission*fill.Price, 0)
		signed -= fill.BaseCommission
	}

	oldQty := pos.Quantity
	newQty := oldQty + signed

	switch {
	case math.Abs(oldQty) < 1e-12:
		pos.AvgPrice = fill.Price
	case sameSign(oldQty, signed):
		totalCost := math.Abs(oldQty)*pos.AvgPrice + math.Abs(signed)*fill.Price
		if math.Abs(newQty) > 1e-12 {
			pos.AvgPrice = totalCost / math.Abs(newQty)
		} else {
			pos.AvgPrice = 0
		}
	default:
		closed := math.Min(math.Abs(signed), math.Abs(oldQty))
		direction := 1.0
		if oldQty < 0 {
			direction = -1.0
		}
		p.RealizedPnL += closed * (fill.Price - pos.AvgPrice) * direction
		switch {
		case math.Abs(newQty) < 1e-12:
			pos.AvgPrice = 0
		case !sameSign(newQty, oldQty):
			// Flipped from long to short (or back): the new leg starts here.
			pos.AvgPrice = fill.Price
		}
	}

	if math.Abs(newQty) < 1e-12 {
		pos.Quantity = 0
		pos.OpenedAt = time.Time{}
		pos.StopPrice = math.NaN()
		pos.TakeProfitPrice = math.NaN()
	} else {
		pos.Quantity = newQty
	}
}

// MarkToMarketPnL is the unrealized PnL across all open positions.
func (p *Portfolio) MarkToMarketPnL(prices map[string]float64) float64 {
	total := 0.0
	for symbol, pos := range p.Positions {
		if !pos.IsOpen() {
			continue
		}
		price, ok := prices[symbol]
		if !ok {
			price = pos.AvgPrice
		}
		total += pos.Unrealized(price)
	}
	return total
}

func sameSign(a, b float64) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0)
}
