// FuturesBroker places orders on Binance USDT-margined perpetual futures.
// Unlike spot it can hold a signed position (long or short), so both entry
// directions the strategy emits are tradable, and a position is sized in
// base-coin quantity with leverage applied to the margin budget.
//
// The client is deliberately small: HMAC-SHA256 signed REST calls against
// fapi.binance.com, leverage and margin-type setup, position/balance
// reconciliation, and an optional dry-run mode that simulates fills locally
// so the trade loop can be exercised without keys.
package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/huijun/trading-agent-go/internal/marketdata"
)

// FuturesConfig configures a live futures broker.
type FuturesConfig struct {
	BaseURL     string // default https://fapi.binance.com
	APIKey      string // from the environment, never from config files
	SecretKey   string
	Symbol      string // e.g. BTCUSDT perpetual
	DryRun      bool   // no network; fills simulated locally
	Timeout     time.Duration
	Leverage    int    // 1 = isolated spot-like, up to the exchange max
	MarginMode  string // "ISOLATED" (default) or "CROSS"
	StepSize    float64
	MaxPriceDev float64 // reject fills deviating more than this fraction
}

// FuturesBroker implements Broker against Binance USDT-margined perpetuals.
type FuturesBroker struct {
	cfg     FuturesConfig
	http    *http.Client
	Trades  []Fill
	StepSz  float64
	usedIDs map[string]bool
	// Resolved by Init from the exchange contract info.
	QuantityPrecision int
}

// NewFutures builds a futures broker. In DryRun no network is touched and no
// keys are needed. Otherwise APIKey/SecretKey must be set and leverage >= 1.
func NewFutures(cfg FuturesConfig) *FuturesBroker {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://fapi.binance.com"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Leverage < 1 {
		cfg.Leverage = 1
	}
	if cfg.MarginMode == "" {
		cfg.MarginMode = "ISOLATED"
	}
	return &FuturesBroker{
		cfg:     cfg,
		http:    &http.Client{Timeout: cfg.Timeout},
		StepSz:  cfg.StepSize,
		usedIDs: map[string]bool{},
	}
}

// Fills implements Broker.
func (b *FuturesBroker) Fills() []Fill { return b.Trades }

// IsDryRun reports whether orders are simulated locally.
func (b *FuturesBroker) IsDryRun() bool { return b.cfg.DryRun }

// Leverage returns the configured leverage.
func (b *FuturesBroker) Leverage() int { return b.cfg.Leverage }

// MarginMode returns the configured margin mode.
func (b *FuturesBroker) MarginMode() string { return b.cfg.MarginMode }

// Init sets the symbol's leverage and margin type, then fetches the contract
// filters. A margin-type change the exchange rejects as a no-op (-4045) is
// ignored so a second start is idempotent.
func (b *FuturesBroker) Init() error {
	if b.cfg.DryRun && b.StepSz > 0 {
		return nil
	}
	if err := b.setLeverage(); err != nil {
		return err
	}
	if err := b.setMarginType(); err != nil {
		return err
	}
	contract, err := b.contractInfo()
	if err != nil {
		return err
	}
	return b.applyContract(contract)
}

func (b *FuturesBroker) setLeverage() error {
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	query.Set("leverage", strconv.Itoa(b.cfg.Leverage))
	if _, err := b.postSigned("/fapi/v1/leverage", query); err != nil {
		return fmt.Errorf("设置杠杆失败: %w", err)
	}
	return nil
}

func (b *FuturesBroker) setMarginType() error {
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	query.Set("marginType", b.cfg.MarginMode)
	_, err := b.postSigned("/fapi/v1/marginType", query)
	if err == nil {
		return nil
	}
	// -4045 means the margin type is already what we asked for.
	if strings.Contains(err.Error(), "-4045") {
		return nil
	}
	return fmt.Errorf("设置保证金模式失败: %w", err)
}

// OpenPosition reconciles the exchange's signed position against the local
// book: it returns the side, quantity (absolute) and entry price of the open
// position, or the zero value when flat.
func (b *FuturesBroker) OpenPosition() (side Side, quantity, entry float64, err error) {
	body, err := b.getSigned("/fapi/v2/positionRisk", url.Values{})
	if err != nil {
		return "", 0, 0, err
	}
	var rows []struct {
		Symbol      string      `json:"symbol"`
		PositionAmt json.Number `json:"positionAmt"`
		EntryPrice  json.Number `json:"entryPrice"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return "", 0, 0, fmt.Errorf("解析持仓失败: %w", err)
	}
	want := strings.ToUpper(b.cfg.Symbol)
	for _, row := range rows {
		if strings.ToUpper(row.Symbol) != want {
			continue
		}
		amt, _ := row.PositionAmt.Float64()
		entr, _ := row.EntryPrice.Float64()
		switch {
		case amt > 0:
			return Buy, amt, entr, nil
		case amt < 0:
			return Sell, -amt, entr, nil
		}
	}
	return "", 0, 0, nil
}

// USDTBalance returns the available USDT balance on the futures account, the
// figure the risk manager should treat as its trading capital.
func (b *FuturesBroker) USDTBalance() (float64, error) {
	body, err := b.getSigned("/fapi/v2/balance", url.Values{})
	if err != nil {
		return 0, err
	}
	var rows []struct {
		Asset            string      `json:"asset"`
		AvailableBalance json.Number `json:"availableBalance"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, fmt.Errorf("解析余额失败: %w", err)
	}
	for _, row := range rows {
		if strings.ToUpper(row.Asset) == "USDT" {
			v, _ := row.AvailableBalance.Float64()
			return v, nil
		}
	}
	return 0, nil
}

// MarketOrder places (or simulates) one futures market order. Quantity is in
// base-coin units; a Sell entry opens or adds a short.
func (b *FuturesBroker) MarketOrder(ts time.Time, symbol string, side Side, quantity, price float64, reason string) Fill {
	quantity = b.roundQuantity(quantity)
	if quantity <= 0 {
		return b.rejected(ts, symbol, side, price, "数量为 0，拒单", reason)
	}

	if b.cfg.DryRun {
		return b.dryFill(ts, symbol, side, quantity, price)
	}

	clientID := b.nextOrderID(symbol, side)
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(symbol))
	query.Set("side", strings.ToUpper(string(side)))
	query.Set("type", "MARKET")
	query.Set("quantity", formatQtyPrec(quantity, b.QuantityPrecision))
	query.Set("newClientOrderId", clientID)
	body, err := b.postSigned("/fapi/v1/order", query)
	if err != nil {
		return b.rejected(ts, symbol, side, price, "下单失败: "+err.Error(), reason)
	}
	var order struct {
		OrderID    int64       `json:"orderId"`
		Executed   json.Number `json:"executedQty"`
		AvgPrice   json.Number `json:"avgPrice"`
		Commission json.Number `json:"commission"`
		Status     string      `json:"status"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		return b.rejected(ts, symbol, side, price, "解析订单失败: "+err.Error(), reason)
	}
	execs, _ := order.Executed.Float64()
	avg, _ := order.AvgPrice.Float64()
	commission, _ := order.Commission.Float64()
	if (execs <= 0 || avg <= 0) && order.OrderID > 0 {
		if got, err := b.order(order.OrderID); err == nil {
			execs, avg, commission = got.executed, got.avgPrice, got.commission
		}
	}
	if execs <= 0 || avg <= 0 {
		return b.rejected(ts, symbol, side, price, "订单未成交 (status="+order.Status+")", reason)
	}
	if b.cfg.MaxPriceDev > 0 && price > 0 {
		if dev := math.Abs(avg/price - 1); dev > b.cfg.MaxPriceDev {
			return b.rejected(ts, symbol, side, avg,
				fmt.Sprintf("成交价偏离参考价 %.1f%%，超过上限 %.1f%%", dev*100, b.cfg.MaxPriceDev*100), reason)
		}
	}
	fill := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: execs,
		Price: avg, Commission: commission, Notional: execs * avg,
		Reason: reason, OrderID: strconv.FormatInt(order.OrderID, 10),
	}
	b.Trades = append(b.Trades, fill)
	return fill
}

func (b *FuturesBroker) dryFill(ts time.Time, symbol string, side Side, quantity, price float64) Fill {
	const slip = 5 / 10_000.0
	fillPrice := price * (1 + slip)
	if side == Sell {
		fillPrice = price * (1 - slip)
	}
	notional := quantity * fillPrice
	f := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: quantity,
		Price: fillPrice, Notional: notional,
		Commission: notional * 0.0004, // futures taker fee, ~0.04%
		Reason:     "dry-run",
		OrderID:    "fdry-" + strconv.FormatInt(ts.UnixNano(), 10),
	}
	b.Trades = append(b.Trades, f)
	return f
}

func (b *FuturesBroker) order(id int64) (orderDetail, error) {
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	query.Set("orderId", strconv.FormatInt(id, 10))
	raw, err := b.getSigned("/fapi/v1/order", query)
	if err != nil {
		return orderDetail{}, err
	}
	var o struct {
		Executed   json.Number `json:"executedQty"`
		AvgPrice   json.Number `json:"avgPrice"`
		Commission json.Number `json:"commission"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return orderDetail{}, err
	}
	out := orderDetail{}
	out.executed, _ = o.Executed.Float64()
	out.avgPrice, _ = o.AvgPrice.Float64()
	out.commission, _ = o.Commission.Float64()
	return out, nil
}

func (b *FuturesBroker) roundQuantity(qty float64) float64 {
	step := b.StepSz
	if step <= 0 {
		step = 1e-9
	}
	return math.Floor(qty/step+1e-9) * step
}

func (b *FuturesBroker) nextOrderID(symbol string, side Side) string {
	n := 1
	for {
		id := fmt.Sprintf("taf-%s-%s-%d-%d",
			strings.ToLower(string(side)), marketdata.BinanceSymbol(symbol), time.Now().Unix(), n)
		if !b.usedIDs[id] {
			b.usedIDs[id] = true
			return id
		}
		n++
	}
}

func (b *FuturesBroker) rejected(ts time.Time, symbol string, side Side, price float64, message, reason string) Fill {
	full := message
	if reason != "" {
		full = fmt.Sprintf("%s (%s)", message, reason)
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		price = 0
	}
	f := Fill{Time: ts, Symbol: symbol, Side: side, Price: price, Reason: full, Rejected: true}
	b.Trades = append(b.Trades, f)
	return f
}

// contractInfo returns the exchangeInfo contract block for the symbol.
func (b *FuturesBroker) contractInfo() (json.RawMessage, error) {
	body, err := b.getPublic("/fapi/v1/exchangeInfo?symbol=" + url.QueryEscape(strings.ToUpper(b.cfg.Symbol)))
	if err != nil {
		return nil, err
	}
	var info struct {
		Symbols []json.RawMessage `json:"symbols"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析 futures exchangeInfo 失败: %w", err)
	}
	if len(info.Symbols) == 0 {
		return nil, fmt.Errorf("exchangeInfo 没有 %s 的合约信息", b.cfg.Symbol)
	}
	return info.Symbols[0], nil
}

// applyContract pulls the LOT_SIZE step size and the quantity precision from
// the contract block.
func (b *FuturesBroker) applyContract(raw json.RawMessage) error {
	var c struct {
		QuantityPrecision int `json:"quantityPrecision"`
		Filters           []struct {
			Filter   string `json:"filterType"`
			StepSize string `json:"stepSize"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("解析合约过滤器失败: %w", err)
	}
	b.QuantityPrecision = c.QuantityPrecision
	for _, f := range c.Filters {
		if f.Filter == "LOT_SIZE" {
			if step, err := strconv.ParseFloat(f.StepSize, 64); err == nil && step > 0 {
				b.StepSz = step
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- HTTP

// getPublic performs an unauthenticated GET against the futures base.
func (b *FuturesBroker) getPublic(path string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, b.cfg.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := b.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Binance futures 失败: %w", err)
	}
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

// getSigned performs a signed read-only futures request.
func (b *FuturesBroker) getSigned(path string, query url.Values) ([]byte, error) {
	if err := signQuery(b.cfg.SecretKey, query); err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodGet,
		b.cfg.BaseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-MBX-APIKEY", b.cfg.APIKey)
	response, err := b.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Binance futures 失败: %w", err)
	}
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

// postSigned signs and posts a futures trading request.
func (b *FuturesBroker) postSigned(path string, query url.Values) ([]byte, error) {
	if err := signQuery(b.cfg.SecretKey, query); err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost,
		b.cfg.BaseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-MBX-APIKEY", b.cfg.APIKey)
	response, err := b.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Binance futures 失败: %w", err)
	}
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

// formatQtyPrec renders a quantity with a fixed number of decimal places,
// dropping the decimal point when the precision is zero.
func formatQtyPrec(qty float64, prec int) string {
	if prec <= 0 {
		return strconv.FormatInt(int64(math.Round(qty)), 10)
	}
	return strconv.FormatFloat(qty, 'f', prec, 64)
}
