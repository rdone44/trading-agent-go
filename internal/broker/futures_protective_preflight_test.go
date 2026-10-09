package broker

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A malformed later row must leave the valid first leg on the exchange.
// Exercise both closing directions and missing fields, not just bad values:
// permissive JSON decoding must never turn incomplete evidence into permission
// to remove protection before the engine's local exit.
func TestCancelProtectivePreflightPreservesFirstLeg(t *testing.T) {
	mutations := []struct {
		name string
		edit func(map[string]interface{})
	}{
		{"missing algo ID", func(r map[string]interface{}) { delete(r, "algoId") }},
		{"negative algo ID", func(r map[string]interface{}) { r["algoId"] = -1 }},
		{"duplicate algo ID", func(r map[string]interface{}) { r["algoId"] = 1 }},
		{"duplicate client ID", func(r map[string]interface{}) { r["clientAlgoId"] = "tap-stop" }},
		{"duplicate type", func(r map[string]interface{}) { r["orderType"] = "STOP_MARKET" }},
		{"missing status", func(r map[string]interface{}) { delete(r, "algoStatus") }},
		{"triggered", func(r map[string]interface{}) { r["algoStatus"] = "TRIGGERED" }},
		{"finished", func(r map[string]interface{}) { r["algoStatus"] = "FINISHED" }},
		{"missing close flag", func(r map[string]interface{}) { delete(r, "closePosition") }},
		{"false close flag", func(r map[string]interface{}) { r["closePosition"] = false }},
		{"invalid close flag", func(r map[string]interface{}) { r["closePosition"] = "TRUE" }},
		{"unknown type", func(r map[string]interface{}) { r["orderType"] = "TRAILING_STOP_MARKET" }},
		{"missing side", func(r map[string]interface{}) { delete(r, "side") }},
		{"opposite side", func(r map[string]interface{}) {
			if r["side"] == "SELL" {
				r["side"] = "BUY"
			} else {
				r["side"] = "SELL"
			}
		}},
		{"missing position side", func(r map[string]interface{}) { delete(r, "positionSide") }},
		{"hedge position side", func(r map[string]interface{}) { r["positionSide"] = "LONG" }},
		{"missing working type", func(r map[string]interface{}) { delete(r, "workingType") }},
		{"contract price", func(r map[string]interface{}) { r["workingType"] = "CONTRACT_PRICE" }},
		{"missing trigger", func(r map[string]interface{}) { delete(r, "triggerPrice") }},
		{"invalid trigger", func(r map[string]interface{}) { r["triggerPrice"] = "invalid" }},
		{"zero trigger", func(r map[string]interface{}) { r["triggerPrice"] = "0" }},
		{"negative trigger", func(r map[string]interface{}) { r["triggerPrice"] = "-1" }},
		{"NaN trigger", func(r map[string]interface{}) { r["triggerPrice"] = "NaN" }},
		{"infinite trigger", func(r map[string]interface{}) { r["triggerPrice"] = "+Inf" }},
	}
	for _, side := range []string{"SELL", "BUY"} {
		for _, mutation := range mutations {
			t.Run(side+"/"+mutation.name, func(t *testing.T) {
				first := map[string]interface{}{
					"algoId": 1, "algoStatus": "NEW", "clientAlgoId": "tap-stop",
					"symbol": "BTCUSDT", "orderType": "STOP_MARKET", "side": side,
					"positionSide": "BOTH", "workingType": "MARK_PRICE",
					"triggerPrice": "90", "closePosition": true,
				}
				second := map[string]interface{}{}
				for key, value := range first {
					second[key] = value
				}
				second["algoId"] = 2
				second["clientAlgoId"] = "tap-target"
				second["orderType"] = "TAKE_PROFIT_MARKET"
				second["triggerPrice"] = "110"
				mutation.edit(second)
				body, err := json.Marshal([]map[string]interface{}{first, second})
				if err != nil {
					t.Fatal(err)
				}
				stub := newProtectiveStub(string(body))
				srv := stub.server()
				defer srv.Close()
				b := newLiveFuturesForStub(srv.URL)
				// Repeating a rejected preflight must not gradually remove legs.
				for cycle := 0; cycle < 3; cycle++ {
					if err := b.CancelProtective(); err == nil {
						t.Fatalf("cycle %d accepted incomplete/conflicting evidence", cycle)
					}
				}
				if got := len(stub.queriesFor(http.MethodGet, "/fapi/v1/openAlgoOrders")); got != 3 {
					t.Fatalf("inspection calls=%d, want 3", got)
				}
				stub.mu.Lock()
				defer stub.mu.Unlock()
				for _, path := range stub.reqPaths {
					if path != "GET /fapi/v1/openAlgoOrders" {
						t.Fatalf("rejected preflight performed unexpected request %q", path)
					}
				}
			})
		}
	}
}
