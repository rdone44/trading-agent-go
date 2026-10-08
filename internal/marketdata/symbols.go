// The venue's full pair list, fetched once per request instead of typed by a
// human. The exchange's 24-hour ticker endpoint called without a symbol filter
// returns every pair at once, which is what "get me all the coins" means: no
// single-symbol choice up front, liquidity-ranked so the useful ones are on
// top.
package marketdata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SymbolInfo is one tradable pair as the exchange's 24-hour view reports it.
// Volume24h is quote volume (denominated in USDT/USDC), the honest "is this
// liquid enough to backtest or trade" number.
type SymbolInfo struct {
	Symbol    string  `json:"symbol"`
	Price     float64 `json:"price"`
	ChangePct float64 `json:"change_pct"`
	Volume24h float64 `json:"volume_24h"`
}

// tickerRow is one entry of the exchange's 24-hour ticker array. Numbers
// arrive as strings, so they are decoded raw and converted per field.
type tickerRow struct {
	Symbol string      `json:"symbol"`
	Last   json.Number `json:"lastPrice"`
	Change json.Number `json:"priceChangePercent"`
	Quote  json.Number `json:"quoteVolume"`
	Status string      `json:"status"`
}

// AllSymbols fetches every pair in a venue in a single exchange request and
// returns the USDT/USDC-quoted pairs that are still trading, sorted by
// 24-hour quote volume, largest first. limit caps the result; 0 means no cap.
// venue is "spot" or "futures"; anything else is treated as spot.
func AllSymbols(venue string, limit int) ([]SymbolInfo, error) {
	base, path := binanceEndpoint, "/api/v3/ticker/24hr"
	if venue == "futures" {
		base, path = fapiEndpoint, "/fapi/v1/ticker/24hr"
	}

	client := &http.Client{Timeout: 20 * time.Second}
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 Binance 请求失败: %w", err)
	}
	response, err := doGetWithRetry(client, request, venue)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Binance 返回 HTTP 状态码 %d%s", response.StatusCode, binanceErrorDetail(response))
	}

	// The whole market is one array; bound the read so a bad answer cannot
	// balloon the process.
	var rows []tickerRow
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&rows); err != nil {
		return nil, fmt.Errorf("解析 Binance 币种列表失败: %w", err)
	}

	out := filterTradable(rows)
	// Liquidity first, symbol as a deterministic tiebreak so two runs with
	// equal volumes print the same order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Volume24h != out[j].Volume24h {
			return out[i].Volume24h > out[j].Volume24h
		}
		return out[i].Symbol < out[j].Symbol
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// filterTradable keeps the quoted pairs the app can actually backtest and
// trade: USDT- or USDC-quoted, and (where the exchange reports a status) not
// halted. It is pure, so it is testable with no HTTP at all.
func filterTradable(rows []tickerRow) []SymbolInfo {
	out := []SymbolInfo{}
	for _, row := range rows {
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if !strings.HasSuffix(symbol, "USDT") && !strings.HasSuffix(symbol, "USDC") {
			continue
		}
		// Spot reports status on halted pairs; futures leaves it empty, which
		// means "assume trading".
		if row.Status != "" && !strings.EqualFold(row.Status, "TRADING") {
			continue
		}
		info := SymbolInfo{Symbol: symbol}
		info.Price, _ = row.Last.Float64()
		info.ChangePct, _ = row.Change.Float64()
		info.Volume24h, _ = row.Quote.Float64()
		out = append(out, info)
	}
	return out
}
