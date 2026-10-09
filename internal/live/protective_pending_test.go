package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

func recoveredPendingRunner(t *testing.T, statePath, baseURL string) (*Runner, *broker.FuturesBroker) {
	t.Helper()
	cfg := config.Default()
	cfg.Agent.Symbol = "BTCUSDT"
	b := broker.NewFutures(broker.FuturesConfig{
		Symbol: "BTCUSDT", BaseURL: baseURL,
		ProtectiveJournalPath: statePath + ".protective-pending.json",
	})
	a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b,
		portfolio.New(1000, 1), risk.New(cfg.Risk))
	a.RestoreState(1000, 1000,
		&engine.OpenTrade{Quantity: 1, EntryPrice: 100, Side: broker.Buy},
		90, 110, risk.RiskState{})
	a.Protective = &futuresProtective{b: b}
	return &Runner{
		cfg: cfg, agent: a, broker: b, executed: true, leverage: 1,
		statePath: statePath, protectiveRecoveryRequired: true,
	}, b
}

func TestCycleWithRecoveredProtectiveIntentStopsBeforeMarketOrOrders(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	pendingPath := statePath + ".protective-pending.json"
	pending := `{
  "symbol":"BTCUSDT","side":"buy","created_at":"2026-10-09T00:00:00Z",
  "legs":[
    {"order_type":"STOP_MARKET","client_algo_id":"tap-original-stop","trigger_price":90},
    {"order_type":"TAKE_PROFIT_MARKET","client_algo_id":"tap-original-target","trigger_price":110}
  ]
}`
	if err := os.WriteFile(pendingPath, []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}

	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /fapi/v2/positionRisk":
			_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`)
		case "GET /fapi/v1/openAlgoOrders":
			_, _ = fmt.Fprint(w, `[
{"algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-original-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true},
{"algoId":2,"algoStatus":"NEW","clientAlgoId":"tap-original-target","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}
]`)
		case "GET /fapi/v2/balance":
			_, _ = fmt.Fprint(w, `[{"asset":"USDT","balance":"975.25"}]`)
		default:
			writes++
			http.Error(w, "must not write", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	runner, b := recoveredPendingRunner(t, statePath, srv.URL)
	a := runner.agent
	priceCalls := 0
	runner.PriceLoader = func(string) (float64, time.Time, error) {
		priceCalls++
		return 100, time.Now(), nil
	}

	if _, err := runner.Cycle(time.Now()); err == nil {
		t.Fatal("recovered pending intent did not require manual recovery")
	}
	if priceCalls != 0 || writes != 0 || len(b.Fills()) != 0 {
		t.Fatalf("unsafe work during recovery: prices=%d writes=%d fills=%d", priceCalls, writes, len(b.Fills()))
	}
	if !a.Risk.OrderUncertain || !a.Book.Position("BTCUSDT").IsOpen() {
		t.Fatal("recovery must preserve the position and persist a reconciliation halt")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending intent was cleared before manual recovery: %v", err)
	}
}

func TestRecoverProtectiveIntentRequiresPhraseAndNeverWritesExchange(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	pendingPath := statePath + ".protective-pending.json"
	pending := `{
  "symbol":"BTCUSDT","side":"buy","created_at":"2026-10-09T00:00:00Z",
  "legs":[
    {"order_type":"STOP_MARKET","client_algo_id":"tap-original-stop","trigger_price":90},
    {"order_type":"TAKE_PROFIT_MARKET","client_algo_id":"tap-original-target","trigger_price":110}
  ]
}`
	if err := os.WriteFile(pendingPath, []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}

	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /fapi/v2/positionRisk":
			_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`)
		case "GET /fapi/v1/openAlgoOrders":
			_, _ = fmt.Fprint(w, `[
{"algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-original-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true},
{"algoId":2,"algoStatus":"NEW","clientAlgoId":"tap-original-target","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}
]`)
		case "GET /fapi/v2/balance":
			_, _ = fmt.Fprint(w, `[{"asset":"USDT","balance":"975.25"}]`)
		default:
			writes++
			http.Error(w, "must not write", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	runner, b := recoveredPendingRunner(t, statePath, srv.URL)
	runner.agent.Risk.RequireReconciliation("保护单意图已核验，等待人工恢复")
	if err := runner.RecoverProtectiveIntent("yes"); err == nil {
		t.Fatal("wrong recovery phrase was accepted")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("wrong phrase cleared pending evidence: %v", err)
	}
	if err := runner.RecoverProtectiveIntent(ProtectionRecoveryPhrase); err != nil {
		t.Fatalf("confirmed recovery: %v", err)
	}
	if writes != 0 || len(b.Fills()) != 0 {
		t.Fatalf("recovery wrote to exchange: writes=%d fills=%d", writes, len(b.Fills()))
	}
	if runner.ProtectionRecoveryRequired() || runner.agent.Risk.OrderUncertain {
		t.Fatal("successful recovery did not clear the local recovery lock")
	}
	if runner.agent.Book.Cash != 975.25 {
		t.Fatalf("wallet snapshot = %v, want 975.25", runner.agent.Book.Cash)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("confirmed recovery kept pending evidence: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("recovered ledger was not saved: %v", err)
	}
}
