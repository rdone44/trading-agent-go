package marketdata

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// AllSymbols must hand back exactly the USDT/USDC perpetual pairs, ordered by
// 24h quote volume, largest first. The stub offers a mix: two USDT pairs, one
// USDC pair, and one non-USD pair (to be dropped).
func TestAllSymbolsFiltersAndRanks(t *testing.T) {
	fastRetry(t)
	withFuturesServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/ticker/24hr" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		rows := []map[string]string{
			{"symbol": "BTCUSDT", "lastPrice": "60000", "priceChangePercent": "1", "quoteVolume": "500000000", "status": "TRADING"},
			{"symbol": "ETHUSDT", "lastPrice": "3000", "priceChangePercent": "-2", "quoteVolume": "400000000", "status": "TRADING"},
			{"symbol": "LINKUSDC", "lastPrice": "15", "priceChangePercent": "0", "quoteVolume": "200000000"},
			{"symbol": "BTCBTC", "lastPrice": "1", "priceChangePercent": "0", "quoteVolume": "999999999", "status": "TRADING"}, // not a USDT/USDC pair
		}
		json.NewEncoder(w).Encode(rows)
	})

	infos, err := AllSymbols(0)
	if err != nil {
		t.Fatalf("AllSymbols: %v", err)
	}
	// Exactly three pairs survive: BTCUSDT, ETHUSDT, LINKUSDC.
	if len(infos) != 3 {
		t.Fatalf("pairs = %d, want 3 (got %v)", len(infos), symbolsOnly(infos))
	}
	// Rank check: 500M > 400M > 200M, so the order is BTC, ETH, LINK.
	wantOrder := []string{"BTCUSDT", "ETHUSDT", "LINKUSDC"}
	for i, symbol := range wantOrder {
		if infos[i].Symbol != symbol {
			t.Errorf("rank %d = %s, want %s", i, infos[i].Symbol, symbol)
		}
	}
	// Volume must be parsed as a real number, not left zero.
	if infos[0].Volume24h != 500000000 {
		t.Errorf("top pair volume = %v, want 500000000", infos[0].Volume24h)
	}
	if infos[0].Price != 60000 {
		t.Errorf("top pair price = %v, want 60000", infos[0].Price)
	}
}

// limit must cap the result without dropping the ranking: top 1 returns only
// the single most-liquid pair.
func TestAllSymbolsLimitCaps(t *testing.T) {
	fastRetry(t)
	withFuturesServer(t, func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]string{
			{"symbol": "BTCUSDT", "lastPrice": "1", "quoteVolume": "3"},
			{"symbol": "ETHUSDT", "lastPrice": "2", "quoteVolume": "2"},
			{"symbol": "SOLUSDT", "lastPrice": "3", "quoteVolume": "1"},
		}
		json.NewEncoder(w).Encode(rows)
	})

	infos, err := AllSymbols(2)
	if err != nil {
		t.Fatalf("AllSymbols: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("limited pairs = %d, want 2", len(infos))
	}
	if infos[0].Symbol != "BTCUSDT" || infos[1].Symbol != "ETHUSDT" {
		t.Errorf("limit kept the wrong pairs: %v", symbolsOnly(infos))
	}
}

// A transient 5xx on the ticker call must be retried, like every other
// marketdata request, not fail the pair-list load.
func TestAllSymbolsRetriesTransient(t *testing.T) {
	fastRetry(t)
	attempts := 0
	withFuturesServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"code":-1001,"msg":"internal"}`))
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{
			{"symbol": "BTCUSDT", "lastPrice": "1", "quoteVolume": "5"},
		})
	})

	if _, err := AllSymbols(0); err != nil {
		t.Fatalf("AllSymbols should recover from 502s, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// symbolsOnly is a compact debug projection of a result slice.
func symbolsOnly(infos []SymbolInfo) []string {
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Symbol)
	}
	return out
}

// filterTradable is pure, so its full semantics can be exercised without any
// HTTP: quote-filter, status-filter, and numeric parse all in one table.
func TestFilterTradable(t *testing.T) {
	rows := []tickerRow{
		{Symbol: "btcusdt", Last: "1", Change: "2", Quote: "300", Status: "TRADING"},
		{Symbol: "ETHUSDT", Last: "4", Change: "-1", Quote: "200"},
		{Symbol: "XRPUSDC", Last: "0.5", Change: "0", Quote: "100"},
		{Symbol: "BTCBTC", Last: "1", Quote: "999", Status: "TRADING"},
		{Symbol: "DOGEUSDT", Last: "0.1", Quote: "999", Status: "BREAKING"},
		{Symbol: "BTCUSDT", Last: "0.9", Quote: "999", Status: "HALTED"},
	}
	got := filterTradable(rows)
	if len(got) != 3 {
		t.Fatalf("filterTradable kept %d, want 3 (%v)", len(got), symbolsOnly(got))
	}
	// Ranking: BTCUSDT(300) > ETHUSDT(200) > XRPUSDC(100).
	sort.Slice(got, func(i, j int) bool { return got[i].Volume24h > got[j].Volume24h })
	want := []string{"BTCUSDT", "ETHUSDT", "XRPUSDC"}
	for i, symbol := range want {
		if got[i].Symbol != symbol {
			t.Errorf("rank %d = %s, want %s", i, got[i].Symbol, symbol)
		}
	}
}
