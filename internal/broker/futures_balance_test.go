package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUSDTBalanceRequiresOneValidWalletRow(t *testing.T) {
	tests := map[string]string{
		"missing":   `[{"asset":"BTC","balance":"1"}]`,
		"bad":       `[{"asset":"USDT","balance":"NaN"}]`,
		"negative":  `[{"asset":"USDT","balance":"-1"}]`,
		"duplicate": `[{"asset":"USDT","balance":"1"},{"asset":"usdt","balance":"1"}]`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
			if _, err := b.USDTBalance(); err == nil {
				t.Fatalf("invalid wallet response accepted: %s", body)
			}
		})
	}
}

func TestUSDTBalanceReturnsValidatedValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"asset":"BTC","balance":"3"},{"asset":"USDT","balance":"12.5"}]`)
	}))
	defer srv.Close()
	b := NewFutures(FuturesConfig{Symbol: "BTCUSDT", BaseURL: srv.URL})
	got, err := b.USDTBalance()
	if err != nil || got != 12.5 {
		t.Fatalf("balance=%v err=%v", got, err)
	}
}
