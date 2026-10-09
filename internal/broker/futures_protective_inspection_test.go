package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInspectProtectiveReportsExactCoverage(t *testing.T) {
	tests := []struct {
		name  string
		pos   Side
		stop  float64
		take  float64
		body  string
		state ProtectionState
	}{
		{
			name: "verified long",
			pos:  Buy, stop: 90, take: 110,
			body: `[{
  "algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-stop","symbol":"BTCUSDT",
  "orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true
},{
  "algoId":2,"algoStatus":"NEW","clientAlgoId":"tap-target","symbol":"BTCUSDT",
  "orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true
}]`,
			state: ProtectionVerified,
		},
		{
			name: "missing stop",
			pos:  Buy, stop: 90, take: 110,
			body: `[{
  "algoId":2,"algoStatus":"NEW","clientAlgoId":"tap-target","symbol":"BTCUSDT",
  "orderType":"TAKE_PROFIT_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"110","closePosition":true
}]`,
			state: ProtectionPartial,
		},
		{
			name: "missing all",
			pos:  Buy, stop: 90, take: 110,
			body:  `[]`,
			state: ProtectionMissing,
		},
		{
			name:  "flat with no legs",
			body:  `[]`,
			state: ProtectionNotRequired,
		},
		{
			name: "flat with residual leg",
			body: `[{
  "algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-stop","symbol":"BTCUSDT",
  "orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true
}]`,
			state: ProtectionConflict,
		},
		{
			name: "wrong trigger",
			pos:  Buy, stop: 90, take: 110,
			body: `[{
  "algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-stop","symbol":"BTCUSDT",
  "orderType":"STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"91","closePosition":true
}]`,
			state: ProtectionConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/fapi/v1/openAlgoOrders" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			b := newLiveFuturesForStub(srv.URL)
			got, err := b.InspectProtective(tt.pos, tt.stop, tt.take)
			if err != nil {
				t.Fatalf("InspectProtective error: %v", err)
			}
			if got.State != tt.state {
				t.Fatalf("state = %q, want %q; snapshot=%+v", got.State, tt.state, got)
			}
			if got.CheckedAt.IsZero() {
				t.Fatal("inspection must carry a check timestamp")
			}
		})
	}
}

func TestInspectProtectiveTransportFailureIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	srv.Close()

	b := newLiveFuturesForStub(srv.URL)
	got, err := b.InspectProtective(Buy, 90, 110)
	if err == nil || got.State != ProtectionUnknown {
		t.Fatalf("snapshot=%+v err=%v, want unknown with an error", got, err)
	}
}
