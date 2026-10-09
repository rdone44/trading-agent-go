package marketdata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Candle struct {
	Time   int64   `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

type MarketSnapshot struct {
	Symbol    string    `json:"symbol"`
	Venue     string    `json:"venue"`
	Interval  string    `json:"interval"`
	Price     float64   `json:"price"`
	ChangePct float64   `json:"change_pct"`
	High      float64   `json:"high"`
	Low       float64   `json:"low"`
	Volume    float64   `json:"volume"`
	UpdatedAt time.Time `json:"updated_at"`
	Candles   []Candle  `json:"candles"`
}

func ValidMarketInterval(interval string) bool {
	switch interval {
	case "15m", "1h", "4h", "1d":
		return true
	}
	return false
}

// Snapshot fetches the console's market view (24h ticker plus recent candles)
// from the perpetual endpoint. There is no spot variant: the terminal trades
// perpetuals, so the chart must show the market the orders execute in.
func Snapshot(symbol, interval string) (MarketSnapshot, error) {
	symbol = BinanceSymbol(symbol)
	if !ValidMarketInterval(interval) {
		return MarketSnapshot{}, fmt.Errorf("不支持的K线周期")
	}
	base, prefix, venue := fapiEndpoint, "/fapi/v1", "futures"
	client := &http.Client{Timeout: 12 * time.Second}
	get := func(path string, out any) error {
		resp, err := client.Get(base + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Binance 行情 HTTP %d%s", resp.StatusCode, binanceErrorDetail(resp))
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
	}
	var ticker struct {
		Price  json.Number `json:"lastPrice"`
		Change json.Number `json:"priceChangePercent"`
		High   json.Number `json:"highPrice"`
		Low    json.Number `json:"lowPrice"`
		Volume json.Number `json:"quoteVolume"`
	}
	if err := get(prefix+"/ticker/24hr?symbol="+url.QueryEscape(symbol), &ticker); err != nil {
		return MarketSnapshot{}, err
	}
	out := MarketSnapshot{Symbol: symbol, Venue: venue, Interval: interval, UpdatedAt: time.Now().UTC()}
	out.Price, _ = ticker.Price.Float64()
	out.ChangePct, _ = ticker.Change.Float64()
	out.High, _ = ticker.High.Float64()
	out.Low, _ = ticker.Low.Float64()
	out.Volume, _ = ticker.Volume.Float64()
	var rows [][]json.RawMessage
	if err := get(prefix+"/klines?symbol="+url.QueryEscape(symbol)+"&interval="+interval+"&limit=100", &rows); err != nil {
		return MarketSnapshot{}, err
	}
	for _, row := range rows {
		if len(row) < 6 {
			return MarketSnapshot{}, fmt.Errorf("K线字段不完整")
		}
		ts, err := rawInt64(row[0])
		if err != nil {
			return MarketSnapshot{}, err
		}
		var values [5]float64
		for i := range values {
			values[i], err = rawFloat(row[i+1])
			if err != nil {
				return MarketSnapshot{}, err
			}
		}
		out.Candles = append(out.Candles, Candle{Time: ts, Open: values[0], High: values[1], Low: values[2], Close: values[3], Volume: values[4]})
	}
	if out.Price <= 0 || len(out.Candles) == 0 {
		return MarketSnapshot{}, fmt.Errorf("%s 无可用行情", strings.ToUpper(symbol))
	}
	return out, nil
}
