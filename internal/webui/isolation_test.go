package webui_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

// wireRisingSession points an account's session at deterministic offline bars
// so the isolation tests never touch Binance.
func wireRisingSession(t *testing.T, server *webui.Server, username string) {
	t.Helper()
	sess := server.Session(username)
	sess.SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		series := model.Series{Symbol: symbol, Source: "offline-isolation"}
		for i := 0; i < days; i++ {
			price := float64(i + 100)
			series.Bars = append(series.Bars, model.Bar{
				Time: end.AddDate(0, 0, i-days), Open: price, High: price + 1, Low: price - 1, Close: price,
			})
		}
		return series, nil
	}
	sess.PriceLoader = func(string) (float64, time.Time, error) { return 100, time.Now(), nil }
}

// Regression: accounts used to share one on-disk ledger
// ("paper-spot-<symbol>.json" with no account segment), so a second user
// starting the same symbol inherited the first user's position, cash and
// pending order journal. Each account now gets its own path, and a state file
// written by another account is refused even when the path is forced.
func TestAccountStateFilesDoNotCollide(t *testing.T) {
	server, _ := newAuthServer(t)
	alice := register(t, server, "iso_alice", "offline-pass-a")
	bob := register(t, server, "iso_bob", "offline-pass-b")
	wireRisingSession(t, server, "iso_alice")
	wireRisingSession(t, server, "iso_bob")

	if rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600,"initial_cash":100000}`, alice); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if err := server.Session("iso_alice").Step(); err != nil {
		t.Fatal(err)
	}
	if err := server.Session("iso_alice").Stop(); err != nil {
		t.Fatal(err)
	}
	aliceState := getSessionAs(t, server, alice)
	if !aliceState.Position.Open {
		t.Fatal("precondition: alice should hold a position after the step")
	}

	if rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600,"initial_cash":1000}`, bob); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	defer server.Session("iso_bob").Stop()
	bobStarted := getSessionAs(t, server, bob)

	if aliceState.StatePath == bobStarted.StatePath {
		t.Fatalf("accounts share the ledger path %s", bobStarted.StatePath)
	}
	// Bob starts flat with his own 1,000. His first cycle legitimately opens
	// its own position, so the inherited-book check must happen before any
	// cycle: the give-away is Alice's 100,000 initial cash and her quantity
	// and entry price, none of which can come from Bob's request.
	if bobStarted.InitialCash != 1000 {
		t.Fatalf("bob inherited initial cash %.0f, want his own 1000", bobStarted.InitialCash)
	}
	if bobStarted.Position.Open && bobStarted.Position.EntryPrice == aliceState.Position.EntryPrice &&
		bobStarted.Position.Quantity == aliceState.Position.Quantity {
		t.Fatalf("bob inherited alice's position: %+v", bobStarted.Position)
	}
}

// Regression: /runs/ was served straight out of the shared output directory
// outside the account middleware, so an anonymous visitor could download any
// report by guessing its name and any account could read another's runs.
func TestReportsAreNotPubliclyReadable(t *testing.T) {
	server, _ := newAuthServer(t)
	alice := register(t, server, "rep_alice", "offline-pass-a")
	bob := register(t, server, "rep_bob", "offline-pass-b")

	rec := postJSONAs(t, server, "/api/backtest", `{"symbol":"BTCUSDT","days":120}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var result struct {
		RunName string `json:"run_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	get := func(path, cookie string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != "" {
			request.AddCookie(&http.Cookie{Name: "ta_session", Value: cookie})
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	if got := get("/runs/"+result.RunName+"/report.html", ""); got.Code == http.StatusOK {
		t.Errorf("anonymous report download = %d, want a rejection", got.Code)
	}
	if got := get("/runs/"+result.RunName+"/report.html", bob); got.Code == http.StatusOK {
		t.Errorf("another account's report = %d, want a rejection", got.Code)
	}
	// The owner can still read their own report.
	if got := get("/runs/"+result.RunName+"/report.html", alice); got.Code != http.StatusOK || got.Body.Len() == 0 {
		t.Errorf("owner report = %d (%d bytes), want 200 with content", got.Code, got.Body.Len())
	}
	// And so can /api/run, while another account cannot.
	if got := get("/api/run?name="+result.RunName, bob); got.Code == http.StatusOK {
		t.Errorf("bob /api/run on alice's run = %d, want a rejection", got.Code)
	}
	if got := get("/api/run?name="+result.RunName, alice); got.Code != http.StatusOK {
		t.Errorf("owner /api/run = %d, want 200", got.Code)
	}
}

// Regression: an account with no stored Binance credentials used to fall back
// to the deployer's BINANCE_API_KEY/BINANCE_SECRET_KEY and could start a live
// session on the operator's exchange account.
func TestKeylessAccountCannotUseDeployerExchangeKeys(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "1")
	t.Setenv("BINANCE_API_KEY", "deployer-key")
	t.Setenv("BINANCE_SECRET_KEY", "deployer-secret")

	server, _ := newAuthServer(t)
	cookie := register(t, server, "keyless", "offline-pass")
	wireRisingSession(t, server, "keyless")

	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("no network is permitted in this test (got %s %s)", r.Method, r.URL.Path)
	})

	rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600,"execute":true,"confirm":"确认实盘"}`, cookie)
	if rec.Code == http.StatusOK {
		t.Fatal("a keyless account started a live session on the deployer's keys")
	}
	if body := rec.Body.String(); !strings.Contains(body, "密钥") {
		t.Fatalf("expected the missing-keys message, got: %s", body)
	}
}

// The same account that stores its own keys still passes the gate, so the fix
// does not break the vault path.
func TestStoredKeysStillPassGateWithEnvDisabled(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "1")
	t.Setenv("BINANCE_API_KEY", "deployer-key")
	t.Setenv("BINANCE_SECRET_KEY", "deployer-secret")

	server, _ := newAuthServer(t)
	cookie := register(t, server, "ownkeys", "offline-pass")
	wireRisingSession(t, server, "ownkeys")
	if rec := postJSONCookie(t, server, "/api/auth/credentials", map[string]string{
		"binance_api_key": "VK", "binance_secret_key": "VS",
	}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("set credentials = %d: %s", rec.Code, rec.Body.String())
	}

	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("no network is permitted in this test (got %s %s)", r.Method, r.URL.Path)
	})

	rec := postJSONAs(t, server, "/api/session/start",
		`{"symbol":"BTCUSDT","days":120,"interval_seconds":3600,"execute":true,"confirm":"确认实盘"}`, cookie)
	if body := rec.Body.String(); strings.Contains(body, "密钥") && strings.Contains(body, "填写") {
		t.Fatalf("stored keys were ignored: %s", body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
