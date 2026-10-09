package broker

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFuturesUsesSymbolFiltersAndReduceOnlyExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			// Init syncs the clock before its first signed call.
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		case "/fapi/v1/leverage":
			fmt.Fprint(w, `{}`)
		case "/fapi/v1/marginType":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":-4046,"msg":"No need to change margin type."}`)
		case "/fapi/v1/exchangeInfo":
			fmt.Fprint(w, `{"symbols":[{"symbol":"OTHER","quantityPrecision":0,"filters":[]},{"symbol":"BTCUSDT","quantityPrecision":3,"filters":[{"filterType":"LOT_SIZE","stepSize":"0.001"}]}]}`)
		case "/fapi/v1/order":
			if r.URL.Query().Get("reduceOnly") != "true" || r.URL.Query().Get("newOrderRespType") != "RESULT" {
				t.Errorf("unsafe close request %s", r.URL)
			}
			fmt.Fprint(w, `{"orderId":12,"executedQty":"0.1","avgPrice":"80000","status":"FILLED"}`)
		case "/fapi/v1/userTrades":
			fmt.Fprint(w, `[{"commission":"3.2","commissionAsset":"USDT"}]`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	b := NewFutures(FuturesConfig{BaseURL: server.URL, Symbol: "BTCUSDT"})
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	if math.Abs(b.StepSz-.001) > 1e-9 || b.QuantityPrecision != 3 {
		t.Fatal("loaded another symbol's filters")
	}
	f := b.MarketOrder(time.Now(), "BTCUSDT", Sell, .1, 80000, "stop_loss")
	if f.Rejected || f.Uncertain || f.Commission != 3.2 {
		t.Fatalf("bad close fill %+v", f)
	}
}

func TestClientIDFitsExchangeLimitAndSurvivesRestart(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		b := NewFutures(FuturesConfig{})
		id := b.nextOrderID("BTCUSDT", Buy)
		if len(id) > 36 || seen[id] || !strings.Contains(id, "BTCUSDT") {
			t.Fatalf("invalid or duplicate %s", id)
		}
		seen[id] = true
	}
}
