// USDT-margined perpetual futures market data, from the public fapi REST API.
// Same K-line shape as spot; the venue difference matters for live trading
// (leverage, shorting) and for marking stops at the exchange price.
package marketdata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
)

// fapiEndpoint is the USDT-margined futures REST base. It is a variable so
// tests can point the client at a local server.
var fapiEndpoint = "https://fapi.binance.com"

// Futures loads daily bars for a USDT-margined perpetual from the public
// Binance futures REST API. Crypto futures trade every day, so N days of
// history maps to roughly N candles; the still-forming candle is dropped so
// a backtest never acts on a partial bar.
func Futures(symbol string, days int, end time.Time) (model.Series, error) {
	return klinesFor(fapiEndpoint, "binance-usdt-perp", symbol, days, end)
}

// klinesFor pages through one K-line endpoint and returns closed daily bars.
func klinesFor(baseURL, source, symbol string, days int, end time.Time) (model.Series, error) {
	if days < 1 {
		days = 1
	}
	ticker := BinanceSymbol(symbol)
	if ticker == "" {
		return model.Series{}, fmt.Errorf("Binance 数据源需要交易对代码，例如 BTCUSDT")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	cursor := end.AddDate(0, 0, -days)

	klines := make([]binanceKline, 0, days+1)
	for len(klines) < days+1 {
		page, err := fetchKlinesPage(client, baseURL, ticker, cursor, end)
		if err != nil {
			return model.Series{}, err
		}
		if len(page) == 0 {
			break
		}
		klines = append(klines, page...)
		// A short page means the exchange has nothing older to give.
		if len(page) < binanceMaxLimit {
			break
		}
		next := page[len(page)-1].bar.Time.Add(time.Millisecond)
		if !next.Before(end) {
			break
		}
		cursor = next
	}

	if len(klines) == 0 {
		return model.Series{}, fmt.Errorf("Binance 没有返回 %s 的任何K线，请确认交易对是否存在", ticker)
	}

	bars := make([]model.Bar, 0, len(klines))
	for _, k := range klines {
		if k.closeTime.After(end) {
			continue // still forming: its close is not a real close yet
		}
		bars = append(bars, k.bar)
	}
	if len(bars) == 0 {
		return model.Series{}, fmt.Errorf("Binance 返回的 %s 只有尚未收盘的K线", ticker)
	}
	if len(bars) > days {
		bars = bars[len(bars)-days:]
	}
	return model.Series{Symbol: ticker, Bars: bars, Source: source}, nil
}

// fetchKlinesPage fetches one page of daily candles from a K-line endpoint.
// The spot and futures endpoints share the row layout but live at different
// paths, so the URL is built per venue.
func fetchKlinesPage(client *http.Client, baseURL, ticker string, start, end time.Time) ([]binanceKline, error) {
	query := url.Values{}
	query.Set("symbol", ticker)
	query.Set("interval", "1d")
	query.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	query.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
	query.Set("limit", strconv.Itoa(binanceMaxLimit))

	var u string
	switch baseURL {
	case fapiEndpoint:
		u = baseURL + "/fapi/v1/klines?" + query.Encode()
	default:
		u = baseURL + "/api/v3/klines?" + query.Encode()
	}

	request, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 Binance 请求失败: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	rows, err := binanceRequest(client, request, ticker)
	if err != nil {
		return nil, err
	}

	out := make([]binanceKline, 0, len(rows))
	for i, row := range rows {
		if len(row) < 7 {
			return nil, fmt.Errorf("Binance 第 %d 条K线只有 %d 个字段，至少需要 7 个", i, len(row))
		}
		openTime, err := rawInt64(row[0])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的时间: %w", i, err)
		}
		closeTime, err := rawInt64(row[6])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的收盘时间: %w", i, err)
		}
		open, err := rawFloat(row[1])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的开盘价: %w", i, err)
		}
		high, err := rawFloat(row[2])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的最高价: %w", i, err)
		}
		low, err := rawFloat(row[3])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的最低价: %w", i, err)
		}
		closePrice, err := rawFloat(row[4])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的收盘价: %w", i, err)
		}
		volume, err := rawFloat(row[5])
		if err != nil {
			return nil, fmt.Errorf("Binance 第 %d 条K线的成交量: %w", i, err)
		}
		out = append(out, binanceKline{
			bar: model.Bar{
				Time:   time.UnixMilli(openTime).UTC(),
				Open:   open,
				High:   high,
				Low:    low,
				Close:  closePrice,
				Volume: volume,
			},
			closeTime: time.UnixMilli(closeTime).UTC(),
		})
	}
	return out, nil
}

// FuturesLastPrice fetches the latest traded price for a futures ticker from
// the public fapi REST API. The trade loop calls this each cycle to mark
// open positions and to evaluate protective stops between candles.
func FuturesLastPrice(symbol string) (float64, time.Time, error) {
	ticker := BinanceSymbol(symbol)
	if ticker == "" {
		return 0, time.Time{}, fmt.Errorf("Binance 数据源需要交易对代码，例如 BTCUSDT")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	query := url.Values{}
	query.Set("symbol", ticker)
	query.Set("interval", "1d")
	query.Set("limit", "2")
	request, err := http.NewRequest(http.MethodGet, fapiEndpoint+"/fapi/v1/klines?"+query.Encode(), nil)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("构造 Binance futures 请求失败: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("请求 Binance futures 最新价失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, time.Time{}, fmt.Errorf("Binance futures 返回 HTTP 状态码 %d（%s）",
			response.StatusCode, binanceErrorDetail(response))
	}
	var rows [][]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		return 0, time.Time{}, fmt.Errorf("解析 Binance futures 最新价失败: %w", err)
	}
	if len(rows) == 0 || len(rows[len(rows)-1]) < 5 {
		return 0, time.Time{}, fmt.Errorf("Binance futures 没有返回 %s 的K线", ticker)
	}
	last := rows[len(rows)-1]
	openTime, err := rawInt64(last[0])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("Binance futures 最新K线的时间: %w", err)
	}
	price, err := rawFloat(last[4])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("Binance futures 最新K线的价格: %w", err)
	}
	return price, time.UnixMilli(openTime).UTC(), nil
}

// FuturesMarkPrice fetches the current mark price for a futures ticker.
// Mark prices are what the exchange uses for liquidations and funding, so
// they are the honest reference for judging stops on a leveraged position.
func FuturesMarkPrice(symbol string) (float64, error) {
	ticker := BinanceSymbol(symbol)
	if ticker == "" {
		return 0, fmt.Errorf("Binance 数据源需要交易对代码，例如 BTCUSDT")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	u := fapiEndpoint + "/fapi/v1/premiumIndex?symbol=" + url.QueryEscape(ticker)
	request, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("请求 Binance futures 标记价格失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Binance futures 返回 HTTP 状态码 %d（%s）",
			response.StatusCode, binanceErrorDetail(response))
	}
	var payload struct {
		MarkPrice json.Number `json:"markPrice"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("解析 Binance futures 标记价格失败: %w", err)
	}
	price, err := payload.MarkPrice.Float64()
	if err != nil {
		return 0, fmt.Errorf("标记价格不是数字: %w", err)
	}
	return price, nil
}
