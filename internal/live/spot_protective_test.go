package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
)

func TestSpotProtectiveUsesNetInventoryAndReusesStop(t *testing.T) {
	var row string
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v3/openOrders":
			if row == "" {
				fmt.Fprint(w, "[]")
			} else {
				fmt.Fprintf(w, "[%s]", row)
			}
		case "POST /api/v3/order":
			writes++
			q := r.URL.Query()
			if q.Get("quantity") != "0.999" || q.Get("stopPrice") != "90" || q.Get("type") != "STOP_LOSS" || q.Get("side") != "SELL" {
				t.Errorf("wrong net-quantity stop request")
			}
			row = fmt.Sprintf(`{"orderId":1,"clientOrderId":%q,"symbol":"BTCUSDT","side":"SELL","type":"STOP_LOSS","status":"NEW","origQty":"0.999","executedQty":"0","stopPrice":"90","orderListId":-1}`, q.Get("newClientOrderId"))
			fmt.Fprint(w, row)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer srv.Close()
	b := broker.NewBinance(broker.BinanceConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, StepSize: 0.001})
	book := portfolio.New(1000)
	book.ApplyFill(broker.Fill{Symbol: "BTCUSDT", Side: broker.Buy, Quantity: 1, Price: 100, Commission: 0.1, BaseCommission: 0.001})
	p := &spotProtective{b: b, book: book, symbol: "BTCUSDT"}
	if has, err := p.Has(); has || err != nil {
		t.Fatalf("initial Has=%v err=%v", has, err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Open(broker.Buy, 90, 110); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("writes=%d want 1", writes)
	}
	if has, err := p.Has(); !has || err != nil {
		t.Fatalf("Has=%v err=%v", has, err)
	}
	if err := p.Open(broker.Sell, 90, 110); err == nil {
		t.Fatal("short protection accepted")
	}
	if writes != 1 {
		t.Fatal("invalid side wrote order")
	}
}

func TestSpotProtectiveCancelChecksReleasedInventory(t *testing.T) {
	for _, tc := range []struct {
		name, free, locked string
		fail               bool
	}{
		{"released", "0.999", "0", false},
		{"still_locked", "0", "0.999", true},
		{"stop_already_filled", "0", "0", true},
		{"partial_fill", "0.5", "0", true},
		{"unrelated_inventory", "1.1", "0", true},
		{"account_error", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canceled := false
			reads := 0
			row := `{"orderId":1,"clientOrderId":"tas-test","symbol":"BTCUSDT","side":"SELL","type":"STOP_LOSS","status":"NEW","origQty":"0.999","executedQty":"0","stopPrice":"90","orderListId":-1}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v3/openOrders":
					if tc.name == "stop_already_filled" || tc.name == "partial_fill" {
						fmt.Fprint(w, "[]")
					} else {
						fmt.Fprintf(w, "[%s]", row)
					}
				case "DELETE /api/v3/order":
					canceled = true
					fmt.Fprint(w, `{"orderId":1,"symbol":"BTCUSDT","status":"CANCELED","executedQty":"0"}`)
				case "GET /api/v3/account":
					reads++
					if tc.name != "stop_already_filled" && tc.name != "partial_fill" && !canceled {
						t.Error("balance checked before cancellation")
					}
					if tc.name == "account_error" {
						http.Error(w, "unavailable", 503)
						return
					}
					fmt.Fprintf(w, `{"balances":[{"asset":"BTC","free":%q,"locked":%q},{"asset":"USDT","free":"900","locked":"0"}]}`, tc.free, tc.locked)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
				}
			}))
			defer srv.Close()
			b := broker.NewBinance(broker.BinanceConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			b.BaseAsset, b.QuoteAsset = "BTC", "USDT"
			book := portfolio.New(1000)
			book.Position("BTCUSDT").Quantity = 0.999
			p := &spotProtective{b: b, book: book, symbol: "BTCUSDT"}
			err := p.Cancel()
			if (err != nil) != tc.fail {
				t.Fatalf("Cancel err=%v want failure=%v", err, tc.fail)
			}
			if reads == 0 {
				t.Fatal("no inventory confirmation")
			}
		})
	}
}
