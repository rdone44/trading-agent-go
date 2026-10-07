package marketdata

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
)

// encodeRows writes Binance-encoded kline rows (see kline in binance_test.go)
// as a JSON array, mirroring the exchange's real response shape.
func encodeRows(t *testing.T, w http.ResponseWriter, rows [][]any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(rows); err != nil {
		t.Fatalf("encode stub rows: %v", err)
	}
}

// CountGapDays must count only the days actually skipped between ascending
// daily bars: contiguous bars are 0, a one-bar series is 0, and a k-day
// separation contributes k-1 missing bars.
func TestCountGapDaysContiguous(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]model.Bar, 5)
	for i := range bars {
		bars[i].Time = base.AddDate(0, 0, i) // every day, no gap
	}
	if got := model.CountGapDays(bars); got != 0 {
		t.Fatalf("contiguous series: CountGapDays = %d, want 0", got)
	}
}

func TestCountGapDaysDetectsMissingDays(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// days 0,1,2 then jump to day 6: days 3,4,5 are missing (3 days).
	bars := []model.Bar{
		{Time: base.AddDate(0, 0, 0)},
		{Time: base.AddDate(0, 0, 1)},
		{Time: base.AddDate(0, 0, 2)},
		{Time: base.AddDate(0, 0, 6)},
	}
	if got := model.CountGapDays(bars); got != 3 {
		t.Fatalf("3-day gap: CountGapDays = %d, want 3", got)
	}
}

func TestCountGapDaysSingleAndEmptySeries(t *testing.T) {
	if got := model.CountGapDays(nil); got != 0 {
		t.Fatalf("empty series: CountGapDays = %d, want 0", got)
	}
	if got := model.CountGapDays([]model.Bar{{Time: time.Now()}}); got != 0 {
		t.Fatalf("one bar: CountGapDays = %d, want 0", got)
	}
}

// The Binance loader must surface skipped days on the returned Series so a
// caller can tell a ragged history from a clean one. This stub drops day 2 of
// a 3-day window, leaving one missing day.
func TestBinanceSeriesReportsGapDays(t *testing.T) {
	fastRetry(t)
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Days 5,4 (then a gap) day 2: day 3 is missing.
		rows := [][]any{
			kline(end.AddDate(0, 0, -5).Truncate(24*time.Hour), 100),
			kline(end.AddDate(0, 0, -4).Truncate(24*time.Hour), 100),
			kline(end.AddDate(0, 0, -2).Truncate(24*time.Hour), 100),
		}
		encodeRows(t, w, rows)
	})

	series, err := Binance("BTCUSDT", 30, end)
	if err != nil {
		t.Fatalf("Binance: %v", err)
	}
	if series.GapDays != 1 {
		t.Fatalf("GapDays = %d, want 1 (one missing day)", series.GapDays)
	}
}

// A clean loader run must report no gaps, confirming the field is not just
// noise: 3 contiguous days -> GapDays 0.
func TestBinanceSeriesCleanHasNoGaps(t *testing.T) {
	fastRetry(t)
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		rows := [][]any{
			kline(end.AddDate(0, 0, -3).Truncate(24*time.Hour), 100),
			kline(end.AddDate(0, 0, -2).Truncate(24*time.Hour), 100),
			kline(end.AddDate(0, 0, -1).Truncate(24*time.Hour), 100),
		}
		encodeRows(t, w, rows)
	})

	series, err := Binance("BTCUSDT", 30, end)
	if err != nil {
		t.Fatalf("Binance: %v", err)
	}
	if series.GapDays != 0 {
		t.Fatalf("clean run: GapDays = %d, want 0", series.GapDays)
	}
}

// The price endpoints now retry transient 5xx like the K-line endpoint: a 502
// twice then success must recover and must not retry a 400.
func TestLastPriceRetriesTransient(t *testing.T) {
	fastRetry(t)
	attempts := 0
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"code":-1001,"msg":"Internal error"}`))
			return
		}
		// A valid 2-row klines body; LastPrice reads the last row's close.
		rows := [][]any{
			kline(time.Now().AddDate(0, 0, -1).Truncate(24*time.Hour), 100),
			kline(time.Now().Truncate(24*time.Hour), 105),
		}
		encodeRows(t, w, rows)
	})

	price, _, err := LastPrice("BTCUSDT")
	if err != nil {
		t.Fatalf("LastPrice should recover from 502s, got: %v", err)
	}
	if price != 105 {
		t.Errorf("LastPrice = %v, want 105", price)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// A 400 on the price endpoint is definitive and must not be retried.
func TestLastPriceDoesNotRetryClientError(t *testing.T) {
	fastRetry(t)
	attempts := 0
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
	})

	if _, _, err := LastPrice("NOPEUSDT"); err == nil {
		t.Fatal("expected an error for an unknown symbol")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a 400 must not be retried)", attempts)
	}
}
