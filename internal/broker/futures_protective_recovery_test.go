package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The stub accepts placement but drops its response, reproducing an ambiguous
// network failure without contacting an exchange or sleeping for a timeout.
func TestProtectiveAlgoLostResponseRecovery(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		for _, lostLeg := range []string{"STOP_MARKET", "TAKE_PROFIT_MARKET"} {
			t.Run(string(side)+"/"+lostLeg, func(t *testing.T) {
				var orders []map[string]interface{}
				writes, queries := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					q := r.URL.Query()
					switch r.Method + " " + r.URL.Path {
					case "GET /fapi/v1/openAlgoOrders":
						if orders == nil {
							fmt.Fprint(w, `[]`)
						} else {
							json.NewEncoder(w).Encode(orders)
						}
					case "POST /fapi/v1/algoOrder":
						writes++
						row := recoveredAlgo(q)
						orders = append(orders, row)
						if q.Get("type") == lostLeg {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						}
						fmt.Fprint(w, `{"algoId":1}`)
					case "GET /fapi/v1/algoOrder":
						queries++
						if q.Get("signature") == "" || q.Get("timestamp") == "" || q.Has("algoId") {
							t.Error("invalid signed client-ID query")
						}
						for _, row := range orders {
							if row["clientAlgoId"] == q.Get("clientAlgoId") {
								json.NewEncoder(w).Encode(row)
								return
							}
						}
						http.Error(w, "unknown ID", 404)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
					}
				}))
				defer srv.Close()
				b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
				for i := 0; i < 3; i++ {
					if err := b.PlaceProtective(side, 90, 110); err != nil {
						t.Fatal(err)
					}
				}
				if writes != 2 || queries != 1 {
					t.Fatalf("writes=%d queries=%d; want 2/1", writes, queries)
				}
			})
		}
	}
}

func recoveredAlgo(q url.Values) map[string]interface{} {
	return map[string]interface{}{
		"algoId": 1, "algoStatus": "NEW", "clientAlgoId": q.Get("clientAlgoId"),
		"symbol": q.Get("symbol"), "orderType": q.Get("type"), "side": q.Get("side"),
		"positionSide": q.Get("positionSide"), "workingType": q.Get("workingType"),
		"triggerPrice": q.Get("triggerPrice"), "closePosition": true,
	}
}

func TestProtectiveAlgoRecoveryRejectsUnconfirmedOrder(t *testing.T) {
	for _, fault := range []string{"unavailable", "bad_json", "algoId", "clientAlgoId", "symbol", "orderType", "side", "positionSide", "workingType", "triggerPrice", "closePosition", "algoStatus", "nan_trigger"} {
		t.Run(fault, func(t *testing.T) {
			writes, queries := 0, 0
			var submitted url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /fapi/v1/openAlgoOrders":
					fmt.Fprint(w, `[]`)
				case "POST /fapi/v1/algoOrder":
					writes++
					submitted = r.URL.Query()
					http.Error(w, "accepted response lost", 503)
				case "GET /fapi/v1/algoOrder":
					queries++
					if r.URL.Query().Get("clientAlgoId") != submitted.Get("clientAlgoId") {
						t.Error("did not query submitted identity")
					}
					switch fault {
					case "unavailable":
						http.Error(w, "offline", 503)
						return
					case "bad_json":
						fmt.Fprint(w, `{`)
						return
					}
					row := recoveredAlgo(submitted)
					switch fault {
					case "algoId":
						row[fault] = 0
					case "closePosition":
						row[fault] = false
					case "triggerPrice":
						row[fault] = "91"
					case "nan_trigger":
						row["triggerPrice"] = "NaN"
					default:
						row[fault] = "wrong"
					}
					json.NewEncoder(w).Encode(row)
				default:
					t.Error("unexpected request")
					http.Error(w, "unexpected", 400)
				}
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			if err := b.PlaceProtective(Buy, 90, 110); err == nil {
				t.Fatal("unconfirmed protection accepted")
			}
			if writes != 1 || queries != 1 {
				t.Fatalf("writes=%d queries=%d; want 1/1", writes, queries)
			}
		})
	}
}
