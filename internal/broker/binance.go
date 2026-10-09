// BinanceBroker places real spot orders on Binance. Spot accounts cannot
// short: the engine only ever sends Buy (open/close) — and Sell when closing
// a long — and the strategy's short signal is simply not tradable here.
//
// The client is deliberately small: HMAC-SHA256 signed REST calls, exchange
// quantity filters, idempotent client order ids, and optional dry-run mode
// (no network, fills simulated at the reference price) so the trade loop can
// be exercised without keys.
package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BinanceConfig configures a live broker.
type BinanceConfig struct {
	BaseURL     string // default https://api.binance.com
	APIKey      string // from the environment, never from config files
	SecretKey   string
	Symbol      string // e.g. BTCUSDT
	DryRun      bool   // no network; fills simulated locally
	Timeout     time.Duration
	StepSize    float64 // 0 = fetch LOT_SIZE filter on Init
	MinNotional float64 // 0 = fetch from exchange filters on Init
	MaxPriceDev float64 // reject fills deviating more than this fraction from the reference price
	JournalPath string
}

// BinanceBroker implements Broker against Binance spot.
type BinanceBroker struct {
	cfg     BinanceConfig
	client  *http.Client
	Trades  []Fill
	StepSz  float64
	usedIDs map[string]bool
	// clock corrects the host clock against the exchange's, so a machine with
	// a drifting clock can still sign valid requests.
	clock exchangeClock
	// BaseAsset and QuoteAsset come from exchangeInfo and are set by Init.
	// Balances uses them to identify the pair in the account, so any symbol
	// shape (LINKUSD, DOGUSDC, ...) is handled without string guessing.
	BaseAsset  string
	QuoteAsset string
}

// NewBinance builds a broker. In DryRun no network is touched and no keys are
// needed. Otherwise APIKey/SecretKey must be set.
func NewBinance(cfg BinanceConfig) *BinanceBroker {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.binance.com"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &BinanceBroker{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}, StepSz: cfg.StepSize, usedIDs: map[string]bool{}}
}

// Fills implements Broker.
func (b *BinanceBroker) Fills() []Fill { return b.Trades }

// IsDryRun reports whether orders are simulated locally.
func (b *BinanceBroker) IsDryRun() bool { return b.cfg.DryRun }

// StepSize returns the effective quantity step.
func (b *BinanceBroker) StepSize() float64 { return b.StepSz }

// Init fetches the exchange quantity filters so order sizes respect the
// symbol's step size. Skipped in dry-run when a StepSize was supplied. It
// also records the authoritative base/quote asset names, which Balances
// uses to read the account.
func (b *BinanceBroker) Init() error {
	if b.cfg.DryRun && b.StepSz > 0 {
		return nil
	}
	// Align with the exchange before the first signed call. Init's own
	// leverage/filter setup is the first thing that needs a valid timestamp,
	// so a drifting host clock would otherwise fail here with -1021.
	b.trySyncClock()
	symbolRaw, err := b.exchangeInfo(b.cfg.Symbol)
	if err != nil {
		return err
	}
	if err := applyFilters(b, symbolRaw); err != nil {
		return err
	}
	if b.BaseAsset == "" || b.QuoteAsset == "" {
		return fmt.Errorf("exchangeInfo 没有 %s 的 baseAsset/quoteAsset", b.cfg.Symbol)
	}
	return nil
}

// syncClock measures the offset between the host clock and the exchange's.
func (b *BinanceBroker) syncClock() error {
	return b.clock.measure(b.client, b.cfg.BaseURL+"/api/v3/time")
}

// trySyncClock aligns the clock, tolerating a failure. A host whose clock is
// already correct must not be stopped from trading because /api/v3/time was
// unreachable; if the clock really is wrong, the signed path answers the
// exchange's -1021 by syncing again and reporting the failure then.
func (b *BinanceBroker) trySyncClock() {
	_ = b.syncClock()
}

// Balances fetches the account balances for the trading pair, used to
// reconcile local state before resuming. Returns base and quote amounts.
// It requires Init to have run first so the base/quote asset names are set.
func (b *BinanceBroker) Balances() (base, quote float64, err error) {
	base, quote, _, _, err = b.accountBalances()
	return
}

func (b *BinanceBroker) AvailableBalances() (base, quote float64, err error) {
	_, _, base, quote, err = b.accountBalances()
	return
}

func (b *BinanceBroker) accountBalances() (base, quote, freeBase, freeQuote float64, err error) {
	if b.BaseAsset == "" || b.QuoteAsset == "" {
		return 0, 0, 0, 0, fmt.Errorf("Balances 需要先 Init 获取交易对资产名")
	}
	body, err := b.getSigned("/api/v3/account", url.Values{})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var account struct {
		Balances []struct {
			Asset  string      `json:"asset"`
			Free   json.Number `json:"free"`
			Locked json.Number `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &account); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("解析账户余额失败: %w", err)
	}
	for _, bal := range account.Balances {
		asset := strings.ToUpper(bal.Asset)
		total, _ := bal.Free.Float64()
		locked, _ := bal.Locked.Float64()
		switch asset {
		case strings.ToUpper(b.BaseAsset):
			base = total + locked
			freeBase = total
		case strings.ToUpper(b.QuoteAsset):
			quote = total + locked
			freeQuote = total
		}
	}
	return base, quote, freeBase, freeQuote, nil
}

// MarketOrder places (or simulates) one market order.
func (b *BinanceBroker) MarketOrder(ts time.Time, symbol string, side Side, quantity, price float64, reason string) Fill {
	quantity = b.roundQuantity(quantity)
	if quantity <= 0 {
		return b.rejected(ts, symbol, side, price, "数量为 0，拒单", reason)
	}
	if b.cfg.MinNotional > 0 && quantity*price < b.cfg.MinNotional {
		return b.rejected(ts, symbol, side, price,
			fmt.Sprintf("名义 %.2f 低于最小 %.2f", quantity*price, b.cfg.MinNotional), reason)
	}

	if b.cfg.DryRun {
		return b.dryFill(ts, symbol, side, quantity, price)
	}

	clientID := b.nextOrderID(symbol, side)
	if err := recordIntent(b.cfg.JournalPath, clientID, symbol, side, quantity); err != nil {
		return b.rejected(ts, symbol, side, price, "下单前日志写入失败: "+err.Error(), reason)
	}
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(symbol))
	query.Set("side", strings.ToUpper(string(side)))
	query.Set("type", "MARKET")
	query.Set("quantity", formatQty(quantity))
	query.Set("newClientOrderId", clientID)
	query.Set("newOrderRespType", "FULL")
	body, err := b.postSigned("/api/v3/order", query)
	if err != nil {
		// A timeout is not a rejection. Query the same ID before giving control
		// back to the engine, which will halt if the result remains unknown.
		body, err = b.getSigned("/api/v3/order", orderQuery(symbol, clientID))
		if err != nil {
			f := orderUnknown(ts, symbol, side, price, clientID, err.Error())
			b.Trades = append(b.Trades, f)
			return f
		}
	}
	var order exchangeOrder
	if err := json.Unmarshal(body, &order); err != nil {
		f := orderUnknown(ts, symbol, side, price, clientID, "订单响应无法解析")
		b.Trades = append(b.Trades, f)
		return f
	}
	detail := order.detail()
	execs, avg := detail.executed, detail.avgPrice
	if (execs <= 0 || avg <= 0) && order.OrderID > 0 {
		// The ack can report zeros before the fill lands: poll once.
		if got, err := b.order(order.OrderID); err == nil {
			execs, avg, order.Status = got.executed, got.avgPrice, got.status
		}
	}
	if execs <= 0 || avg <= 0 {
		if terminalOrder(order.Status) && execs == 0 {
			return b.rejected(ts, symbol, side, price, "订单未成交 (status="+order.Status+")", reason)
		}
		f := orderUnknown(ts, symbol, side, price, clientID, "status="+order.Status)
		b.Trades = append(b.Trades, f)
		return f
	}
	commission, baseFee, feeAssets := order.fees(b.BaseAsset, b.QuoteAsset)
	feesKnown := len(order.Fills) > 0
	if !feesKnown && order.OrderID > 0 {
		if fills, err := b.tradeFills(order.OrderID); err == nil && len(fills) > 0 {
			order.Fills = fills
			commission, baseFee, feeAssets = order.fees(b.BaseAsset, b.QuoteAsset)
			feesKnown = true
		}
	}
	fill := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: execs,
		Price: avg, Commission: commission, Notional: execs * avg,
		Reason: reason, OrderID: strconv.FormatInt(order.OrderID, 10),
		ClientOrderID: clientID, Status: "filled", BaseCommission: baseFee, CommissionAssets: feeAssets,
	}
	if execs < quantity-1e-10 {
		fill.Status = "partial"
	}
	if !terminalOrder(order.Status) {
		fill.Uncertain = true
	}
	if !feesKnown {
		fill.Uncertain = true
		fill.Reason += "；手续费明细尚未确认"
	}
	for asset, fee := range feeAssets {
		if fee > 0 && asset != b.BaseAsset && asset != b.QuoteAsset {
			fill.Uncertain = true
			fill.Reason += "；" + asset + " 手续费需要核对"
		}
	}
	// An adverse fill already happened: book it rather than pretending it was
	// rejected. The engine stops new orders until the operator reconciles it.
	if b.cfg.MaxPriceDev > 0 && price > 0 && math.Abs(avg/price-1) > b.cfg.MaxPriceDev {
		fill.Uncertain = true
		fill.Reason += "；成交偏离参考价，暂停并核对"
	}
	b.Trades = append(b.Trades, fill)
	return fill
}

// dryFill simulates a fill at the reference price with a 5 bp haircut,
// recording it like a real fill so the engine path is identical.
func (b *BinanceBroker) dryFill(ts time.Time, symbol string, side Side, quantity, price float64) Fill {
	const slip = 5 / 10_000.0
	fillPrice := price * (1 + slip)
	if side == Sell {
		fillPrice = price * (1 - slip)
	}
	notional := quantity * fillPrice
	f := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: quantity,
		Price: fillPrice, Notional: notional,
		Commission: notional * 0.001,
		Reason:     "dry-run",
		OrderID:    "dry-" + strconv.FormatInt(ts.UnixNano(), 10),
	}
	b.Trades = append(b.Trades, f)
	return f
}

// orderDetail is the subset of a Binance order we need to confirm a fill.
type orderDetail struct {
	executed   float64
	avgPrice   float64
	commission float64
	status     string
}

func (b *BinanceBroker) order(id int64) (orderDetail, error) {
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	query.Set("orderId", strconv.FormatInt(id, 10))
	raw, err := b.getSigned("/api/v3/order", query)
	if err != nil {
		return orderDetail{}, err
	}
	var o exchangeOrder
	if err := json.Unmarshal(raw, &o); err != nil {
		return orderDetail{}, err
	}
	return o.detail(), nil
}

func (b *BinanceBroker) tradeFills(id int64) ([]exchangeExecution, error) {
	raw, err := b.getSigned("/api/v3/myTrades", url.Values{"symbol": {b.cfg.Symbol}, "orderId": {strconv.FormatInt(id, 10)}})
	if err != nil {
		return nil, err
	}
	var trades []exchangeExecution
	if err := json.Unmarshal(raw, &trades); err != nil {
		return nil, err
	}
	return trades, nil
}

// roundQuantity floors to the exchange step size.
func (b *BinanceBroker) roundQuantity(qty float64) float64 {
	step := b.StepSz
	if step <= 0 {
		step = 1e-9
	}
	return math.Floor(qty/step+1e-9) * step
}

// nextOrderID builds an idempotent client order id and remembers it.
func (b *BinanceBroker) nextOrderID(symbol string, side Side) string {
	return newClientID("ta-" + string(side))
}

func (b *BinanceBroker) rejected(ts time.Time, symbol string, side Side, price float64, message, reason string) Fill {
	full := message
	if reason != "" {
		full = fmt.Sprintf("%s (%s)", message, reason)
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		price = 0
	}
	f := Fill{Time: ts, Symbol: symbol, Side: side, Price: price, Reason: full, Rejected: true, Status: "rejected"}
	b.Trades = append(b.Trades, f)
	return f
}

// exchangeInfo returns the exchangeInfo payload for one symbol.
func (b *BinanceBroker) exchangeInfo(symbol string) (json.RawMessage, error) {
	body, err := b.getPublic("/api/v3/exchangeInfo?symbol=" + url.QueryEscape(strings.ToUpper(symbol)))
	if err != nil {
		return nil, err
	}
	var info struct {
		Symbols []json.RawMessage `json:"symbols"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析 exchangeInfo 失败: %w", err)
	}
	if len(info.Symbols) == 0 {
		return nil, fmt.Errorf("exchangeInfo 没有 %s 的信息", symbol)
	}
	return info.Symbols[0], nil
}

// applyFilters pulls the LOT_SIZE step size, the min notional, and the
// authoritative base/quote asset names from one exchangeInfo symbol block.
func applyFilters(b *BinanceBroker, raw json.RawMessage) error {
	var symbol struct {
		BaseAsset  string `json:"baseAsset"`
		QuoteAsset string `json:"quoteAsset"`
		Filters    []struct {
			Filter      string `json:"filterType"`
			StepSize    string `json:"stepSize"`
			MinQty      string `json:"minQty"`
			MinNotional string `json:"minNotional"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(raw, &symbol); err != nil {
		return fmt.Errorf("解析交易对过滤器失败: %w", err)
	}
	b.BaseAsset, b.QuoteAsset = symbol.BaseAsset, symbol.QuoteAsset
	for _, f := range symbol.Filters {
		switch f.Filter {
		case "LOT_SIZE":
			if step, err := strconv.ParseFloat(f.StepSize, 64); err == nil && step > 0 {
				b.StepSz = step
			}
		case "NOTIONAL":
			if min, err := strconv.ParseFloat(f.MinNotional, 64); err == nil && min > 0 {
				b.cfg.MinNotional = min
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- HTTP

// getPublic performs a public (unauthenticated) GET.
func (b *BinanceBroker) getPublic(path string) ([]byte, error) {
	u := b.cfg.BaseURL + path
	response, err := b.do(http.MethodGet, u, nil, false)
	if err != nil {
		return nil, err
	}
	body, err := b.readBody(response)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// getSigned performs a signed read-only request (account, order status).
// These endpoints require both the API key header and an HMAC signature; a
// key header without a signature is a 400 from the exchange.
func (b *BinanceBroker) getSigned(path string, query url.Values) ([]byte, error) {
	return b.signedRequest(http.MethodGet, path, query)
}

// postSigned signs and posts a trading request.
func (b *BinanceBroker) postSigned(path string, query url.Values) ([]byte, error) {
	return b.signedRequest(http.MethodPost, path, query)
}

// signedRequest signs and sends one private request, re-syncing the clock and
// retrying once when the exchange rejects the timestamp.
func (b *BinanceBroker) signedRequest(method, path string, query url.Values) ([]byte, error) {
	return signAndRetry(b.syncClock, func() ([]byte, error) {
		return b.signedOnce(method, path, query)
	})
}

// signedOnce performs a single signed attempt with the current clock offset.
func (b *BinanceBroker) signedOnce(method, path string, query url.Values) ([]byte, error) {
	signed := signedQuery(b.cfg.SecretKey, b.clock.timestamp(), query)
	response, err := b.do(method, b.cfg.BaseURL+path+"?"+signed, &b.cfg.APIKey, false)
	if err != nil {
		return nil, err
	}
	return b.readBody(response)
}

// do sends one request, attaching the API key header when provided.
func (b *BinanceBroker) do(method, u string, apiKey *string, _ bool) (*http.Response, error) {
	request, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != nil {
		request.Header.Set("X-MBX-APIKEY", *apiKey)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Binance 失败: %w", err)
	}
	return response, nil
}

// readBody drains a response and maps non-200s to the exchange error message.
func (b *BinanceBroker) readBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, httpError(response.StatusCode, body)
	}
	return body, nil
}

// sign produces the HMAC-SHA256 signature for a query string.
func sign(secret, query string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// binanceError is a non-200 response from the exchange. The code is kept as a
// field, not just text, so the timestamp failure (-1021) can be recognised and
// answered with a clock re-sync instead of a guess at the message wording.
type binanceError struct {
	Status int
	Code   int32
	Msg    string
}

func (e *binanceError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("Binance HTTP %d (code %d): %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("Binance HTTP %d", e.Status)
}

// isTimestampError reports whether the exchange rejected the request because
// its timestamp fell outside the accepted window.
func isTimestampError(err error) bool {
	var exchangeErr *binanceError
	if errors.As(err, &exchangeErr) {
		return exchangeErr.Code == -1021
	}
	return false
}

// httpError extracts the exchange message from a failed response.
func httpError(status int, body []byte) error {
	var payload struct {
		Code int32  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &payload)
	return &binanceError{Status: status, Code: payload.Code, Msg: payload.Msg}
}

func formatQty(qty float64) string {
	text := strconv.FormatFloat(qty, 'f', 9, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimSuffix(text, ".")
	if text == "" {
		text = "0"
	}
	return text
}
