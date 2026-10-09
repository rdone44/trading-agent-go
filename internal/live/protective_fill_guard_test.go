package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/state"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// A position delta is evidence to halt, not a fill price or a commission.
// Exercise both directions, repeated polls and a restored ledger without
// allowing either market data or any exchange mutation past the guard.
func TestProtectiveFillGuardPreservesLedgerAcrossRestart(t *testing.T) {
	cases := []struct {
		name, amount, entry string
		status              int
	}{
		{"closed", "0", "0", http.StatusOK},
		{"partial", "0.5", "100", http.StatusOK},
		{"extra_inventory", "1.5", "100", http.StatusOK},
		{"reversed", "-1", "100", http.StatusOK},
		{"different_entry", "1", "101", http.StatusOK},
		{"invalid_quantity", "NaN", "100", http.StatusOK},
		{"query_failure", "", "", http.StatusServiceUnavailable},
	}
	for _, side := range []broker.Side{broker.Buy, broker.Sell} {
		for _, tc := range cases {
			t.Run(string(side)+"/"+tc.name, func(t *testing.T) {
				queries, writes, marketCalls := 0, 0, 0
				amount := tc.amount
				if side == broker.Sell && amount != "0" && amount != "NaN" && amount != "" {
					if amount[0] == '-' {
						amount = amount[1:]
					} else {
						amount = "-" + amount
					}
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodGet {
						writes++
					}
					if req.Method != http.MethodGet || req.URL.Path != "/fapi/v2/positionRisk" {
						t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					queries++
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprintf(w, `[{"symbol":"BTCUSDT","positionAmt":%q,"entryPrice":%q,"positionSide":"BOTH"}]`, amount, tc.entry)
				}))
				defer srv.Close()

				cfg := config.Default()
				b := broker.NewFutures(broker.FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
				a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b,
					portfolio.New(1000, 1), risk.New(cfg.Risk))
				a.RestoreState(1000, 1000, &engine.OpenTrade{
					Quantity: 1, EntryPrice: 100, EntryFee: 0.04, Side: side,
					EntryTime: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
				}, 90, 110, risk.RiskState{})
				a.Protective = &futuresProtective{b: b}
				path := filepath.Join(t.TempDir(), "ledger.json")
				r := &Runner{cfg: cfg, agent: a, broker: b, executed: true, leverage: 1, statePath: path}
				r.PriceLoader = func(string) (float64, time.Time, error) {
					marketCalls++
					return 100, time.Now(), nil
				}
				r.SeriesLoader = func(string, int, time.Time) (model.Series, error) {
					marketCalls++
					return model.Series{}, nil
				}
				original := *a.OpenTrade()
				position := *a.Book.Position("BTCUSDT")
				for poll := 0; poll < 3; poll++ {
					if _, err := r.Cycle(time.Now()); err == nil {
						t.Fatal("unsafe position was accepted")
					}
				}
				if queries != 1 || writes != 0 || marketCalls != 0 || len(b.Fills()) != 0 {
					t.Fatalf("guard leaked work: queries=%d writes=%d market=%d fills=%d", queries, writes, marketCalls, len(b.Fills()))
				}
				if !a.Risk.OrderUncertain || !a.Risk.Halted || a.Book.Cash != 1000 ||
					!reflect.DeepEqual(original, *a.OpenTrade()) || !reflect.DeepEqual(position, *a.Book.Position("BTCUSDT")) {
					t.Fatal("guard changed accounting or failed to halt")
				}
				saved, ok, err := state.Load(path)
				if err != nil || !ok || !saved.Risk.OrderUncertain || !saved.Risk.Halted {
					t.Fatalf("halt not persisted: ok=%v err=%v state=%+v", ok, err, saved)
				}
				cash, peak, open, stop, target, riskState := saved.ToEngine()
				if cash != 1000 || !reflect.DeepEqual(original, *open) {
					t.Fatal("saved ledger invented a fill or commission")
				}
				resumed := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b,
					portfolio.New(1000, 1), risk.New(cfg.Risk))
				resumed.RestoreState(cash, peak, open, stop, target, riskState)
				r.agent = resumed
				if _, err := r.Cycle(time.Now()); err == nil {
					t.Fatal("restored runner accepted the unreconciled halt")
				}
				if queries != 1 || writes != 0 || marketCalls != 0 || len(b.Fills()) != 0 {
					t.Fatal("restart retried exchange or market work")
				}
			})
		}
	}
}
