package webui_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/localcreds"
)

func readAccount(t *testing.T, server http.Handler, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/account", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func TestAccountReadIsIsolatedAndDoesNotStartTrading(t *testing.T) {
	server, vault := newAuthServer(t)
	alice := register(t, server, "alice", "s3cret-pass")
	bob := register(t, server, "bob", "s3cret-pass")
	if rec := readAccount(t, server.Handler(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous account read = %d", rec.Code)
	}
	if err := vault.SetCredentials("alice", "alice-key", "alice-secret", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	server.AccountLoader = func(cfg broker.FuturesConfig) (broker.AccountBalance, error) {
		if cfg.APIKey != "alice-key" || cfg.SecretKey != "alice-secret" || !cfg.DryRun {
			t.Fatal("wrong account credentials or broker mode")
		}
		return broker.AccountBalance{Wallet: 12.5, Available: 8.25}, nil
	}
	if rec := readAccount(t, server.Handler(), bob); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "API Key") {
		t.Fatalf("bob read another account: %d %s", rec.Code, rec.Body.String())
	}
	rec := readAccount(t, server.Handler(), alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice read = %d %s", rec.Code, rec.Body.String())
	}
	var data struct {
		Wallet    float64 `json:"wallet"`
		Available float64 `json:"available"`
		Venue     string  `json:"venue"`
		Source    string  `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Wallet != 12.5 || data.Available != 8.25 || data.Venue != "futures" || data.Source != "binance" {
		t.Fatalf("unexpected account result: %+v", data)
	}
	if server.Session("alice").Status().Running {
		t.Fatal("account read started trading")
	}
}

func TestDesktopAccountReadWorksWithLiveGateOffAndReportsErrors(t *testing.T) {
	server, store := newDesktopServer(t)
	t.Setenv("TA_ALLOW_LIVE", "0")
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")
	if rec := readAccount(t, server.Handler(), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key = %d", rec.Code)
	}
	if err := store.Set(localcreds.Patch{BinanceAPIKey: "key", BinanceSecretKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	server.AccountLoader = func(cfg broker.FuturesConfig) (broker.AccountBalance, error) {
		if cfg.APIKey != "key" || cfg.SecretKey != "secret" {
			t.Fatalf("wrong keys")
		}
		return broker.AccountBalance{}, errors.New("Binance HTTP 403 (code -2015): Invalid API-key, IP, or permissions")
	}
	rec := readAccount(t, server.Handler(), "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "-2015") {
		t.Fatalf("account error = %d %s", rec.Code, rec.Body.String())
	}
}
