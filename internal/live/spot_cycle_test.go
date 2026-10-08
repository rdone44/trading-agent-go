package live

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/state"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

func TestSpotCycleInventoryGuard(t *testing.T) {
	for _, tc := range []struct {
		name, free, locked string
		fail, blocked      bool
	}{
		{"locked_intact", "0", "0.999", false, false},
		{"free_intact", "0.999", "0", false, false},
		{"filled", "0", "0", false, true},
		{"partially_filled", "0.5", "0", false, true},
		{"extra_inventory", "1.1", "0", false, true},
		{"small_mismatch", "0.9990005", "0", false, true},
		{"invalid_balance", "NaN", "0", false, true},
		{"account_error", "0", "0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" || req.URL.Path != "/api/v3/account" {
					writes++
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", 400)
					return
				}
				reads++
				if tc.fail {
					http.Error(w, "unavailable", 503)
					return
				}
				fmt.Fprintf(w, `{"balances":[{"asset":"BTC","free":%q,"locked":%q},{"asset":"USDT","free":"900","locked":"0"}]}`, tc.free, tc.locked)
			}))
			defer srv.Close()
			cfg := config.Default()
			cfg.Agent.Symbol = "BTCUSDT"
			strat, err := strategy.New(cfg.Strategy.Name, cfg)
			if err != nil {
				t.Fatal(err)
			}
			r, err := New(cfg, strat, false, filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			b := broker.NewBinance(broker.BinanceConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			b.BaseAsset, b.QuoteAsset = "BTC", "USDT"
			r.broker = b
			r.agent.RestoreState(900, 1000, &engine.OpenTrade{Side: broker.Buy, Quantity: .999, EntryPrice: 100}, 90, 110, r.agent.Risk.Snapshot())
			r.agent.Protective = &spotProtective{b: b, book: r.agent.Book, symbol: "BTCUSDT"}
			priceCalls := 0
			marker := errors.New("offline price sentinel")
			r.PriceLoader = func(string) (float64, time.Time, error) { priceCalls++; return 0, time.Time{}, marker }
			r.SeriesLoader = func(string, int, time.Time) (model.Series, error) {
				t.Fatal("history must not run")
				return model.Series{}, nil
			}
			for i := 0; i < 3; i++ {
				_, err = r.Cycle(time.Now().UTC())
				if tc.blocked {
					if err == nil || !strings.Contains(err.Error(), "对账") {
						t.Fatalf("missing reconciliation error: %v", err)
					}
				} else if !errors.Is(err, marker) {
					t.Fatalf("intact inventory blocked: %v", err)
				}
			}
			if writes != 0 || len(b.Fills()) != 0 || r.agent.Book.Position("BTCUSDT").Quantity != .999 || r.agent.Book.Cash != 900 {
				t.Fatal("guard changed orders or accounting")
			}
			if tc.blocked {
				if reads != 1 || priceCalls != 0 || !r.agent.Risk.OrderUncertain || !r.agent.Risk.Halted {
					t.Fatalf("unsafe retry: reads=%d prices=%d", reads, priceCalls)
				}
				saved, exists, err := state.Load(r.statePath)
				if err != nil || !exists {
					t.Fatalf("saved halt: %v exists=%v", err, exists)
				}
				_, _, _, _, _, riskState := saved.ToEngine()
				if !riskState.OrderUncertain || !riskState.Halted {
					t.Fatal("halt not persisted")
				}
				resumed, err := New(cfg, strat, false, r.statePath)
				if err != nil {
					t.Fatal(err)
				}
				if err = resumed.Init(); err != nil {
					t.Fatal(err)
				}
				resumed.PriceLoader = r.PriceLoader
				if _, err = resumed.Cycle(time.Now().UTC()); err == nil || priceCalls != 0 {
					t.Fatal("restored uncertain cycle continued")
				}
			} else if reads != 3 || priceCalls != 3 {
				t.Fatalf("intact inventory calls=%d/%d", reads, priceCalls)
			}
		})
	}
}
