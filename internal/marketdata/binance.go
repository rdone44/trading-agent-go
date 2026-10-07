package marketdata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
)

// binanceEndpoint is the spot REST base. It is a variable so tests can point
// the client at a local server.
var binanceEndpoint = "https://api.binance.com"

// binanceMaxLimit is the largest page one klines request may return.
const binanceMaxLimit = 1000

// binanceAttempts is how many times a page is requested before giving up.
// The public endpoint occasionally drops a TLS handshake, and a single flaky
// connection should not fail a whole backtest.
const binanceAttempts = 3

// binanceBackoffs is the wait before retries 2 and 3: exponential 1s/2s/4s.
// Index attempt-2 in it. It is a variable so a test can shrink it.
var binanceBackoffs = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// binanceSleep is the one wait binanceRequest performs between attempts. It
// is a variable so a test can replace it with a no-op and keep the suite fast.
var binanceSleep = func(d time.Duration) { time.Sleep(d) }

// binanceBackoff returns the wait before the attempt-numbered retry.
func binanceBackoff(attempt int) time.Duration {
	if attempt-1 >= len(binanceBackoffs) {
		return binanceBackoffs[len(binanceBackoffs)-1]
	}
	return binanceBackoffs[attempt-1]
}

// doGetWithRetry performs one GET, retrying transient failures (5xx, network
// errors) with the same exponential backoff binanceRequest uses. A 4xx is a
// definitive exchange answer and is returned immediately. The winning response
// is left open for the caller to read and close.
func doGetWithRetry(client *http.Client, request *http.Request, ticker string) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= binanceAttempts; attempt++ {
		if attempt > 1 {
			binanceSleep(binanceBackoff(attempt))
		}
		response, err := client.Do(request)
		if err != nil {
			lastErr = fmt.Errorf("请求 Binance %s 失败: %w", ticker, err)
			continue
		}
		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("Binance 返回 %s 的 HTTP 状态码 %d%s",
				ticker, response.StatusCode, binanceErrorDetail(response))
			response.Body.Close()
			continue
		}
		return response, nil
	}
	return nil, fmt.Errorf("%w（已重试 %d 次）", lastErr, binanceAttempts)
}

// BinanceSymbol normalizes a ticker into Binance's compact form, so "BTCUSDT",
// "btc-usdt" and "BTC/USDT" all resolve to the same market.
func BinanceSymbol(symbol string) string {
	compact := strings.NewReplacer("-", "", "_", "", "/", "", " ", "").Replace(strings.TrimSpace(symbol))
	return strings.ToUpper(compact)
}

// binanceKline is one candle plus its close time, which is what tells us
// whether the candle has finished forming.
type binanceKline struct {
	bar       model.Bar
	closeTime time.Time
}

// Binance loads daily bars from the public Binance spot REST API.
//
// Crypto trades every day, so N days of history maps to roughly N candles. The
// still-forming candle of the current day is dropped so a backtest never acts
// on a partial bar.
func Binance(symbol string, days int, end time.Time) (model.Series, error) {
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
		page, err := binanceKlines(client, ticker, cursor, end)
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
	series := model.Series{Symbol: ticker, Bars: bars, Source: "binance"}
	series.GapDays = model.CountGapDays(series.Bars)
	return series, nil
}

// LastPrice fetches the most recent price for a ticker from the public
// Binance REST API. The trade loop calls this each cycle to mark open
// positions and to evaluate protective stops without waiting for a candle to
// close.
func LastPrice(symbol string) (float64, time.Time, error) {
	ticker := BinanceSymbol(symbol)
	if ticker == "" {
		return 0, time.Time{}, fmt.Errorf("Binance 数据源需要交易对代码，例如 BTCUSDT")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	query := url.Values{}
	query.Set("symbol", ticker)
	query.Set("interval", "1d")
	query.Set("limit", "2")
	request, err := http.NewRequest(http.MethodGet, binanceEndpoint+"/api/v3/klines?"+query.Encode(), nil)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("构造 Binance 请求失败: %w", err)
	}
	response, err := doGetWithRetry(client, request, ticker)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("请求 Binance 最新价失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, time.Time{}, fmt.Errorf("Binance 返回 HTTP 状态码 %d（%s）",
			response.StatusCode, binanceErrorDetail(response))
	}
	var rows [][]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		return 0, time.Time{}, fmt.Errorf("解析 Binance 最新价失败: %w", err)
	}
	if len(rows) == 0 || len(rows[len(rows)-1]) < 5 {
		return 0, time.Time{}, fmt.Errorf("Binance 没有返回 %s 的K线", ticker)
	}
	last := rows[len(rows)-1]
	openTime, err := rawInt64(last[0])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("Binance 最新K线的时间: %w", err)
	}
	price, err := rawFloat(last[4])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("Binance 最新K线的价格: %w", err)
	}
	return price, time.UnixMilli(openTime).UTC(), nil
}

// binanceKlines fetches one page of daily candles in [start, end].
func binanceKlines(client *http.Client, ticker string, start, end time.Time) ([]binanceKline, error) {
	query := url.Values{}
	query.Set("symbol", ticker)
	query.Set("interval", "1d")
	query.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	query.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
	query.Set("limit", strconv.Itoa(binanceMaxLimit))

	request, err := http.NewRequest(http.MethodGet, binanceEndpoint+"/api/v3/klines?"+query.Encode(), nil)
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

// binanceRequest performs one page request, retrying transient failures with
// an exponential backoff (1s/2s/4s before attempts 2/3). A 4xx is a real
// answer from the exchange (unknown symbol, bad range) and is returned
// immediately; 5xx, network errors and undecodable bodies are retried.
func binanceRequest(client *http.Client, request *http.Request, ticker string) ([][]json.RawMessage, error) {
	var lastErr error
	for attempt := 1; attempt <= binanceAttempts; attempt++ {
		if attempt > 1 {
			binanceSleep(binanceBackoff(attempt))
		}

		response, err := client.Do(request)
		if err != nil {
			lastErr = fmt.Errorf("从 Binance 下载 %s 失败: %w", ticker, err)
			continue
		}

		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("Binance 返回 %s 的 HTTP 状态码 %d%s",
				ticker, response.StatusCode, binanceErrorDetail(response))
			response.Body.Close()
			continue
		}
		if response.StatusCode != http.StatusOK {
			detail := binanceErrorDetail(response)
			response.Body.Close()
			return nil, fmt.Errorf("Binance 返回 %s 的 HTTP 状态码 %d%s", ticker, response.StatusCode, detail)
		}

		// Every field arrives as either a number or a string, so the rows are
		// decoded raw and converted one by one.
		var rows [][]json.RawMessage
		decodeErr := json.NewDecoder(response.Body).Decode(&rows)
		response.Body.Close()
		if decodeErr != nil {
			lastErr = fmt.Errorf("解析 Binance 返回内容失败: %w", decodeErr)
			continue
		}
		return rows, nil
	}
	return nil, fmt.Errorf("%w（已重试 %d 次）", lastErr, binanceAttempts)
}

// binanceErrorDetail extracts the human-readable message Binance puts in the
// body of a failed request, e.g. {"code":-1121,"msg":"Invalid symbol."}.
func binanceErrorDetail(response *http.Response) string {
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	// The body is consumed here, so cap it: an error page should be small.
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Msg == "" {
		return ""
	}
	return fmt.Sprintf("（%s）", payload.Msg)
}

// rawNumber reads a JSON value that may be encoded as either a number or a
// string: Binance sends timestamps as numbers and prices as strings.
func rawNumber(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", fmt.Errorf("字段既不是字符串也不是数字: %s", string(raw))
	}
	return number.String(), nil
}

func rawFloat(raw json.RawMessage) (float64, error) {
	text, err := rawNumber(raw)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("无法把 %q 解析为数字", text)
	}
	return value, nil
}

func rawInt64(raw json.RawMessage) (int64, error) {
	text, err := rawNumber(raw)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法把 %q 解析为整数", text)
	}
	return value, nil
}
