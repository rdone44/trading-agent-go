package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// Recovery and cycle gates must not report rounded or non-decimal prices as
// verified coverage. Repeat inspections with and without a persisted intent.
func TestInspectProtectiveRequiresExactDecimalCoverage(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		for _, pending := range []bool{false, true} {
			for _, kind := range []string{"STOP_MARKET", "TAKE_PROFIT_MARKET"} {
				for _, price := range []string{"90.000000000000001", "0x1.68p+6", "90.0000"} {
					t.Run(fmt.Sprintf("%s/pending=%t/%s/%s", side, pending, kind, price), func(t *testing.T) {
						closeSide := "SELL"
						if side == Sell {
							closeSide = "BUY"
						}
						rows := []protectiveAlgoOrder{
							{AlgoID: 1, AlgoStatus: "NEW", ClientAlgoID: "tap-stop", Symbol: "BTCUSDT", OrderType: "STOP_MARKET", Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: "90", ClosePos: true},
							{AlgoID: 2, AlgoStatus: "NEW", ClientAlgoID: "tap-target", Symbol: "BTCUSDT", OrderType: "TAKE_PROFIT_MARKET", Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: "90", ClosePos: true},
						}
						for i := range rows {
							if rows[i].OrderType == kind {
								rows[i].TriggerPrice = price
							}
						}
						requests := 0
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							requests++
							if r.Method != http.MethodGet || r.URL.Path != "/fapi/v1/openAlgoOrders" {
								t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
								http.Error(w, "read only", http.StatusBadRequest)
								return
							}
							_ = json.NewEncoder(w).Encode(rows)
						}))
						defer srv.Close()
						b := newLiveFuturesForStub(srv.URL)
						b.cfg.Leverage = 2
						if pending {
							b.cfg.ProtectiveJournalPath = filepath.Join(t.TempDir(), "pending.json")
							_, err := createProtectiveIntent(b.cfg.ProtectiveJournalPath, "BTCUSDT", side, []protectiveIntentLeg{
								{OrderType: "STOP_MARKET", ClientAlgoID: "tap-stop", TriggerPrice: 90},
								{OrderType: "TAKE_PROFIT_MARKET", ClientAlgoID: "tap-target", TriggerPrice: 90},
							})
							if err != nil {
								t.Fatal(err)
							}
						}
						want := ProtectionConflict
						if price == "90.0000" {
							want = ProtectionVerified
						}
						for cycle := 0; cycle < 3; cycle++ {
							got, err := b.InspectProtective(side, 90, 90)
							if err != nil || got.State != want {
								t.Fatalf("state=%s err=%v, want %s", got.State, err, want)
							}
						}
						if requests != 3 {
							t.Fatalf("requests=%d, want 3 read-only inspections", requests)
						}
						// Freeze the historical Leverage:1 inspection behavior.
						b.cfg.Leverage = 1
						legacy, err := b.InspectProtective(side, 90, 90)
						if err != nil || legacy.State != ProtectionVerified {
							t.Fatalf("Leverage:1 changed: state=%s err=%v", legacy.State, err)
						}
					})
				}
			}
		}
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
