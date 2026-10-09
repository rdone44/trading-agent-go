package live

import (
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
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// Regression: an exchange-side protective leg can fill between polls without
// any local fill. The cycle used to keep trading the stale book (it queried
// the exchange position zero times), so the next strategy call sized against
// a position that no longer existed. Every executing futures cycle now
// verifies the venue's position first and halts for reconciliation on a
// mismatch — it never invents a fill price/fee from the position delta.
func TestFuturesCycleDetectsExchangeSideProtectiveFill(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "1")
	queries := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v2/positionRisk" {
			queries++
			// The exchange is flat: the protective stop filled and closed
			// the position while the local book still shows it open.
			fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"0","entryPrice":"0","positionSide":"BOTH"}]`)
			return
		}
		t.Errorf("unexpected exchange request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mock.Close()

	cfg := config.Default()
	cfg.Live.Futures = true
	b := broker.NewFutures(broker.FuturesConfig{BaseURL: mock.URL, Symbol: "BTCUSDT"})
	a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b,
		portfolio.NewFutures(1000, 1), risk.New(cfg.Risk))
	a.RestoreState(1000, 1000, &engine.OpenTrade{Quantity: 1, EntryPrice: 100, Side: broker.Buy},
		90, 110, risk.RiskState{})
	a.Protective = &futuresProtective{b: b}

	runner := &Runner{
		cfg: cfg, agent: a, broker: b, futures: true, executed: true, leverage: 1,
		statePath: filepath.Join(t.TempDir(), "ledger.json"),
	}
	runner.PriceLoader = func(string) (float64, time.Time, error) { return 95, time.Now(), nil }
	runner.SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		series := model.Series{Symbol: symbol}
		for i := 0; i < 120; i++ {
			series.Bars = append(series.Bars, model.Bar{Close: float64(100 + i)})
		}
		return series, nil
	}

	if _, err := runner.Cycle(time.Now()); err == nil {
		t.Fatal("cycle traded on a book the exchange no longer matches")
	} else if !strings.Contains(err.Error(), "保护单成交") {
		t.Fatalf("cycle error = %v, want the protective-fill reconciliation message", err)
	}
	if queries == 0 {
		t.Fatal("cycle never queried the exchange position")
	}
	if !a.Risk.OrderUncertain {
		t.Fatal("the mismatch must persist a reconciliation halt")
	}
	// The halt survives a restart: the state file now carries it.
	resumed, err := New(cfg, strategy.MovingAverageCross{}, true, runner.statePath)
	if err != nil {
		t.Fatal(err)
	}
	resumed.SeriesLoader = runner.SeriesLoader
	if err := resumed.Init(); err == nil {
		t.Fatal("a restarted runner accepted the unreconciled halt")
	}
}

// A cycle whose exchange position agrees with the book must not halt: the
// check is a guard, not a blanket refusal to trade.
func TestFuturesCyclePassesWhenPositionsAgree(t *testing.T) {
	queries := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v2/positionRisk" {
			queries++
			fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`)
			return
		}
		t.Errorf("unexpected exchange request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mock.Close()

	cfg := config.Default()
	cfg.Live.Futures = true
	b := broker.NewFutures(broker.FuturesConfig{BaseURL: mock.URL, Symbol: "BTCUSDT"})
	a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b,
		portfolio.NewFutures(1000, 1), risk.New(cfg.Risk))
	a.RestoreState(1000, 1000, &engine.OpenTrade{Quantity: 1, EntryPrice: 100, Side: broker.Buy},
		90, 110, risk.RiskState{})

	runner := &Runner{
		cfg: cfg, agent: a, broker: b, futures: true, executed: true, leverage: 1,
		statePath: filepath.Join(t.TempDir(), "ledger.json"),
	}
	runner.PriceLoader = func(string) (float64, time.Time, error) { return 105, time.Now(), nil }
	runner.SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		series := model.Series{Symbol: symbol}
		for i := 0; i < 120; i++ {
			series.Bars = append(series.Bars, model.Bar{Close: float64(100 + i)})
		}
		return series, nil
	}

	if _, err := runner.Cycle(time.Now()); err != nil {
		t.Fatalf("cycle halted on matching positions: %v", err)
	}
	if queries != 1 {
		t.Fatalf("position queries = %d, want exactly 1 per cycle", queries)
	}
}

// A state file written by another account must never be resumed: the
// accounts-mode runner records its owner and refuses a foreign ledger.
func TestLedgerOwnerIsEnforced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	cfg := config.Default()

	alice, err := NewForOwner(cfg, strategy.MovingAverageCross{}, false, path, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Save(); err != nil {
		t.Fatal(err)
	}

	bob, err := NewForOwner(cfg, strategy.MovingAverageCross{}, false, path, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.Init(); err == nil {
		t.Fatal("bob resumed alice's ledger")
	}

	// Alice can still resume her own file.
	aliceAgain, err := NewForOwner(cfg, strategy.MovingAverageCross{}, false, path, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := aliceAgain.Init(); err != nil {
		t.Fatalf("alice could not resume her own ledger: %v", err)
	}
}
