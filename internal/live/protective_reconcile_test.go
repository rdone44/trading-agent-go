package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// Exercise the runner's restart path, not only the broker's repair helper.
func TestReconcileFuturesRepairsStopWhenOnlyTargetSurvives(t *testing.T) {
	stopPresent := false
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /fapi/v2/positionRisk":
			_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`)
		case "GET /fapi/v1/openAlgoOrders":
			stop := ""
			if stopPresent {
				stop = `,{"algoId":2,"algoStatus":"NEW","clientAlgoId":"tap-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true}`
			}
			_, _ = fmt.Fprintf(w, `[{"algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-target","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}%s]`, stop)
		case "POST /fapi/v1/algoOrder":
			writes++
			q := r.URL.Query()
			if q.Get("type") != "STOP_MARKET" || q.Get("triggerPrice") != "90" {
				t.Error("must repair only the missing stop using the persisted level")
			}
			stopPresent = true
			_, _ = fmt.Fprint(w, `{"algoId":2,"algoStatus":"NEW"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer srv.Close()
	b := broker.NewFutures(broker.FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
	book := portfolio.New(1000, 1)
	pos := book.Position("BTCUSDT")
	pos.Quantity, pos.AvgPrice, pos.StopPrice, pos.TakeProfitPrice = 1, 100, 90, 110
	r := &Runner{broker: b, agent: &engine.Agent{Symbol: "BTCUSDT", Book: book, Protective: &futuresProtective{b: b}}}
	for i := 0; i < 3; i++ {
		if err := r.reconcileFutures(); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("writes=%d want one missing stop only", writes)
	}
}

func TestProtectionRepairCheckpointClearsIntentAfterLedgerSave(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	pendingPath := statePath + ".protective-pending.json"
	stopPresent := false
	var stopClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /fapi/v2/positionRisk":
			_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`)
		case "GET /fapi/v1/openAlgoOrders":
			stop := ""
			if stopPresent {
				stop = fmt.Sprintf(`,{"algoId":2,"algoStatus":"NEW","clientAlgoId":%q,"symbol":"BTCUSDT","orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true}`, stopClientID)
			}
			_, _ = fmt.Fprintf(w, `[{"algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-target","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}%s]`, stop)
		case "POST /fapi/v1/algoOrder":
			stopPresent = true
			stopClientID = r.URL.Query().Get("clientAlgoId")
			_, _ = fmt.Fprint(w, `{"algoId":2,"algoStatus":"NEW"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	b := broker.NewFutures(broker.FuturesConfig{
		Symbol: "BTCUSDT", BaseURL: srv.URL,
		ProtectiveJournalPath: pendingPath,
	})
	book := portfolio.New(1000, 1)
	pos := book.Position("BTCUSDT")
	pos.Quantity, pos.AvgPrice, pos.StopPrice, pos.TakeProfitPrice = 1, 100, 90, 110
	r := &Runner{
		broker: b, executed: true, statePath: statePath, leverage: 1,
		agent: &engine.Agent{Symbol: "BTCUSDT", Strategy: strategy.MovingAverageCross{}, Book: book,
			Risk: risk.New(config.Default().Risk), Protective: &futuresProtective{b: b}},
	}
	if err := r.reconcileFutures(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("placement did not leave a durable intent before checkpoint: %v", err)
	}
	if err := r.checkpointProtectionIntent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("checkpoint did not clear verified intent: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("checkpoint did not save ledger: %v", err)
	}
}
