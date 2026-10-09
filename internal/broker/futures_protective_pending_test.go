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

func TestProtectivePendingReusesOriginalIDsWithoutPosting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.protective-pending.json")
	if _, err := createProtectiveIntent(path, "BTCUSDT", Buy, []protectiveIntentLeg{
		{OrderType: "STOP_MARKET", ClientAlgoID: "tap-original-stop", TriggerPrice: 90},
		{OrderType: "TAKE_PROFIT_MARKET", ClientAlgoID: "tap-original-target", TriggerPrice: 110},
	}); err != nil {
		t.Fatal(err)
	}

	writes := 0
	rows := []protectiveAlgoOrder{
		{AlgoID: 1, AlgoStatus: "NEW", ClientAlgoID: "tap-original-stop", Symbol: "BTCUSDT", OrderType: "STOP_MARKET", Side: "SELL", PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: "90", ClosePos: true},
		{AlgoID: 2, AlgoStatus: "NEW", ClientAlgoID: "tap-original-target", Symbol: "BTCUSDT", OrderType: "TAKE_PROFIT_MARKET", Side: "SELL", PositionSide: "BOTH", WorkingType: "MARK_PRICE", TriggerPrice: "110", ClosePos: true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openAlgoOrders" {
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		writes++
		http.Error(w, "must not write", http.StatusBadRequest)
	}))
	defer srv.Close()

	b := NewFutures(FuturesConfig{
		Symbol: "BTCUSDT", BaseURL: srv.URL,
		ProtectiveJournalPath: path,
	})
	if err := b.PlaceProtective(Buy, 90, 110); err != nil {
		t.Fatalf("existing original legs were not accepted: %v", err)
	}
	got, err := b.InspectProtective(Buy, 90, 110)
	if err != nil || got.State != ProtectionVerified {
		t.Fatalf("inspection = %+v err=%v", got, err)
	}
	if writes != 0 {
		t.Fatalf("pending recovery posted %d new order(s)", writes)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("broker cleared the intent before the runner saved its ledger: %v", err)
	}
}

func TestProtectivePendingMissingLegNeverBlindlyReposts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.protective-pending.json")
	if _, err := createProtectiveIntent(path, "BTCUSDT", Buy, []protectiveIntentLeg{
		{OrderType: "STOP_MARKET", ClientAlgoID: "tap-original-stop", TriggerPrice: 90},
	}); err != nil {
		t.Fatal(err)
	}

	writes, idQueries := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /fapi/v1/openAlgoOrders":
			_, _ = fmt.Fprint(w, `[]`)
		case "GET /fapi/v1/algoOrder":
			idQueries++
			if r.URL.Query().Get("clientAlgoId") != "tap-original-stop" {
				t.Error("did not query the original pending identity")
			}
			http.Error(w, `{"code":-2013,"msg":"Order does not exist"}`, http.StatusBadRequest)
		default:
			writes++
			http.Error(w, "must not write", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	b := NewFutures(FuturesConfig{
		Symbol: "BTCUSDT", BaseURL: srv.URL,
		ProtectiveJournalPath: path,
	})
	if err := b.PlaceProtective(Buy, 90, 0); err == nil {
		t.Fatal("missing pending leg was accepted")
	}
	if writes != 0 || idQueries != 1 {
		t.Fatalf("writes=%d idQueries=%d, want 0/1", writes, idQueries)
	}
}
