package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenPositionRejectsInvalidExchangeEvidence(t *testing.T) {
	tests := map[string]string{
		"missing symbol":     `[]`,
		"bad quantity":       `[{"symbol":"BTCUSDT","positionAmt":"NaN","entryPrice":"100","positionSide":"BOTH"}]`,
		"bad entry":          `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"NaN","positionSide":"BOTH"}]`,
		"missing open entry": `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"0","positionSide":"BOTH"}]`,
		"flat with entry":    `[{"symbol":"BTCUSDT","positionAmt":"0","entryPrice":"100","positionSide":"BOTH"}]`,
		"hedge mode":         `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"LONG"}]`,
		"duplicate symbol": `[
{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"},
{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"100","positionSide":"BOTH"}]`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/fapi/v2/positionRisk" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			if _, _, _, err := b.OpenPosition(); err == nil {
				t.Fatalf("invalid position evidence accepted: %s", body)
			}
		})
	}
}

func TestOpenPositionReturnsValidatedLongAndShort(t *testing.T) {
	for _, tc := range []struct {
		name string
		amt  string
		side Side
		qty  float64
	}{
		{"long", "1.25", Buy, 1.25},
		{"short", "-2.5", Sell, 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `[{"symbol":"BTCUSDT","positionAmt":%q,"entryPrice":"100.5","positionSide":"BOTH"}]`, tc.amt)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			side, qty, entry, err := b.OpenPosition()
			if err != nil || side != tc.side || qty != tc.qty || entry != 100.5 {
				t.Fatalf("position = %s/%v/%v err=%v", side, qty, entry, err)
			}
		})
	}
}
