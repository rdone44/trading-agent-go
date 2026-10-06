package marketdata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// kline builds one row in Binance's own encoding: timestamps as JSON numbers,
// prices and volumes as JSON strings.
func kline(open time.Time, closePrice float64) []any {
	closeTime := open.Add(24*time.Hour - time.Millisecond)
	return []any{
		open.UnixMilli(),
		"100.5",
		"101.5",
		"99.5",
		strconv.FormatFloat(closePrice, 'f', 2, 64),
		"1234.5",
		closeTime.UnixMilli(),
		1, 2, "3", "4", "5",
	}
}

// withBinanceServer points the client at a stub exchange for the test.
func withBinanceServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previous := binanceEndpoint
	binanceEndpoint = server.URL
	t.Cleanup(func() {
		binanceEndpoint = previous
		server.Close()
	})
	return server
}

func TestBinanceSymbolNormalization(t *testing.T) {
	cases := map[string]string{
		"BTCUSDT":   "BTCUSDT",
		"btcusdt":   "BTCUSDT",
		"btc-usdt":  "BTCUSDT",
		"BTC/USDT":  "BTCUSDT",
		" btc_usdt": "BTCUSDT",
		"":          "",
	}
	for input, want := range cases {
		if got := BinanceSymbol(input); got != want {
			t.Errorf("BinanceSymbol(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBinanceLoadsDailyBars(t *testing.T) {
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var gotQuery url.Values

	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/klines" {
			t.Errorf("path = %q, want /api/v3/klines", r.URL.Path)
		}
		gotQuery = r.URL.Query()

		rows := make([][]any, 0, 10)
		for i := 10; i > 0; i-- {
			open := end.AddDate(0, 0, -i).Truncate(24 * time.Hour)
			rows = append(rows, kline(open, float64(100+i)))
		}
		json.NewEncoder(w).Encode(rows)
	})

	series, err := Binance("btc-usdt", 10, end)
	if err != nil {
		t.Fatalf("Binance: %v", err)
	}
	if series.Symbol != "BTCUSDT" {
		t.Errorf("symbol = %q, want BTCUSDT", series.Symbol)
	}
	if series.Source != "binance" {
		t.Errorf("source = %q, want binance", series.Source)
	}
	if len(series.Bars) != 10 {
		t.Fatalf("bars = %d, want 10", len(series.Bars))
	}
	if gotQuery.Get("symbol") != "BTCUSDT" || gotQuery.Get("interval") != "1d" {
		t.Errorf("query = %v, want symbol=BTCUSDT interval=1d", gotQuery)
	}
	if gotQuery.Get("limit") != "1000" {
		t.Errorf("limit = %q, want 1000", gotQuery.Get("limit"))
	}

	// Bars must arrive in ascending time order with every field populated.
	for i, bar := range series.Bars {
		if i > 0 && !bar.Time.After(series.Bars[i-1].Time) {
			t.Fatalf("bar %d is out of order: %s", i, bar.Time)
		}
		if bar.Open != 100.5 || bar.High != 101.5 || bar.Low != 99.5 || bar.Volume != 1234.5 {
			t.Fatalf("bar %d parsed incorrectly: %+v", i, bar)
		}
	}
	if last := series.Bars[len(series.Bars)-1]; !last.Time.Before(end) {
		t.Errorf("last bar %s is not before the request end %s", last.Time, end)
	}
}

// A daily candle whose close time is still in the future has not finished
// forming; acting on it would be look-ahead bias.
func TestBinanceDropsTheStillFormingCandle(t *testing.T) {
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		rows := [][]any{}
		for i := 5; i >= 1; i-- {
			open := end.AddDate(0, 0, -i).Truncate(24 * time.Hour)
			rows = append(rows, kline(open, 100))
		}
		// Today's candle: opened at 00:00, closes at 23:59, still open at noon.
		rows = append(rows, kline(end.Truncate(24*time.Hour), 999))
		json.NewEncoder(w).Encode(rows)
	})

	series, err := Binance("BTCUSDT", 30, end)
	if err != nil {
		t.Fatalf("Binance: %v", err)
	}
	if len(series.Bars) != 5 {
		t.Fatalf("bars = %d, want 5 (the open candle must be dropped)", len(series.Bars))
	}
	for _, bar := range series.Bars {
		if bar.Close == 999 {
			t.Fatal("a candle that has not closed was included")
		}
	}
}

func TestBinancePaginatesBeyondOneThousandBars(t *testing.T) {
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	const days = 1005
	requests := 0
	var starts []int64

	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		query := r.URL.Query()
		start, err := strconv.ParseInt(query.Get("startTime"), 10, 64)
		if err != nil {
			t.Errorf("startTime %q is not a number", query.Get("startTime"))
		}
		starts = append(starts, start)

		rows := [][]any{}
		cursor := time.UnixMilli(start).UTC()
		for i := 0; i < binanceMaxLimit; i++ {
			if !cursor.Before(end) {
				break
			}
			rows = append(rows, kline(cursor, 100))
			cursor = cursor.Add(24 * time.Hour)
		}
		json.NewEncoder(w).Encode(rows)
	})

	series, err := Binance("BTCUSDT", days, end)
	if err != nil {
		t.Fatalf("Binance: %v", err)
	}
	if requests < 2 {
		t.Fatalf("requests = %d, want at least 2 (the client must page)", requests)
	}
	if len(starts) > 1 && starts[1] <= starts[0] {
		t.Errorf("second page start %d does not advance past the first %d", starts[1], starts[0])
	}
	if len(series.Bars) != days {
		t.Fatalf("bars = %d, want %d", len(series.Bars), days)
	}
	for i := 1; i < len(series.Bars); i++ {
		if !series.Bars[i].Time.After(series.Bars[i-1].Time) {
			t.Fatalf("bar %d is out of order after paging", i)
		}
	}
}

func TestBinanceReportsExchangeErrors(t *testing.T) {
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":-1121,"msg":"Invalid symbol."}`)
	})

	_, err := Binance("NOPEUSDT", 10, time.Now().UTC())
	if err == nil {
		t.Fatal("expected an error for an unknown symbol")
	}
	if !strings.Contains(err.Error(), "Invalid symbol.") {
		t.Errorf("error %q does not surface the exchange message", err)
	}
}

func TestBinanceRejectsEmptyAndMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty array", `[]`},
		{"short row", `[[1499040000000,"1","2"]]`},
		{"bad price", `[[1499040000000,"abc","1","1","1","1",1499644799999]]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			})
			if _, err := Binance("BTCUSDT", 10, time.Now().UTC()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestBinanceNeedsASymbol(t *testing.T) {
	if _, err := Binance("", 10, time.Now().UTC()); err == nil {
		t.Fatal("expected an error for an empty symbol")
	}
}

// The public endpoint occasionally drops a TLS handshake or answers 5xx; one
// flaky connection must not fail the whole backtest.
func TestBinanceRetriesTransientFailures(t *testing.T) {
	end := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	attempts := 0

	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"code":-1001,"msg":"Internal error"}`)
			return
		}
		rows := [][]any{}
		for i := 3; i >= 1; i-- {
			rows = append(rows, kline(end.AddDate(0, 0, -i).Truncate(24*time.Hour), 100))
		}
		json.NewEncoder(w).Encode(rows)
	})

	series, err := Binance("BTCUSDT", 3, end)
	if err != nil {
		t.Fatalf("Binance should recover from a 502, got: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if len(series.Bars) != 3 {
		t.Errorf("bars = %d, want 3", len(series.Bars))
	}
}

// A 4xx is a definitive answer from the exchange: retrying only wastes time.
func TestBinanceDoesNotRetryClientErrors(t *testing.T) {
	attempts := 0
	withBinanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":-1121,"msg":"Invalid symbol."}`)
	})

	if _, err := Binance("NOPEUSDT", 10, time.Now().UTC()); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a 400 must not be retried)", attempts)
	}
}
