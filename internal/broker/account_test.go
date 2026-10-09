package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFuturesAccountReadsOnlySignedBalanceWithExchangeClock(t *testing.T) {
	serverTime := time.Now().Add(2 * time.Hour).UnixMilli()
	var balanceCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, serverTime)
		case "/fapi/v2/balance":
			balanceCalls++
			if r.Method != http.MethodGet || r.Header.Get("X-MBX-APIKEY") != "key" || r.URL.Query().Get("signature") == "" {
				t.Errorf("balance request must be signed GET")
			}
			fmt.Fprint(w, `[{"asset":"BTC","balance":"9"},{"asset":"USDT","balance":"12.50","availableBalance":"8.25","crossUnPnl":"-0.75"}]`)
		default:
			t.Errorf("account read attempted an unexpected exchange call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	b := NewFutures(FuturesConfig{BaseURL: srv.URL, APIKey: "key", SecretKey: "secret"})
	account, err := b.Account()
	if err != nil {
		t.Fatal(err)
	}
	if balanceCalls != 1 || account.Wallet != 12.5 || account.Available != 8.25 || account.CrossUnrealized != -0.75 {
		t.Fatalf("balance calls=%d, account=%+v", balanceCalls, account)
	}
}

func TestFuturesAccountRejectsMissingUSDTAndInvalidNumbers(t *testing.T) {
	for _, response := range []string{`[]`, `[{"asset":"BTC","balance":"10"}]`, `[{"asset":"USDT","balance":"NaN","availableBalance":"2"}]`, `[{"asset":"USDT","balance":"1","availableBalance":"bad"}]`, `[{"asset":"USDT","balance":"1","availableBalance":"1"}]`} {
		t.Run(response, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fapi/v1/time" {
					fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
					return
				}
				fmt.Fprint(w, response)
			}))
			defer srv.Close()
			b := NewFutures(FuturesConfig{BaseURL: srv.URL, APIKey: "key", SecretKey: "secret"})
			if _, err := b.Account(); err == nil {
				t.Fatal("invalid or absent USDT balance was accepted")
			}
		})
	}
}

func TestFuturesAccountSurfacesExchangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fapi/v1/time" {
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`)
	}))
	defer srv.Close()
	b := NewFutures(FuturesConfig{BaseURL: srv.URL, APIKey: "key", SecretKey: "secret"})
	if _, err := b.Account(); err == nil || !strings.Contains(err.Error(), "-2015") {
		t.Fatalf("exchange error not surfaced: %v", err)
	}
}
