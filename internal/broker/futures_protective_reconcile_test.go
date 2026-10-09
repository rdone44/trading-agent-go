package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtectiveRepairsMissingLegIdempotently(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		for _, existing := range []string{"", "STOP_MARKET", "TAKE_PROFIT_MARKET", "both"} {
			t.Run(string(side)+"/"+existing, func(t *testing.T) {
				closeSide := "SELL"
				if side == Sell {
					closeSide = "BUY"
				}
				rows := []protectiveAlgoOrder{}
				add := func(kind, price string) {
					rows = append(rows, protectiveAlgoOrder{AlgoID: int64(len(rows) + 1), AlgoStatus: "NEW", ClientAlgoID: fmt.Sprintf("tap-%d", len(rows)+1), Symbol: "BTCUSDT", OrderType: kind, Side: closeSide, PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: price, ClosePos: true})
				}
				if existing == "STOP_MARKET" || existing == "both" {
					add("STOP_MARKET", "90.000")
				}
				if existing == "TAKE_PROFIT_MARKET" || existing == "both" {
					add("TAKE_PROFIT_MARKET", "110.000")
				}
				initial := len(rows)
				writes := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openAlgoOrders" {
						_ = json.NewEncoder(w).Encode(rows)
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/fapi/v1/algoOrder" {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
						return
					}
					q := r.URL.Query()
					if q.Get("side") != closeSide {
						t.Error("wrong close direction")
					}
					writes++
					add(q.Get("type"), q.Get("triggerPrice"))
					_, _ = fmt.Fprint(w, `{"algoId":3,"algoStatus":"NEW"}`)
				}))
				defer srv.Close()
				b := newLiveFuturesForStub(srv.URL)
				for i := 0; i < 3; i++ {
					if err := b.PlaceProtective(side, 90, 110); err != nil {
						t.Fatal(err)
					}
				}
				if writes != 2-initial || len(rows) != 2 {
					t.Fatalf("writes=%d rows=%d initial=%d", writes, len(rows), initial)
				}
			})
		}
	}
}

func TestProtectiveRejectsConflictingLegBeforeAnyWrite(t *testing.T) {
	for _, field := range []string{"id", "side", "position", "working", "price", "bad_price", "duplicate", "missing_price"} {
		t.Run(field, func(t *testing.T) {
			row := protectiveAlgoOrder{AlgoID: 1, AlgoStatus: "NEW", ClientAlgoID: "tap-stop", Symbol: "BTCUSDT", OrderType: "STOP_MARKET", Side: "SELL", PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: "90", ClosePos: true}
			switch field {
			case "id":
				row.AlgoID = 0
			case "side":
				row.Side = "BUY"
			case "position":
				row.PositionSide = "LONG"
			case "working":
				row.WorkingType = "CONTRACT_PRICE"
			case "price":
				row.TriggerPrice = "91"
			case "bad_price":
				row.TriggerPrice = "NaN"
			case "missing_price":
				row.TriggerPrice = ""
			}
			rows := []protectiveAlgoOrder{row}
			if field == "duplicate" {
				rows = append(rows, row)
			}
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(rows)
					return
				}
				writes++
				http.Error(w, "must not write", 400)
			}))
			defer srv.Close()
			if err := newLiveFuturesForStub(srv.URL).PlaceProtective(Buy, 90, 110); err == nil || writes != 0 {
				t.Fatalf("error=%v writes=%d", err, writes)
			}
		})
	}
}

func TestProtectivePartialPlacementFailureDoesNotDuplicateStop(t *testing.T) {
	rows := []protectiveAlgoOrder{}
	stopWrites, targetWrites := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		q := r.URL.Query()
		kind := q.Get("type")
		if kind == "STOP_MARKET" {
			stopWrites++
		} else {
			targetWrites++
			if targetWrites == 1 {
				http.Error(w, "offline target rejection", http.StatusServiceUnavailable)
				return
			}
		}
		rows = append(rows, protectiveAlgoOrder{AlgoID: int64(len(rows) + 1), AlgoStatus: "NEW", ClientAlgoID: "tap-" + kind,
			Symbol: "BTCUSDT", OrderType: kind, Side: "SELL", PositionSide: "BOTH",
			WorkingType: "MARK_PRICE", TriggerPrice: q.Get("triggerPrice"), ClosePos: true})
		_, _ = fmt.Fprint(w, `{"algoId":1,"algoStatus":"NEW"}`)
	}))
	defer srv.Close()
	b := newLiveFuturesForStub(srv.URL)
	if err := b.PlaceProtective(Buy, 90, 110); err == nil {
		t.Fatal("target rejection must surface")
	}
	for i := 0; i < 2; i++ {
		if err := b.PlaceProtective(Buy, 90, 110); err != nil {
			t.Fatal(err)
		}
	}
	if stopWrites != 1 || targetWrites != 2 {
		t.Fatalf("stop writes=%d target writes=%d", stopWrites, targetWrites)
	}
}

func TestProtectiveInspectionFailurePreventsPlacement(t *testing.T) {
	for _, body := range []string{`{`, `{"code":-1}`} {
		t.Run(body, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()
			if err := newLiveFuturesForStub(srv.URL).PlaceProtective(Buy, 90, 110); err == nil || writes != 0 {
				t.Fatalf("error=%v writes=%d", err, writes)
			}
		})
	}
}

func TestProtectiveUnknownOwnedTypePreventsPlacement(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openAlgoOrders" {
			_, _ = fmt.Fprint(w, `[{"algoId":1,"algoStatus":"NEW","clientAlgoId":"tap-unknown","symbol":"BTCUSDT","orderType":"TRAILING_STOP_MARKET","side":"SELL","positionSide":"BOTH","workingType":"MARK_PRICE","triggerPrice":"90","closePosition":true}]`)
			return
		}
		writes++
		http.Error(w, "must not write", http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := newLiveFuturesForStub(srv.URL).PlaceProtective(Buy, 90, 110); err == nil {
		t.Fatal("unknown owned algo type was ignored")
	}
	if writes != 0 {
		t.Fatalf("unknown owned algo caused %d write(s)", writes)
	}
}
