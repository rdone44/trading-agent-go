package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpotStopCancelRequiresExactConfirmation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spotProtectiveOrder)
	}{
		{"valid", func(*spotProtectiveOrder) {}},
		{"wrong-order", func(o *spotProtectiveOrder) { o.OrderID++ }},
		{"wrong-client", func(o *spotProtectiveOrder) { o.ClientID = "tas-other" }},
		{"wrong-symbol", func(o *spotProtectiveOrder) { o.Symbol = "ETHUSDT" }},
		{"wrong-side", func(o *spotProtectiveOrder) { o.Side = "BUY" }},
		{"wrong-type", func(o *spotProtectiveOrder) { o.Type = "LIMIT" }},
		{"wrong-list", func(o *spotProtectiveOrder) { o.ListID = 1 }},
		{"missing-list", func(o *spotProtectiveOrder) { o.ListID = 0 }},
		{"wrong-quantity", func(o *spotProtectiveOrder) { o.Quantity = "1" }},
		{"missing-quantity", func(o *spotProtectiveOrder) { o.Quantity = "" }},
		{"wrong-stop", func(o *spotProtectiveOrder) { o.Stop = "91" }},
		{"nan-stop", func(o *spotProtectiveOrder) { o.Stop = "NaN" }},
		{"partial-fill", func(o *spotProtectiveOrder) { o.Executed = "0.01" }},
		{"missing-executed", func(o *spotProtectiveOrder) { o.Executed = "" }},
		{"still-new", func(o *spotProtectiveOrder) { o.Status = "NEW" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := spotStop("tas-owned")
			canceled := original
			canceled.Status = "CANCELED"
			tc.mutate(&canceled)
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v3/openOrders":
					json.NewEncoder(w).Encode([]spotProtectiveOrder{original})
				case "DELETE /api/v3/order":
					writes++
					if r.URL.Query().Get("orderId") != "42" || r.URL.Query().Get("symbol") != "BTCUSDT" {
						t.Error("wrong cancellation identity")
					}
					json.NewEncoder(w).Encode(canceled)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT"})
			err := b.CancelSpotStops()
			if (err == nil) != (tc.name == "valid") || writes != 1 {
				t.Fatalf("err=%v writes=%d", err, writes)
			}
		})
	}
}
