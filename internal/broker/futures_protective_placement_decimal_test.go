package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Preflight must reject a conflicting surviving leg before repairing its
// missing sibling, even when float64 rounds the exchange price to the intent.
func TestPlaceProtectiveRequiresExactExistingPrice(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		for _, pending := range []bool{false, true} {
			for _, kind := range []string{"STOP_MARKET", "TAKE_PROFIT_MARKET"} {
				for _, price := range []string{"90.000000000000001", "0x1.68p+6", "90.0000"} {
					t.Run(fmt.Sprintf("%s/pending=%t/%s/%s", side, pending, kind, price), func(t *testing.T) {
						closeSide := "SELL"
						if side == Sell {
							closeSide = "BUY"
						}
						rows := []protectiveAlgoOrder{{AlgoID: 1, AlgoStatus: "NEW", ClientAlgoID: "tap-existing", Symbol: "BTCUSDT", OrderType: kind, Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: price, ClosePos: true}}
						writes, reads := 0, 0
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							switch r.Method + " " + r.URL.Path {
							case "GET /fapi/v1/openAlgoOrders":
								reads++
								_ = json.NewEncoder(w).Encode(rows)
							case "POST /fapi/v1/algoOrder":
								writes++
								q := r.URL.Query()
								rows = append(rows, protectiveAlgoOrder{AlgoID: 2, AlgoStatus: "NEW", ClientAlgoID: q.Get("clientAlgoId"), Symbol: "BTCUSDT", OrderType: q.Get("type"), Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: q.Get("triggerPrice"), ClosePos: true})
								fmt.Fprint(w, `{"algoId":2}`)
							default:
								t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
								http.Error(w, "unexpected", 400)
							}
						}))
						defer srv.Close()
						b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: 2})
						var original []byte
						if pending {
							b.cfg.ProtectiveJournalPath = filepath.Join(t.TempDir(), "pending.json")
							if _, err := createProtectiveIntent(b.cfg.ProtectiveJournalPath, "BTCUSDT", side, []protectiveIntentLeg{{OrderType: kind, ClientAlgoID: "tap-existing", TriggerPrice: 90}}); err != nil {
								t.Fatal(err)
							}
							var err error
							original, err = os.ReadFile(b.cfg.ProtectiveJournalPath)
							if err != nil {
								t.Fatal(err)
							}
						}
						for cycle := 0; cycle < 3; cycle++ {
							err := b.PlaceProtective(side, 90, 90)
							// Pending evidence cannot authorize a missing sibling even
							// when the surviving price is equivalent.
							wantSuccess := price == "90.0000" && !pending
							if (err == nil) != wantSuccess {
								t.Fatalf("PlaceProtective = %v, want success=%t", err, wantSuccess)
							}
							if price != "90.0000" && (err == nil || !strings.Contains(err.Error(), "拒绝重复补挂")) {
								t.Fatalf("conflict did not stop at preflight: %v", err)
							}
						}
						wantWrites := 0
						if price == "90.0000" && !pending {
							wantWrites = 1
						}
						if writes != wantWrites || reads != 3 {
							t.Fatalf("writes=%d reads=%d, want %d/3", writes, reads, wantWrites)
						}
						if pending {
							got, err := os.ReadFile(b.cfg.ProtectiveJournalPath)
							if err != nil || !bytes.Equal(got, original) {
								t.Fatalf("intent changed: err=%v", err)
							}
						}
						// The historical leverage-one float comparison is frozen.
						b.cfg.Leverage = 1
						b.cfg.ProtectiveJournalPath = ""
						if err := b.PlaceProtective(side, 90, 90); err != nil {
							t.Fatalf("Leverage:1 changed: %v", err)
						}
					})
				}
			}
		}
	}
}
