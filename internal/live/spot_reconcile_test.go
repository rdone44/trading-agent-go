package live

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
)

func TestReconcileSpotProtection(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		local, free, locked                          float64
		stop                                         bool
		conflict, uncertain, accountFail, ordersFail bool
		wantErr                                      bool
		wantWrites, wantCancels                      int
	}{
		{name: "missing_stop", local: .999, free: .999, wantWrites: 1},
		{name: "surviving_locked_stop", local: .999, locked: .999, stop: true},
		{name: "orphan_stop", stop: true, wantCancels: 1},
		{name: "flat"},
		{name: "conflicting_stop", local: .999, locked: .999, stop: true, conflict: true, wantErr: true},
		{name: "already_filled", local: .999, wantErr: true},
		{name: "partial_fill", local: .999, free: .5, wantErr: true},
		{name: "extra_inventory", local: .999, free: 1.1, wantErr: true},
		{name: "small_mismatch", local: .999, free: .9990005, wantErr: true},
		{name: "uncertain", local: .999, free: .999, uncertain: true, wantErr: true},
		{name: "account_error", local: .999, free: .999, accountFail: true, wantErr: true},
		{name: "orders_error", local: .999, free: .999, ordersFail: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := ""
			makeRow := func(id string) string {
				stop := 90
				if tc.conflict {
					stop = 91
				}
				return fmt.Sprintf(`{"orderId":1,"clientOrderId":%q,"symbol":"BTCUSDT","side":"SELL","type":"STOP_LOSS","status":"NEW","origQty":"0.999","executedQty":"0","stopPrice":"%d","orderListId":-1}`, id, stop)
			}
			if tc.stop {
				row = makeRow("tas-existing")
			}
			writes, cancels := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.Method + " " + req.URL.Path {
				case "GET /api/v3/account":
					if tc.accountFail {
						http.Error(w, "unavailable", 503)
						return
					}
					fmt.Fprintf(w, `{"balances":[{"asset":"BTC","free":%q,"locked":%q},{"asset":"USDT","free":"900","locked":"0"}]}`, fmt.Sprint(tc.free), fmt.Sprint(tc.locked))
				case "GET /api/v3/openOrders":
					if tc.ordersFail {
						http.Error(w, "unavailable", 503)
						return
					}
					fmt.Fprintf(w, "[%s]", row)
				case "POST /api/v3/order":
					writes++
					q := req.URL.Query()
					if q.Get("quantity") != "0.999" || q.Get("stopPrice") != "90" || q.Get("type") != "STOP_LOSS" || q.Get("side") != "SELL" {
						t.Error("repair must use the restored net quantity and risk stop")
					}
					row = makeRow(q.Get("newClientOrderId"))
					fmt.Fprint(w, row)
				case "DELETE /api/v3/order":
					cancels++
					if req.URL.Query().Get("orderId") != "1" {
						t.Error("wrong orphan identity")
					}
					row = ""
					fmt.Fprint(w, `{"orderId":1,"clientOrderId":"tas-existing","symbol":"BTCUSDT","side":"SELL","type":"STOP_LOSS","status":"CANCELED","origQty":"0.999","executedQty":"0","stopPrice":"90","orderListId":-1}`)
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", 400)
				}
			}))
			defer srv.Close()
			b := broker.NewBinance(broker.BinanceConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, StepSize: .001})
			b.BaseAsset, b.QuoteAsset = "BTC", "USDT"
			book := portfolio.New(1000)
			pos := book.Position("BTCUSDT")
			pos.Quantity, pos.AvgPrice, pos.StopPrice, pos.TakeProfitPrice = tc.local, 100, 90, 110
			rm := &risk.Manager{OrderUncertain: tc.uncertain}
			r := &Runner{broker: b, agent: &engine.Agent{Symbol: "BTCUSDT", Book: book, Risk: rm, Protective: &spotProtective{b: b, book: book, symbol: "BTCUSDT"}}}
			for i := 0; i < 3; i++ {
				err := r.reconcile()
				if (err != nil) != tc.wantErr {
					t.Fatalf("reconcile=%v wantErr=%v", err, tc.wantErr)
				}
			}
			if writes != tc.wantWrites || cancels != tc.wantCancels {
				t.Fatalf("writes/cancels=%d/%d want %d/%d", writes, cancels, tc.wantWrites, tc.wantCancels)
			}
			if pos.Quantity != tc.local || book.Cash != 1000 {
				t.Fatal("reconciliation changed accounting")
			}
		})
	}
}
