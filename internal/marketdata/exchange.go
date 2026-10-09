package marketdata

// The Binance primitives every loader shares: ticker normalization, the K-line
// row shape, and the retrying GET.
//
// These used to live in binance.go, which was the spot loader. The spot venue
// is retired — the terminal, the live runner and the backtests are all
// USDT-margined perpetuals now — but the futures loader still depends on all
// of this, so the pieces were lifted out rather than deleted with their old
// home.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rdone44/trading-agent-go/internal/model"
)

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
