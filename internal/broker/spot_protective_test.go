package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func spotStop(id string) spotProtectiveOrder {
	return spotProtectiveOrder{OrderID: 42, ClientID: id, Symbol: "BTCUSDT", Side: "SELL", Type: "STOP_LOSS", Status: "NEW", Quantity: "0.12345", Executed: "0", Stop: "90", ListID: -1}
}

func TestSpotStopPlacementAndLostResponse(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "lost-response"}[lost], func(t *testing.T) {
			rows := []spotProtectiveOrder{}
			writes, queries := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("signature") == "" || r.Header.Get("X-MBX-APIKEY") != "offline" {
					t.Error("missing authentication")
				}
				switch {
				case r.Method == "GET" && r.URL.Path == "/api/v3/openOrders":
					if q.Get("symbol") != "BTCUSDT" {
						t.Error("missing symbol")
					}
					json.NewEncoder(w).Encode(rows)
				case r.Method == "POST" && r.URL.Path == "/api/v3/order":
					writes++
					if q.Get("side") != "SELL" || q.Get("type") != "STOP_LOSS" || q.Get("quantity") != "0.12345" || q.Get("stopPrice") != "90" || q.Get("newOrderRespType") != "RESULT" || q.Get("closePosition") != "" || q.Get("price") != "" {
						t.Errorf("wrong contract: %v", q)
					}
					rows = append(rows, spotStop(q.Get("newClientOrderId")))
					if lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					json.NewEncoder(w).Encode(rows[0])
				case r.Method == "GET" && r.URL.Path == "/api/v3/order":
					queries++
					if q.Get("origClientOrderId") != rows[0].ClientID {
						t.Error("wrong recovery ID")
					}
					json.NewEncoder(w).Encode(rows[0])
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, APIKey: "offline", SecretKey: "offline", Symbol: "BTCUSDT", StepSize: 0.00001})
			for i := 0; i < 3; i++ {
				if err := b.PlaceSpotStop(0.123456, 90); err != nil {
					t.Fatal(err)
				}
			}
			wantQueries := 0
			if lost {
				wantQueries = 1
			}
			if writes != 1 || queries != wantQueries {
				t.Fatalf("writes=%d queries=%d", writes, queries)
			}
		})
	}
}

func TestSpotStopConflictsPreventWrites(t *testing.T) {
	for _, kind := range []string{"duplicate", "side", "quantity", "stop", "partial", "list", "id", "status", "null", "bad-json", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			row := spotStop("tas-existing")
			rows := []spotProtectiveOrder{row}
			switch kind {
			case "duplicate":
				rows = append(rows, row)
			case "side":
				rows[0].Side = "BUY"
			case "quantity":
				rows[0].Quantity = "2"
			case "stop":
				rows[0].Stop = "NaN"
			case "partial":
				rows[0].Executed = "0.01"
			case "list":
				rows[0].ListID = 1
			case "id":
				rows[0].OrderID = 0
			case "status":
				rows[0].Status = "PARTIALLY_FILLED"
			}
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					w.WriteHeader(400)
					return
				}
				switch kind {
				case "null":
					w.Write([]byte("null"))
				case "bad-json":
					w.Write([]byte("{"))
				case "unavailable":
					w.WriteHeader(503)
				default:
					json.NewEncoder(w).Encode(rows)
				}
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT", StepSize: 0.00001})
			if err := b.PlaceSpotStop(0.123456, 90); err == nil || writes != 0 {
				t.Fatalf("err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestSpotStopCancelOwnershipAndFillRace(t *testing.T) {
	for _, raced := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "fill-race"}[raced], func(t *testing.T) {
			own := spotStop("tas-owned")
			manual := spotStop("manual")
			other := spotStop("tas-other")
			other.Symbol = "ETHUSDT"
			limit := spotStop("tas-limit")
			limit.Type = "LIMIT"
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/api/v3/openOrders" {
					json.NewEncoder(w).Encode([]spotProtectiveOrder{manual, other, limit, own})
					return
				}
				if r.Method != "DELETE" || r.URL.Path != "/api/v3/order" || r.URL.Query().Get("orderId") != "42" || r.URL.Query().Get("symbol") != "BTCUSDT" {
					t.Error("unsafe cancellation")
					w.WriteHeader(400)
					return
				}
				deletes++
				own.Status = "CANCELED"
				if raced {
					own.Executed = "0.01"
				}
				json.NewEncoder(w).Encode(own)
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT"})
			err := b.CancelSpotStops()
			if (err != nil) != raced || deletes != 1 {
				t.Fatalf("err=%v deletes=%d", err, deletes)
			}
		})
	}
}

func TestSpotStopRecoveryRejectsUnconfirmedState(t *testing.T) {
	for _, kind := range []string{"query-failure", "bad-json", "wrong-id", "filled", "wrong-stop"} {
		t.Run(kind, func(t *testing.T) {
			writes, queries := 0, 0
			id := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v3/openOrders" {
					w.Write([]byte("[]"))
					return
				}
				if r.Method == "POST" {
					writes++
					id = r.URL.Query().Get("newClientOrderId")
					w.WriteHeader(503)
					return
				}
				queries++
				row := spotStop(id)
				switch kind {
				case "query-failure":
					w.WriteHeader(503)
					return
				case "bad-json":
					w.Write([]byte("{"))
					return
				case "wrong-id":
					row.ClientID = "other"
				case "filled":
					row.Status = "FILLED"
				case "wrong-stop":
					row.Stop = "89"
				}
				json.NewEncoder(w).Encode(row)
			}))
			defer srv.Close()
			b := NewBinance(BinanceConfig{BaseURL: srv.URL, Symbol: "BTCUSDT", StepSize: 0.00001})
			if err := b.PlaceSpotStop(0.123456, 90); err == nil || writes != 1 || queries != 1 {
				t.Fatalf("err=%v writes=%d queries=%d", err, writes, queries)
			}
		})
	}
}

func TestSpotProtectionDryRun(t *testing.T) {
	b := NewBinance(BinanceConfig{DryRun: true, BaseURL: "http://invalid.invalid"})
	if err := b.PlaceSpotStop(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.CancelSpotStops(); err != nil {
		t.Fatal(err)
	}
}
