package engine

import (
	"math"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
)

func TestGapStopAccounting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		futures bool
		side    broker.Side
		stop    float64
		open    float64
	}{
		{"spot_long", false, broker.Buy, 90, 80},
		{"futures_long", true, broker.Buy, 90, 80},
		{"futures_short", true, broker.Sell, 110, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Risk.Leverage = 1
			cfg.Execution.LotSize = 1
			cfg.Execution.MinTradeNotional = 0
			cfg.Execution.CommissionBps = 10
			cfg.Execution.SlippageBps = 25
			book := portfolio.New(1000)
			if tc.futures {
				book = portfolio.NewFutures(1000, 1)
			}
			a := NewWithBroker(cfg, nil, broker.New(cfg.Execution), book, risk.New(cfg.Risk))
			ts := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
			// Seed a completed entry without calling any exchange. Its fee is
			// charged once in the wallet and allocated once in the round trip.
			entry := broker.Fill{Symbol: a.Symbol, Side: tc.side, Quantity: 2, Price: 100, Commission: 0.2}
			book.ApplyFill(entry)
			book.Position(a.Symbol).StopPrice = tc.stop
			a.open = &OpenTrade{EntryTime: ts, EntryPrice: 100, Quantity: 2, Side: tc.side, EntryFee: 0.2}
			// A gap has crossed the stop: use the worse open, never the stale
			// stop price. Both long and short paths include adverse slippage.
			a.checkExits(model.Bar{Time: ts.Add(time.Hour), Open: tc.open, Low: tc.open - 1, High: tc.open + 1, Close: tc.open})
			fills := a.Broker.Fills()
			if len(fills) != 1 || fills[0].Rejected || len(a.trades) != 1 || book.Position(a.Symbol).IsOpen() || a.OpenTrade() != nil {
				t.Fatalf("gap stop did not flatten exactly once: fills=%+v trades=%+v", fills, a.trades)
			}
			direction, exitSide := 1.0, broker.Sell
			if tc.side == broker.Sell {
				direction, exitSide = -1, broker.Buy
			}
			wantPrice := tc.open * (1 - direction*0.0025)
			wantGross := (wantPrice - 100) * 2 * direction
			wantFees := 0.2 + wantPrice*2*0.001
			wantPnL := wantGross - wantFees
			trade := a.trades[0]
			for name, pair := range map[string][2]float64{
				"fill price":   {fills[0].Price, wantPrice},
				"gross PnL":    {trade.GrossPnL, wantGross},
				"fees":         {trade.Commission, wantFees},
				"net PnL":      {trade.PnL, wantPnL},
				"wallet":       {book.Cash, 1000 + wantPnL},
				"realized PnL": {book.RealizedPnL, wantGross},
				"flat equity":  {book.Equity(map[string]float64{a.Symbol: tc.open}), book.Cash},
			} {
				if math.Abs(pair[0]-pair[1]) > 1e-9 {
					t.Errorf("%s = %.12f, want %.12f", name, pair[0], pair[1])
				}
			}
			if fills[0].Side != exitSide || trade.Reason != "stop_loss" || trade.Quantity != 2 || trade.ExitPrice != fills[0].Price || book.MarginUsed() != 0 {
				t.Fatalf("incorrect exit metadata or unreleased margin: fill=%+v trade=%+v margin=%v", fills[0], trade, book.MarginUsed())
			}
			a.checkExits(model.Bar{Time: ts.Add(2 * time.Hour), Open: tc.open, Low: tc.open - 1, High: tc.open + 1})
			if len(a.Broker.Fills()) != 1 || len(a.trades) != 1 {
				t.Fatal("flat book produced a duplicate exit")
			}
		})
	}
}
