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

func TestSpotActualFullResponseUsesExecutedQuoteAndFees(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Query().Get("newOrderRespType") != "FULL" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"orderId":123,"executedQty":"0.1","cummulativeQuoteQty":"8000","status":"FILLED","fills":[{"price":"80000","qty":"0.1","commission":"0.0001","commissionAsset":"BTC"}]}`)
	}))
	defer server.Close()
	b := NewBinance(BinanceConfig{BaseURL: server.URL, APIKey: "test", SecretKey: "test", StepSize: .001, Symbol: "BTCUSDT"})
	b.BaseAsset, b.QuoteAsset = "BTC", "USDT"
	f := b.MarketOrder(time.Now(), "BTCUSDT", Buy, .1, 80000, "entry_long")
	if f.Rejected || f.Uncertain || f.Price != 80000 || f.Quantity != .1 || f.BaseCommission != .0001 || f.Commission != 8 {
		t.Fatalf("filled order incorrectly parsed: %+v", f)
	}
}

func TestSpotTimeoutQueriesSameClientIDInsteadOfPostingAgain(t *testing.T) {
	posts := 0
	var clientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/myTrades" {
			fmt.Fprint(w, `[{"price":"80000","qty":"0.1","commission":"0","commissionAsset":"USDT"}]`)
			return
		}
		if r.Method == http.MethodPost {
			posts++
			clientID = r.URL.Query().Get("newClientOrderId")
			w.WriteHeader(504)
			fmt.Fprint(w, `{"code":-1007,"msg":"Timeout"}`)
			return
		}
		if r.URL.Query().Get("origClientOrderId") != clientID {
			t.Error("lookup changed the order identity")
		}
		fmt.Fprint(w, `{"orderId":123,"executedQty":"0.1","cummulativeQuoteQty":"8000","status":"FILLED"}`)
	}))
	defer server.Close()
	b := NewBinance(BinanceConfig{BaseURL: server.URL, StepSize: .001})
	f := b.MarketOrder(time.Now(), "BTCUSDT", Buy, .1, 80000, "entry_long")
	if posts != 1 || f.Rejected || f.Uncertain || f.Price != 80000 || f.ClientOrderID != clientID {
		t.Fatalf("unsafe timeout recovery: posts=%d fill=%+v", posts, f)
	}
}

func TestUnknownSpotOrderRequiresReconciliation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	b := NewBinance(BinanceConfig{BaseURL: server.URL, StepSize: .001})
	f := b.MarketOrder(time.Now(), "BTCUSDT", Buy, .1, 80000, "entry_long")
	if !f.Uncertain || f.ClientOrderID == "" || f.Status != "unknown" {
		t.Fatalf("network ambiguity was treated as an ordinary rejection: %+v", f)
	}
}

func TestAdverseExecutedFillIsNotDiscarded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"orderId":123,"executedQty":"1","cummulativeQuoteQty":"110","status":"FILLED"}`)
	}))
	defer server.Close()
	b := NewBinance(BinanceConfig{BaseURL: server.URL, StepSize: .001, MaxPriceDev: .01})
	f := b.MarketOrder(time.Now(), "BTCUSDT", Buy, 1, 100, "entry_long")
	if f.Rejected || !f.Uncertain || f.Quantity != 1 || f.Price != 110 {
		t.Fatalf("a real fill was lost: %+v", f)
	}
}

func TestFuturesUsesSymbolFiltersAndReduceOnlyExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
		b := NewBinance(BinanceConfig{})
		id := b.nextOrderID("BTCUSDT", Buy)
		if len(id) > 36 || seen[id] {
			t.Fatalf("invalid or duplicate %s", id)
		}
		seen[id] = true
	}
	if id := NewFutures(FuturesConfig{}).nextOrderID("BTCUSDT", Buy); len(id) > 36 || !strings.Contains(id, "BTCUSDT") {
		t.Fatalf("invalid futures id %s", id)
	}
}
