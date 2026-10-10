package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCancelProtectiveRequiresUntriggeredTerminalOrder(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		for _, scenario := range []string{"canceled", "NEW", "TRIGGERED", "FINISHED", "wrong id", "wrong client", "wrong symbol", "wrong side", "wrong type", "wrong position", "wrong working type", "wrong trigger", "rounded trigger", "equivalent trigger", "hex trigger", "fraction trigger", "not close all", "child order", "missing child", "triggered time", "missing time", "actual price", "missing price", "actual quantity", "invalid quantity", "underflow price", "underflow quantity", "exponent zero", "empty", "http failure"} {
			t.Run(side+"/"+scenario, func(t *testing.T) {
				deletes, queries := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.Method + " " + r.URL.Path {
					case "GET /fapi/v1/openAlgoOrders":
						fmt.Fprintf(w, `[{"algoId":1,"clientAlgoId":"tap-stop","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true},{"algoId":2,"clientAlgoId":"tap-target","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}]`, side, side)
					case "DELETE /fapi/v1/algoOrder":
						deletes++
						client := "tap-stop"
						if deletes == 2 {
							client = "tap-target"
						}
						json.NewEncoder(w).Encode(map[string]interface{}{"algoId": deletes, "clientAlgoId": client, "code": "200", "msg": "success"})
					case "GET /fapi/v1/algoOrder":
						queries++
						if r.URL.Query().Get("algoId") != fmt.Sprint(queries) {
							t.Errorf("unexpected terminal query: %v", r.URL.Query())
						}
						row := map[string]interface{}{"algoId": queries, "clientAlgoId": "tap-stop", "symbol": "BTCUSDT", "orderType": "STOP_MARKET", "side": side, "positionSide": "BOTH", "workingType": "MARK_PRICE", "triggerPrice": "90", "closePosition": true, "algoStatus": "CANCELED", "actualOrderId": "", "actualPrice": "0.00000", "triggerTime": 0}
						if queries == 2 {
							row["clientAlgoId"] = "tap-target"
							row["orderType"] = "TAKE_PROFIT_MARKET"
							row["triggerPrice"] = "110"
						}
						if queries == 1 {
							switch scenario {
							case "NEW", "TRIGGERED", "FINISHED":
								row["algoStatus"] = scenario
							case "wrong id":
								row["algoId"] = 3
							case "wrong client":
								row["clientAlgoId"] = "manual"
							case "wrong symbol":
								row["symbol"] = "ETHUSDT"
							case "wrong side":
								row["side"] = "OTHER"
							case "wrong type":
								row["orderType"] = "TAKE_PROFIT_MARKET"
							case "wrong position":
								row["positionSide"] = "LONG"
							case "wrong working type":
								row["workingType"] = "CONTRACT_PRICE"
							case "wrong trigger":
								row["triggerPrice"] = "91"
							case "rounded trigger":
								row["triggerPrice"] = "90.000000000000001"
							case "equivalent trigger":
								row["triggerPrice"] = "90.0000"
							case "hex trigger":
								row["triggerPrice"] = "0x1.68p+6"
							case "fraction trigger":
								row["triggerPrice"] = "180/2"
							case "not close all":
								row["closePosition"] = false
							case "child order":
								row["actualOrderId"] = "99"
							case "missing child":
								delete(row, "actualOrderId")
							case "triggered time":
								row["triggerTime"] = 1
							case "missing time":
								delete(row, "triggerTime")
							case "actual price":
								row["actualPrice"] = "100"
							case "missing price":
								delete(row, "actualPrice")
							case "actual quantity":
								row["actualQty"] = "0.1"
							case "invalid quantity":
								row["actualQty"] = "NaN"
							case "underflow price":
								row["actualPrice"] = "1e-400"
							case "underflow quantity":
								row["actualQty"] = "1e-400"
							case "exponent zero":
								row["actualQty"] = "0e-400"
							case "empty":
								row = map[string]interface{}{}
							case "http failure":
								http.Error(w, "terminal unavailable", 503)
								return
							}
						}
						json.NewEncoder(w).Encode(row)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
					}
				}))
				defer srv.Close()
				b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: 2})
				err := b.CancelProtective()
				ok := scenario == "canceled" || scenario == "exponent zero" || scenario == "equivalent trigger"
				if (err == nil) != ok {
					t.Fatalf("CancelProtective = %v, want success=%v", err, ok)
				}
				want := 1
				if ok {
					want = 2
				}
				if deletes != want || queries != want {
					t.Fatalf("DELETEs=%d terminal GETs=%d, want %d (no retry/next leg on doubt)", deletes, queries, want)
				}
			})
		}
	}
}
