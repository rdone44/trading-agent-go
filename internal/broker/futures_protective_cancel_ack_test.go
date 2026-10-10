package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTTP 200 alone is not evidence that the identified protective leg was canceled.
// An invalid first acknowledgement must also leave the second leg untouched.
func TestCancelProtectiveRequiresExactAcknowledgement(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		for _, tc := range []struct {
			name string
			body string
			ok   bool
		}{
			{"numeric code", `{"algoId":1,"clientAlgoId":"tap-stop","code":200,"msg":"success"}`, true},
			{"string code", `{"algoId":1,"clientAlgoId":"tap-stop","code":"200","msg":"success"}`, true},
			{"empty object", `{}`, false},
			{"null", `null`, false},
			{"bad JSON", `{`, false},
			{"missing id", `{"clientAlgoId":"tap-stop","code":200,"msg":"success"}`, false},
			{"wrong id", `{"algoId":2,"clientAlgoId":"tap-stop","code":200,"msg":"success"}`, false},
			{"missing client", `{"algoId":1,"code":200,"msg":"success"}`, false},
			{"wrong client", `{"algoId":1,"clientAlgoId":"manual","code":200,"msg":"success"}`, false},
			{"missing code", `{"algoId":1,"clientAlgoId":"tap-stop","msg":"success"}`, false},
			{"failure code", `{"algoId":1,"clientAlgoId":"tap-stop","code":-2011,"msg":"success"}`, false},
			{"missing message", `{"algoId":1,"clientAlgoId":"tap-stop","code":200}`, false},
			{"failure message", `{"algoId":1,"clientAlgoId":"tap-stop","code":200,"msg":"rejected"}`, false},
		} {
			t.Run(side+"/"+tc.name, func(t *testing.T) {
				writes := 0
				gets := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openAlgoOrders" {
						gets++
						_, _ = fmt.Fprintf(w, `[{"algoId":1,"clientAlgoId":"tap-stop","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"STOP_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true},{"algoId":2,"clientAlgoId":"tap-target","algoStatus":"NEW","symbol":"BTCUSDT","orderType":"TAKE_PROFIT_MARKET","side":%q,"positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true}]`, side, side)
						return
					}
					if r.Method != http.MethodDelete || r.URL.Path != "/fapi/v1/algoOrder" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
						return
					}
					writes++
					wantID := fmt.Sprint(writes)
					if r.URL.Query().Get("algoId") != wantID {
						t.Errorf("cancellation id = %q, want %q", r.URL.Query().Get("algoId"), wantID)
					}
					if writes == 1 {
						_, _ = fmt.Fprint(w, tc.body)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"algoId": 2, "clientAlgoId": "tap-target", "code": "200", "msg": "success"})
				}))
				defer srv.Close()
				err := newLiveFuturesForStub(srv.URL).CancelProtective()
				if (err == nil) != tc.ok {
					t.Fatalf("CancelProtective = %v, want success=%v", err, tc.ok)
				}
				wantWrites := 1
				if tc.ok {
					wantWrites = 2
				}
				if writes != wantWrites || gets != 1 {
					t.Fatalf("writes=%d GETs=%d, want %d/1 (no blind retry)", writes, gets, wantWrites)
				}
			})
		}
	}
}
