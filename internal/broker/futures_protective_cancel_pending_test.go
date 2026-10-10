package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A missing pending leg may have triggered, not merely disappeared. Never
// authorize a local exit or remove another leg based on the open list alone.
func TestCancelProtectivePreflightsPendingIdentities(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		for _, scenario := range []string{"empty", "missing stop", "missing target", "replacement identity", "wrong trigger", "wrong symbol", "wrong side", "corrupt intent", "matching"} {
			t.Run(fmt.Sprint(side)+"/"+scenario, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "protective-pending.json")
				stop, target, closeSide := 90.0, 110.0, "SELL"
				if side == Sell {
					stop, target, closeSide = 110, 90, "BUY"
				}
				intentSide, symbol := side, "BTCUSDT"
				if scenario == "wrong symbol" {
					symbol = "ETHUSDT"
				}
				if scenario == "wrong side" {
					if side == Buy {
						intentSide = Sell
					} else {
						intentSide = Buy
					}
				}
				if _, err := createProtectiveIntent(path, symbol, intentSide, []protectiveIntentLeg{
					{OrderType: "STOP_MARKET", ClientAlgoID: "tap-stop", TriggerPrice: stop},
					{OrderType: "TAKE_PROFIT_MARKET", ClientAlgoID: "tap-target", TriggerPrice: target},
				}); err != nil {
					t.Fatal(err)
				}
				if scenario == "corrupt intent" {
					if err := os.WriteFile(path, []byte(`{`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				rows := []protectiveAlgoOrder{
					{AlgoID: 1, AlgoStatus: "NEW", ClientAlgoID: "tap-stop", Symbol: "BTCUSDT", OrderType: "STOP_MARKET", Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: fmt.Sprint(stop), ClosePos: true},
					{AlgoID: 2, AlgoStatus: "NEW", ClientAlgoID: "tap-target", Symbol: "BTCUSDT", OrderType: "TAKE_PROFIT_MARKET", Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: fmt.Sprint(target), ClosePos: true},
				}
				switch scenario {
				case "empty":
					rows = nil
				case "missing stop":
					rows = rows[1:]
				case "missing target":
					rows = rows[:1]
				case "replacement identity":
					rows[1].ClientAlgoID = "tap-replacement"
				case "wrong trigger":
					rows[1].TriggerPrice = "120"
				}
				writes := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.Method + " " + r.URL.Path {
					case "GET /fapi/v1/openAlgoOrders":
						_ = json.NewEncoder(w).Encode(rows)
					case "DELETE /fapi/v1/algoOrder":
						writes++
						for _, row := range rows {
							if fmt.Sprint(row.AlgoID) == r.URL.Query().Get("algoId") {
								_ = json.NewEncoder(w).Encode(map[string]interface{}{"algoId": row.AlgoID, "clientAlgoId": row.ClientAlgoID, "code": "200", "msg": "success"})
								return
							}
						}
						http.Error(w, "unknown identity", 400)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
					}
				}))
				defer srv.Close()
				b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: 2, ProtectiveJournalPath: path})
				err = b.CancelProtective()
				wantSuccess := scenario == "matching"
				if (err == nil) != wantSuccess {
					t.Fatalf("CancelProtective = %v, want success=%v", err, wantSuccess)
				}
				wantWrites := 0
				if wantSuccess {
					wantWrites = 2
				}
				if writes != wantWrites {
					t.Fatalf("writes=%d, want %d", writes, wantWrites)
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatalf("cancellation changed pending evidence: %v", err)
				}
				// The explicitly frozen Leverage:1 path must ignore pending
				// evidence exactly as before, including malformed intent files.
				writes = 0
				legacy := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: 1, ProtectiveJournalPath: path})
				if err := legacy.CancelProtective(); err != nil || writes != len(rows) {
					t.Fatalf("Leverage:1 changed: err=%v writes=%d want=%d", err, writes, len(rows))
				}
			})
		}
	}
}
