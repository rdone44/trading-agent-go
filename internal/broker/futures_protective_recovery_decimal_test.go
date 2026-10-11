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

// A lost POST response must not turn a rounded conflicting query price into
// confirmed protection, authorize the sibling POST, or discard intent evidence.
func TestProtectiveAlgoRecoveryRequiresExactPrice(t *testing.T) {
	for _, leverage := range []int{1, 2} {
		for _, side := range []Side{Buy, Sell} {
			for _, lostLeg := range []string{"STOP_MARKET", "TAKE_PROFIT_MARKET"} {
				for _, price := range []string{"90.000000000000001", "0x1.68p+6", "90.0000"} {
					t.Run(fmt.Sprintf("leverage=%d/%s/%s/%s", leverage, side, lostLeg, price), func(t *testing.T) {
						var rows []map[string]interface{}
						writes, queries := 0, 0
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							q := r.URL.Query()
							switch r.Method + " " + r.URL.Path {
							case "GET /fapi/v1/openAlgoOrders":
								if rows == nil {
									fmt.Fprint(w, `[]`)
								} else {
									_ = json.NewEncoder(w).Encode(rows)
								}
							case "POST /fapi/v1/algoOrder":
								writes++
								row := recoveredAlgo(q)
								row["algoId"] = writes
								if q.Get("type") == lostLeg {
									row["triggerPrice"] = price
								}
								rows = append(rows, row)
								if q.Get("type") == lostLeg {
									conn, _, err := w.(http.Hijacker).Hijack()
									if err != nil {
										t.Error(err)
										return
									}
									_ = conn.Close()
									return
								}
								fmt.Fprint(w, `{"algoId":1}`)
							case "GET /fapi/v1/algoOrder":
								queries++
								last := rows[len(rows)-1]
								if q.Get("clientAlgoId") != last["clientAlgoId"] || q.Has("algoId") {
									t.Error("query did not use original submitted client identity")
								}
								_ = json.NewEncoder(w).Encode(last)
							default:
								t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
								http.Error(w, "unexpected", 400)
							}
						}))
						defer srv.Close()
						path := filepath.Join(t.TempDir(), "pending.json")
						b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: leverage, ProtectiveJournalPath: path})
						err := b.PlaceProtective(side, 90, 90)
						wantSuccess := leverage == 1 || price == "90.0000"
						if (err == nil) != wantSuccess {
							t.Fatalf("PlaceProtective = %v, want success=%t", err, wantSuccess)
						}
						if !wantSuccess && !strings.Contains(err.Error(), "查单触发价与本地不一致") {
							t.Fatalf("wrong rejection: %v", err)
						}
						wantWrites := 2
						if !wantSuccess && lostLeg == "STOP_MARKET" {
							wantWrites = 1
						}
						if writes != wantWrites || queries != 1 {
							t.Fatalf("writes=%d queries=%d, want %d/1", writes, queries, wantWrites)
						}
						original, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if leverage > 1 {
							for cycle := 0; cycle < 3; cycle++ {
								err := b.PlaceProtective(side, 90, 90)
								if (err == nil) != wantSuccess {
									t.Fatalf("repeat: %v, want success=%t", err, wantSuccess)
								}
							}
							if writes != wantWrites || queries != 1 {
								t.Fatalf("repeated placement wrote or queried again: %d/%d", writes, queries)
							}
						}
						got, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(got, original) {
							t.Fatalf("intent changed: %v", err)
						}
					})
				}
			}
		}
	}
}
