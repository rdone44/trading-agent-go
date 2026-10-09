package webui_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/marketdata"
	"github.com/rdone44/trading-agent-go/internal/model"
)

func TestFlatSessionUsesQuoteNotEquityAndKeepsEffectiveSettings(t *testing.T) {
	s := newLiveTestServer(t)
	var loadedDays int
	s.Session("").SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		loadedDays = days
		out := model.Series{Symbol: symbol}
		for i := 0; i < days; i++ {
			out.Bars = append(out.Bars, model.Bar{Time: end.AddDate(0, 0, i-days), Close: float64(days - i)})
		}
		return out, nil
	}
	s.Session("").PriceLoader = func(string) (float64, time.Time, error) { return 81234, time.Now(), nil }
	rec := postJSON(t, s, "/api/session/start", `{"symbol":"ETHUSDT","days":120,"interval_seconds":3600,"risk":{"max_drawdown_pct":0.1}}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	defer s.Session("").Stop()
	if err := s.Session("").Step(); err != nil {
		t.Fatal(err)
	}
	status := getSession(t, s)
	if status.Position.Open || status.LastPrice != 81234 || loadedDays != 120 {
		t.Fatalf("flat quote/lookback wrong: %+v days=%d", status, loadedDays)
	}
	if status.Settings == nil || status.Settings.Symbol != "ETHUSDT" || *status.Settings.Risk.MaxDrawdownPct != .1 {
		t.Fatal("effective configuration lost")
	}
}

func TestSessionStateIsPartitionedByModeVenueSymbol(t *testing.T) {
	s := newLiveTestServer(t)
	if r := postJSON(t, s, "/api/session/start", `{"symbol":"ETHUSDT","interval_seconds":3600}`); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	defer s.Session("").Stop()
	if got := filepath.Base(getSession(t, s).StatePath); got != "paper-futures-ETHUSDT.json" {
		t.Fatalf("unexpected state path %s", got)
	}
}

func TestMarketDoesNotRequireRunningTradingSession(t *testing.T) {
	s, _ := newTestServer(t)
	calls := 0
	s.MarketLoader = func(symbol string, interval string) (marketdata.MarketSnapshot, error) {
		calls++
		return marketdata.MarketSnapshot{Symbol: symbol, Interval: interval, Price: 81234, UpdatedAt: time.Now()}, nil
	}
	for i := 0; i < 2; i++ {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/market?symbol=BTCUSDT&interval=1h", nil))
		if r.Code != 200 {
			t.Fatal(r.Body.String())
		}
	}
	if calls != 1 || s.Session("").Running() {
		t.Fatal("market did not cache, or started a trading session")
	}
}

func TestInvalidParametersFailBeforeTrading(t *testing.T) {
	for _, body := range []string{`{"strategy":"ma_cross","params":{"fast":30,"slow":10}}`, `{"leverage":-1}`, `{"days":10}`, `{"risk":{"max_drawdown_pct":-0.1}}`} {
		s := newLiveTestServer(t)
		r := postJSON(t, s, "/api/session/start", body)
		if r.Code != 400 || s.Session("").Running() {
			t.Fatalf("invalid request started trading: %s => %d", body, r.Code)
		}
	}
}

// /api/symbols must serve the whole venue's pair list offline (injected
// loader, no real exchange call), cache it within the 30s window, honour a
// limit cap, and never start a trading session.
func TestSymbolsEndpointServesCachedList(t *testing.T) {
	s, _ := newTestServer(t)
	calls := 0
	s.SymbolList = func(limit int) ([]marketdata.SymbolInfo, error) {
		calls++
		return []marketdata.SymbolInfo{
			{Symbol: "BTCUSDT", Price: 60000, Volume24h: 500000000},
			{Symbol: "ETHUSDT", Price: 3000, Volume24h: 400000000},
			{Symbol: "SOLUSDT", Price: 100, Volume24h: 300000000},
		}, nil
	}

	// First request loads, second is served from the cache (no extra call).
	for i := 0; i < 2; i++ {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/symbols?venue=spot", nil))
		if r.Code != 200 {
			t.Fatalf("GET /api/symbols = %d: %s", r.Code, r.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("SymbolList called %d times, want 1 (must cache)", calls)
	}
	if s.Session("").Running() {
		t.Fatal("loading a pair list must not start a trading session")
	}

	// limit caps the result without another loader call.
	// A stale bookmark carrying ?venue=spot is tolerated and still answered
	// with the perpetual venue: the query no longer selects a data source.
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/symbols?venue=spot&limit=2", nil))
	var payload struct {
		Venue   string                  `json:"venue"`
		Symbols []marketdata.SymbolInfo `json:"symbols"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/symbols: %v", err)
	}
	if payload.Venue != "futures" || len(payload.Symbols) != 2 {
		t.Fatalf("venue=%q len=%d, want futures/2", payload.Venue, len(payload.Symbols))
	}
	if calls != 1 {
		t.Fatalf("limit re-read the cache wrongly: SymbolList called %d times", calls)
	}
}

// A malformed limit is a client error, not a 500, and the wrong method is
// rejected like every other API route.
func TestSymbolsEndpointRejectsBadLimitAndMethod(t *testing.T) {
	s, _ := newTestServer(t)
	s.SymbolList = func(limit int) ([]marketdata.SymbolInfo, error) {
		return nil, nil
	}

	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/symbols?limit=abc", nil))
	if r.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", r.Code)
	}

	r = httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/api/symbols", nil))
	if r.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/symbols = %d, want 405", r.Code)
	}
}

// When the exchange answer cannot be loaded the endpoint degrades to 502,
// which the console shows as "cannot list coins" rather than a blank page.
func TestSymbolsEndpointLoaderFailureIs502(t *testing.T) {
	s, _ := newTestServer(t)
	s.SymbolList = func(limit int) ([]marketdata.SymbolInfo, error) {
		return nil, errors.New("exchange down")
	}
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/symbols", nil))
	if r.Code != http.StatusBadGateway {
		t.Errorf("loader failure = %d, want 502", r.Code)
	}
}
