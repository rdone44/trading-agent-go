package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
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
				stop = `,{"algoId":2,"clientAlgoId":"tap-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true}`
			}
			_, _ = fmt.Fprintf(w, `[{"algoId":1,"clientAlgoId":"tap-target","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}%s]`, stop)
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
	book := portfolio.NewFutures(1000, 1)
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
