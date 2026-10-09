package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpotStopCancelPreflightRejectsUnsafeBatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spotProtectiveOrder)
	}{
		{"missing-quantity", func(o *spotProtectiveOrder) { o.Quantity = "" }},
		{"zero-quantity", func(o *spotProtectiveOrder) { o.Quantity = "0" }},
		{"negative-quantity", func(o *spotProtectiveOrder) { o.Quantity = "-1" }},
		{"nan-quantity", func(o *spotProtectiveOrder) { o.Quantity = "NaN" }},
		{"infinite-quantity", func(o *spotProtectiveOrder) { o.Quantity = "+Inf" }},
		{"missing-stop", func(o *spotProtectiveOrder) { o.Stop = "" }},
		{"zero-stop", func(o *spotProtectiveOrder) { o.Stop = "0" }},
		{"negative-stop", func(o *spotProtectiveOrder) { o.Stop = "-1" }},
		{"nan-stop", func(o *spotProtectiveOrder) { o.Stop = "NaN" }},
		{"infinite-stop", func(o *spotProtectiveOrder) { o.Stop = "+Inf" }},
		{"terminal-status", func(o *spotProtectiveOrder) { o.Status = "CANCELED" }},
		{"missing-status", func(o *spotProtectiveOrder) { o.Status = "" }},
		{"partial-status", func(o *spotProtectiveOrder) { o.Status = "PARTIALLY_FILLED" }},
		{"duplicate-order-id", func(o *spotProtectiveOrder) { o.OrderID = 42 }},
		{"duplicate-client-id", func(o *spotProtectiveOrder) { o.ClientID = "tas-first" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := spotStop("tas-first")
			second := spotStop("tas-second")
			second.OrderID = 43
			tc.mutate(&second)
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v3/openOrders":
					json.NewEncoder(w).Encode([]spotProtectiveOrder{first, second})
				case "DELETE /api/v3/order":
					writes++
					row := first
					if r.URL.Query().Get("orderId") == "43" {
						row = second
					}
					row.Status = "CANCELED"
					json.NewEncoder(w).Encode(row)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT"})
			if err := b.CancelSpotStops(); err == nil || writes != 0 {
				t.Fatalf("unsafe batch must fail before any cancellation: err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestSpotStopCancelPreflightAllowsDistinctValidStops(t *testing.T) {
	first, second := spotStop("tas-first"), spotStop("tas-second")
	second.OrderID = 43
	rows := []spotProtectiveOrder{first, second}
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v3/openOrders" {
			json.NewEncoder(w).Encode(rows)
			return
		}
		if r.Method != "DELETE" || r.URL.Path != "/api/v3/order" || writes >= len(rows) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		row := rows[writes]
		wantID := "42"
		if writes == 1 {
			wantID = "43"
		}
		if r.URL.Query().Get("orderId") != wantID {
			t.Error("wrong cancellation identity")
		}
		writes++
		row.Status = "CANCELED"
		json.NewEncoder(w).Encode(row)
	}))
	defer srv.Close()
	b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT"})
	if err := b.CancelSpotStops(); err != nil || writes != 2 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
}
