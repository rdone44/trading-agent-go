package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/state"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// A cancellation failure must reach the persisted runner halt, not just the
// broker's return value. In particular, a good first ACK cannot authorize a
// flatten when the second leg's ACK is unverified.
func TestProtectiveCancelFailurePersistsWithoutFlattenOrRetry(t *testing.T) {
	for _, side := range []broker.Side{broker.Buy, broker.Sell} {
		for _, failedLeg := range []int{1, 2} {
			for _, failure := range []string{"empty_ack", "wrong_identity", "http_failure", "terminal_triggered", "terminal_missing", "terminal_http_failure", "terminal_underflow_price", "terminal_underflow_quantity", "other_leg_triggered", "other_leg_partial_fill", "first_leg_triggered", "first_leg_partial_fill", "sibling_during_terminal_read", "sibling_partial_during_terminal_read"} {
				// Race the first DELETE against either its own trigger or the
				// sibling's trigger. Own-leg evidence must stop the second DELETE.
				firstLegRace := failure == "first_leg_triggered" || failure == "first_leg_partial_fill"
				terminalReadRace := failure == "sibling_during_terminal_read" || failure == "sibling_partial_during_terminal_read"
				otherLegRace := failure == "other_leg_triggered" || failure == "other_leg_partial_fill" || terminalReadRace
				if (firstLegRace && failedLeg != 1) || (otherLegRace && failedLeg != 2) {
					continue
				}
				t.Run(fmt.Sprintf("%s/leg%d/%s", side, failedLeg, failure), func(t *testing.T) {
					stop, target, quantity, closeSide := 90.0, 110.0, "1", "SELL"
					if side == broker.Sell {
						stop, target, quantity, closeSide = 110, 90, "-1", "BUY"
					}
					requests, deletes, orderWrites, priceCalls := 0, 0, 0, 0
					tracing := firstLegRace || otherLegRace
					racedLegTriggered := false
					var trace []string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						requests++
						trace = append(trace, req.Method+" "+req.URL.Path+" "+req.URL.Query().Get("algoId"))
						w.Header().Set("Content-Type", "application/json")
						switch req.Method + " " + req.URL.Path {
						case "GET /fapi/v2/positionRisk":
							_, _ = fmt.Fprintf(w, `[{"symbol":"BTCUSDT","positionAmt":%q,"entryPrice":"100","positionSide":"BOTH"}]`, quantity)
						case "GET /fapi/v1/openAlgoOrders":
							if firstLegRace && racedLegTriggered {
								// The sibling remains valid protection: do not remove
								// or replace it after the first leg triggers.
								_, _ = fmt.Fprintf(w, `[{"algoId":2,"clientAlgoId":"tap-target","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":%q,"closePosition":true}]`, closeSide, fmt.Sprint(target))
								return
							}
							if tracing && racedLegTriggered {
								// Both legs have left the open list: disappearance
								// cannot override the triggered child's evidence.
								_, _ = fmt.Fprint(w, `[]`)
								return
							}
							// The snapshot deliberately stays NEW: a failed response
							// must not be cleared by a later apparently healthy read.
							_, _ = fmt.Fprintf(w, `[{"algoId":1,"clientAlgoId":"tap-stop","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":%q,"closePosition":true},{"algoId":2,"clientAlgoId":"tap-target","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":%q,"closePosition":true}]`, closeSide, fmt.Sprint(stop), closeSide, fmt.Sprint(target))
						case "GET /fapi/v1/algoOrder":
							leg := deletes
							if terminalReadRace && leg == 1 {
								// The first leg is already canceled. Its clean terminal
								// response says nothing about a sibling that triggers
								// during this read, before the second DELETE.
								racedLegTriggered = true
							}
							if tracing && leg == failedLeg && !racedLegTriggered {
								t.Error("protected leg did not trigger during first cancellation")
							}
							client, kind, level := "tap-stop", "STOP_MARKET", stop
							if leg == 2 {
								client, kind, level = "tap-target", "TAKE_PROFIT_MARKET", target
							}
							row := map[string]interface{}{"algoId": leg, "clientAlgoId": client, "symbol": "BTCUSDT", "side": closeSide, "orderType": kind, "positionSide": "BOTH", "workingType": "MARK_PRICE", "triggerPrice": fmt.Sprint(level), "closePosition": true, "algoStatus": "CANCELED", "actualOrderId": "", "actualPrice": "0", "triggerTime": 0}
							if leg == failedLeg {
								switch failure {
								case "terminal_triggered", "other_leg_triggered", "other_leg_partial_fill", "first_leg_triggered", "first_leg_partial_fill", "sibling_during_terminal_read", "sibling_partial_during_terminal_read":
									row["algoStatus"] = "TRIGGERED"
									row["actualOrderId"] = "999"
									if tracing {
										row["triggerTime"] = 123
										row["actualPrice"] = fmt.Sprint(level)
									}
									if failure == "other_leg_partial_fill" || failure == "first_leg_partial_fill" || failure == "sibling_partial_during_terminal_read" {
										row["actualQty"] = "0.4"
									}
								case "terminal_underflow_price":
									row["actualPrice"] = "1e-400"
								case "terminal_underflow_quantity":
									row["actualQty"] = "1e-400"
								case "terminal_missing":
									delete(row, "actualOrderId")
								case "terminal_http_failure":
									http.Error(w, "terminal unavailable", 503)
									return
								}
							}
							_ = json.NewEncoder(w).Encode(row)
						case "DELETE /fapi/v1/algoOrder":
							deletes++
							if terminalReadRace && deletes == 2 && !racedLegTriggered {
								t.Error("second cancellation happened before the sibling trigger")
							}
							if tracing && !terminalReadRace && deletes == 1 {
								racedLegTriggered = true
							}
							if req.URL.Query().Get("algoId") != fmt.Sprint(deletes) {
								t.Errorf("unexpected cancellation identity: %s", req.URL.Query().Get("algoId"))
							}
							client := "tap-stop"
							if deletes == 2 {
								client = "tap-target"
							}
							if deletes == failedLeg {
								switch failure {
								case "empty_ack":
									_, _ = fmt.Fprint(w, `{}`)
									return
								case "wrong_identity":
									client = "manual-order"
								case "http_failure":
									http.Error(w, "cancel unavailable", http.StatusServiceUnavailable)
									return
								}
							}
							_ = json.NewEncoder(w).Encode(map[string]interface{}{"algoId": deletes, "clientAlgoId": client, "code": "200", "msg": "success"})
						default:
							if req.Method != http.MethodGet {
								orderWrites++
							}
							t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
							http.Error(w, "unexpected", http.StatusBadRequest)
						}
					}))
					defer srv.Close()

					cfg := config.Default()
					leverage := 1
					underflowEvidence := failure == "terminal_underflow_price" || failure == "terminal_underflow_quantity"
					if tracing || underflowEvidence || failure == "terminal_triggered" || failure == "terminal_missing" || failure == "terminal_http_failure" {
						leverage = 2
					}
					b := broker.NewFutures(broker.FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL, Leverage: leverage})
					a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b, portfolio.New(1000, 1), risk.New(cfg.Risk))
					a.RestoreState(1000, 1000, &engine.OpenTrade{Quantity: 1, EntryPrice: 100, EntryFee: 0.04, Side: side}, stop, target, risk.RiskState{})
					a.Protective = &futuresProtective{b: b}
					r := &Runner{cfg: cfg, agent: a, broker: b, executed: true, leverage: 1, statePath: filepath.Join(t.TempDir(), "ledger.json")}
					if tracing || underflowEvidence {
						r.leverage = 2
					}
					r.PriceLoader = func(string) (float64, time.Time, error) {
						priceCalls++
						return stop, time.Now(), nil
					}
					r.SeriesLoader = func(string, int, time.Time) (model.Series, error) {
						t.Fatal("uncertain cancellation fetched strategy history")
						return model.Series{}, nil
					}
					original, position := *a.OpenTrade(), *a.Book.Position("BTCUSDT")
					res, _ := r.Cycle(time.Now())
					if tracing {
						want := []string{"GET /fapi/v2/positionRisk ", "GET /fapi/v1/openAlgoOrders ", "GET /fapi/v1/openAlgoOrders ", "DELETE /fapi/v1/algoOrder 1", "GET /fapi/v1/algoOrder 1", "DELETE /fapi/v1/algoOrder 2", "GET /fapi/v1/algoOrder 2"}
						if firstLegRace {
							want = want[:5]
						}
						if !reflect.DeepEqual(trace, want) {
							t.Fatalf("cancellation race trace = %v, want %v", trace, want)
						}
					}
					if res.Exited || !a.Risk.Halted || !a.Risk.OrderUncertain || deletes != failedLeg || orderWrites != 0 || len(b.Fills()) != 0 || priceCalls != 1 {
						t.Fatalf("unsafe cancellation: result=%+v halted=%v uncertain=%v deletes=%d orders=%d fills=%d prices=%d", res, a.Risk.Halted, a.Risk.OrderUncertain, deletes, orderWrites, len(b.Fills()), priceCalls)
					}
					if a.Book.Cash != 1000 || !reflect.DeepEqual(original, *a.OpenTrade()) || !reflect.DeepEqual(position, *a.Book.Position("BTCUSDT")) {
						t.Fatal("failed cancellation changed the ledger")
					}
					saved, exists, err := state.Load(r.statePath)
					if err != nil || !exists || !saved.Risk.Halted || !saved.Risk.OrderUncertain {
						t.Fatalf("cancellation halt not persisted: exists=%v err=%v risk=%+v", exists, err, saved.Risk)
					}
					cash, peak, open, savedStop, savedTarget, savedRisk := saved.ToEngine()
					if cash != 1000 || !reflect.DeepEqual(original, *open) {
						t.Fatal("saved state invented a fill or fee")
					}
					requestsAtHalt := requests
					for poll := 0; poll < 3; poll++ {
						if _, err := r.Cycle(time.Now()); err == nil {
							t.Fatal("unreconciled cancellation accepted another cycle")
						}
					}
					resumed := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b, portfolio.New(1000, 1), risk.New(cfg.Risk))
					resumed.RestoreState(cash, peak, open, savedStop, savedTarget, savedRisk)
					resumed.Protective = &futuresProtective{b: b}
					r.agent = resumed
					if _, err := r.Cycle(time.Now()); err == nil {
						t.Fatal("restart cleared the uncertain cancellation")
					}
					if requests != requestsAtHalt || priceCalls != 1 || orderWrites != 0 || len(b.Fills()) != 0 {
						t.Fatal("repeat/restart performed exchange, market or fill work")
					}
				})
			}
		}
	}
}
