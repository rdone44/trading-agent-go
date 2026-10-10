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
			for _, failure := range []string{"empty_ack", "wrong_identity", "http_failure"} {
				t.Run(fmt.Sprintf("%s/leg%d/%s", side, failedLeg, failure), func(t *testing.T) {
					stop, target, quantity, closeSide := 90.0, 110.0, "1", "SELL"
					if side == broker.Sell {
						stop, target, quantity, closeSide = 110, 90, "-1", "BUY"
					}
					requests, deletes, orderWrites, priceCalls := 0, 0, 0, 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						requests++
						w.Header().Set("Content-Type", "application/json")
						switch req.Method + " " + req.URL.Path {
						case "GET /fapi/v2/positionRisk":
							_, _ = fmt.Fprintf(w, `[{"symbol":"BTCUSDT","positionAmt":%q,"entryPrice":"100","positionSide":"BOTH"}]`, quantity)
						case "GET /fapi/v1/openAlgoOrders":
							// The snapshot deliberately stays NEW: a failed response
							// must not be cleared by a later apparently healthy read.
							_, _ = fmt.Fprintf(w, `[{"algoId":1,"clientAlgoId":"tap-stop","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":%q,"closePosition":true},{"algoId":2,"clientAlgoId":"tap-target","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":%q,"closePosition":true}]`, closeSide, fmt.Sprint(stop), closeSide, fmt.Sprint(target))
						case "DELETE /fapi/v1/algoOrder":
							deletes++
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
					b := broker.NewFutures(broker.FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
					a := engine.NewWithBroker(cfg, strategy.MovingAverageCross{}, b, portfolio.New(1000, 1), risk.New(cfg.Risk))
					a.RestoreState(1000, 1000, &engine.OpenTrade{Quantity: 1, EntryPrice: 100, EntryFee: 0.04, Side: side}, stop, target, risk.RiskState{})
					a.Protective = &futuresProtective{b: b}
					r := &Runner{cfg: cfg, agent: a, broker: b, executed: true, leverage: 1, statePath: filepath.Join(t.TempDir(), "ledger.json")}
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
