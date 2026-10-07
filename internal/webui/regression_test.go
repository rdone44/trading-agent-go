package webui_test

import (
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
	s.Session().SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		loadedDays = days
		out := model.Series{Symbol: symbol}
		for i := 0; i < days; i++ {
			out.Bars = append(out.Bars, model.Bar{Time: end.AddDate(0, 0, i-days), Close: float64(days - i)})
		}
		return out, nil
	}
	s.Session().PriceLoader = func(string) (float64, time.Time, error) { return 81234, time.Now(), nil }
	rec := postJSON(t, s, "/api/session/start", `{"symbol":"ETHUSDT","days":120,"interval_seconds":3600,"risk":{"max_drawdown_pct":0.1}}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	defer s.Session().Stop()
	if err := s.Session().Step(); err != nil {
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
	defer s.Session().Stop()
	if got := filepath.Base(getSession(t, s).StatePath); got != "paper-spot-ETHUSDT.json" {
		t.Fatalf("unexpected state path %s", got)
	}
}

func TestMarketDoesNotRequireRunningTradingSession(t *testing.T) {
	s, _ := newTestServer(t)
	calls := 0
	s.MarketLoader = func(symbol string, futures bool, interval string) (marketdata.MarketSnapshot, error) {
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
	if calls != 1 || s.Session().Running() {
		t.Fatal("market did not cache, or started a trading session")
	}
}

func TestInvalidParametersFailBeforeTrading(t *testing.T) {
	for _, body := range []string{`{"strategy":"ma_cross","params":{"fast":30,"slow":10}}`, `{"leverage":-1}`, `{"days":10}`, `{"risk":{"max_drawdown_pct":-0.1}}`} {
		s := newLiveTestServer(t)
		r := postJSON(t, s, "/api/session/start", body)
		if r.Code != 400 || s.Session().Running() {
			t.Fatalf("invalid request started trading: %s => %d", body, r.Code)
		}
	}
}
