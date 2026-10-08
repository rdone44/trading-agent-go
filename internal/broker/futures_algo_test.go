package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtectiveAlgoStrictWriteContract(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		t.Run(string(side), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != http.MethodPost || r.URL.Path != "/fapi/v1/algoOrder" {
					t.Error("wrong conditional endpoint")
				}
				if q.Get("algoType") != "CONDITIONAL" || !strings.HasPrefix(q.Get("clientAlgoId"), "tap-") || len(q.Get("clientAlgoId")) > 36 {
					t.Error("missing algo identity")
				}
				if q.Get("signature") == "" || q.Get("timestamp") == "" {
					t.Error("unsigned request")
				}
				for _, key := range []string{"price", "stopPrice", "newClientOrderId", "quantity", "reduceOnly"} {
					if q.Has(key) {
						t.Errorf("unexpected field %s", key)
					}
				}
				wantSide := "SELL"
				if side == Sell {
					wantSide = "BUY"
				}
				if q.Get("side") != wantSide || q.Get("triggerPrice") == "" {
					t.Error("invalid side/trigger")
				}
				_, _ = fmt.Fprint(w, `{"algoId":1,"algoStatus":"NEW"}`)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			if err := b.PlaceProtective(side, 90, 110); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d want 2", calls)
			}
		})
	}
}

func TestProtectiveAlgoInspectionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body                 string
		status                     int
		wantHas, hasErr, cancelErr bool
	}{
		{"empty", `[]`, 200, false, false, false},
		{"manual", `[{"algoId":1,"clientAlgoId":"manual","symbol":"BTCUSDT","orderType":"STOP_MARKET","closePosition":true}]`, 200, false, false, false},
		{"missing_id", `[{"clientAlgoId":"tap-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","closePosition":true}]`, 200, true, false, true},
		{"bad_json", `{`, 200, false, true, true},
		{"unavailable", `{"code":-1,"msg":"offline failure"}`, 503, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/fapi/v1/openAlgoOrders" {
					t.Error("must not write on failed/empty inspection")
					http.Error(w, "unexpected", 400)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			has, err := b.HasProtective()
			if has != tc.wantHas || (err != nil) != tc.hasErr {
				t.Fatalf("Has=%v/%v", has, err)
			}
			if err := b.CancelProtective(); (err != nil) != tc.cancelErr {
				t.Fatalf("Cancel=%v", err)
			}
		})
	}
}

func TestProtectiveAlgoWriteFailureNoBlindRetry(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `[{"algoId":1,"clientAlgoId":"tap-stop","symbol":"BTCUSDT","orderType":"STOP_MARKET","closePosition":true}]`)
					return
				}
				if r.Method != method || r.URL.Path != "/fapi/v1/algoOrder" {
					t.Error("wrong write endpoint")
				}
				writes++
				http.Error(w, `{"code":-1,"msg":"offline rejection"}`, 503)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			var err error
			if method == http.MethodPost {
				err = b.PlaceProtective(Buy, 90, 110)
			} else {
				err = b.CancelProtective()
			}
			if err == nil || writes != 1 {
				t.Fatalf("error=%v writes=%d", err, writes)
			}
		})
	}
}
