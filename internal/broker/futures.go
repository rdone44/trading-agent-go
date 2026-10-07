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

	"github.com/rdone44/trading-agent-go/internal/marketdata"
)

// FuturesConfig configures a live futures broker.
type FuturesConfig struct {
	BaseURL       string // default https://fapi.binance.com
	APIKey        string // from the environment, never from config files
	SecretKey     string
	Symbol        string // e.g. BTCUSDT perpetual
	DryRun        bool   // no network; fills simulated locally
	Timeout       time.Duration
	Leverage      int    // 1 = isolated spot-like, up to the exchange max
	MarginMode    string // "ISOLATED" (default) or "CROSS"
	StepSize      float64
	MaxPriceDev   float64 // reject fills deviating more than this fraction
	CommissionBps *float64
	SlippageBps   *float64
	JournalPath   string
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
	if strings.Contains(err.Error(), "-4046") {
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
		Symbol       string      `json:"symbol"`
		PositionAmt  json.Number `json:"positionAmt"`
		EntryPrice   json.Number `json:"entryPrice"`
		PositionSide string      `json:"positionSide"`
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
		if row.PositionSide != "" && row.PositionSide != "BOTH" {
			return "", 0, 0, fmt.Errorf("当前仅支持单向持仓模式，不支持 Hedge Mode")
		}
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
		Asset         string      `json:"asset"`
		WalletBalance json.Number `json:"balance"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, fmt.Errorf("解析余额失败: %w", err)
	}
	for _, row := range rows {
		if strings.ToUpper(row.Asset) == "USDT" {
			v, _ := row.WalletBalance.Float64()
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
	if err := recordIntent(b.cfg.JournalPath, clientID, symbol, side, quantity); err != nil {
		return b.rejected(ts, symbol, side, price, "下单前日志写入失败: "+err.Error(), reason)
	}
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(symbol))
	query.Set("side", strings.ToUpper(string(side)))
	query.Set("type", "MARKET")
	query.Set("quantity", formatQtyPrec(quantity, b.QuantityPrecision))
	query.Set("newClientOrderId", clientID)
	query.Set("newOrderRespType", "RESULT")
	if reason != "entry_long" && reason != "entry_short" {
		query.Set("reduceOnly", "true")
	}
	body, err := b.postSigned("/fapi/v1/order", query)
	if err != nil {
		body, err = b.getSigned("/fapi/v1/order", orderQuery(symbol, clientID))
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
	commission, feeErr := b.tradeFees(order.OrderID)
	fill := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: execs,
		Price: avg, Commission: commission, Notional: execs * avg,
		Reason: reason, OrderID: strconv.FormatInt(order.OrderID, 10),
		ClientOrderID: clientID, Status: "filled", Uncertain: feeErr != nil || !terminalOrder(order.Status),
	}
	if execs < quantity-1e-10 {
		fill.Status = "partial"
	}
	if feeErr != nil {
		fill.Reason += "；手续费待核对"
	}
	if b.cfg.MaxPriceDev > 0 && price > 0 && math.Abs(avg/price-1) > b.cfg.MaxPriceDev {
		fill.Uncertain = true
		fill.Reason += "；成交价偏离，暂停并核对"
	}
	b.Trades = append(b.Trades, fill)
	return fill
}

func (b *FuturesBroker) dryFill(ts time.Time, symbol string, side Side, quantity, price float64) Fill {
	slip := 5 / 10_000.0
	if b.cfg.SlippageBps != nil {
		slip = *b.cfg.SlippageBps / 10000
	}
	fillPrice := price * (1 + slip)
	if side == Sell {
		fillPrice = price * (1 - slip)
	}
	notional := quantity * fillPrice
	f := Fill{
		Time: ts, Symbol: symbol, Side: side, Quantity: quantity,
		Price: fillPrice, Notional: notional,
		Commission: notional * 0.0004,
		Reason:     "dry-run",
		OrderID:    "fdry-" + strconv.FormatInt(ts.UnixNano(), 10),
	}
	if b.cfg.CommissionBps != nil {
		f.Commission = notional * *b.cfg.CommissionBps / 10000
	}
	f.Status = "filled"
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
	var o exchangeOrder
	if err := json.Unmarshal(raw, &o); err != nil {
		return orderDetail{}, err
	}
	return o.detail(), nil
}

func (b *FuturesBroker) tradeFees(id int64) (float64, error) {
	raw, err := b.getSigned("/fapi/v1/userTrades", url.Values{"symbol": {b.cfg.Symbol}, "orderId": {strconv.FormatInt(id, 10)}})
	if err != nil {
		return 0, err
	}
	var trades []struct {
		Commission json.Number `json:"commission"`
		Asset      string      `json:"commissionAsset"`
	}
	if err := json.Unmarshal(raw, &trades); err != nil {
		return 0, err
	}
	if len(trades) == 0 {
		return 0, fmt.Errorf("没有成交明细")
	}
	var fee float64
	for _, t := range trades {
		if t.Asset != "USDT" && positive(t.Commission) > 0 {
			return fee, fmt.Errorf("非 USDT 手续费需要核对")
		}
		fee += positive(t.Commission)
	}
	return fee, nil
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
		id := newClientID("taf-" + strings.ToLower(string(side)) + "-" + marketdata.BinanceSymbol(symbol))
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
	f := Fill{Time: ts, Symbol: symbol, Side: side, Price: price, Reason: full, Rejected: true, Status: "rejected"}
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
	for _, raw := range info.Symbols {
		var symbol struct {
			Symbol string `json:"symbol"`
		}
		if json.Unmarshal(raw, &symbol) == nil && symbol.Symbol == strings.ToUpper(b.cfg.Symbol) {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("没有找到 %s 的合约过滤器", b.cfg.Symbol)
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

// ---------------------------------------------------------------- protective orders

// PlaceProtective puts exchange-side protective orders on an open position:
// a STOP_MARKET stop-loss leg and, when a target is also present, a
// TAKE_PROFIT_MARKET leg. Each is a self-contained closePosition=true order
// requiring no quantity, so a fill on one leg does not orphan the other and
// the exchange independently guarantees each protective level. Prices are
// taken verbatim from the risk engine (single source) — never recomputed
// here, so the local and exchange stops cannot drift apart. No-op in dry-run.
//
// Trade-off: closePosition MARKET legs buy a guarantee the position is always
// covered at the cost of a little execution slippage. The slippage-controlled
// alternative (a LIMIT_STOP + OCO list) is a documented future hardening, not
// a first cut: OCO leg-side/price semantics differ by long/short and cannot
// be verified against the real exchange offline.
func (b *FuturesBroker) PlaceProtective(side Side, stop, takeProfit float64) error {
	if b.cfg.DryRun {
		return nil
	}
	if stop > 0 {
		if err := b.placeClosePosition("STOP_MARKET", "stopPrice", side, stop); err != nil {
			return err
		}
	}
	if takeProfit > 0 {
		if err := b.placeClosePosition("TAKE_PROFIT_MARKET", "price", side, takeProfit); err != nil {
			return err
		}
	}
	return nil
}

// placeClosePosition places one closePosition protective leg. For
// STOP_MARKET the trigger is stopPrice; for TAKE_PROFIT_MARKET it is price.
// The close side is opposite to the position side (closing a long = SELL).
func (b *FuturesBroker) placeClosePosition(orderType, triggerKey string, side Side, level float64) error {
	closeSide := "SELL" // closing a long
	if side != Buy {
		closeSide = "BUY" // closing a short
	}
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	q.Set("side", closeSide)
	q.Set("type", orderType)
	q.Set("closePosition", "true")
	q.Set("positionSide", "BOTH")
	q.Set("workingType", "MARK_PRICE")
	q.Set(triggerKey, strconv.FormatFloat(level, 'f', -1, 64))
	q.Set("newClientOrderId", newClientID("tap-"))
	q.Set("newOrderRespType", "ACK")
	_, err := b.postSigned("/fapi/v1/order", q)
	return err
}

// CancelProtective cancels every protective open order on the position's
// symbol. Called before any local flatten so the exchange side cannot
// double-close a position the book already closed. No-op in dry-run.
func (b *FuturesBroker) CancelProtective() error {
	if b.cfg.DryRun {
		return nil
	}
	return b.deleteAllOpenOrders()
}

// HasProtective reports whether any protective (close-position stop) order is
// still open on the symbol, so a restart does not double-place. It looks at
// the symbol's open orders for STOP_MARKET/TAKE_PROFIT_MARKET closePosition
// legs. Returns false in dry-run.
func (b *FuturesBroker) HasProtective() (bool, error) {
	if b.cfg.DryRun {
		return false, nil
	}
	body, err := b.getSigned("/fapi/v1/openOrders", url.Values{})
	if err != nil {
		return false, err
	}
	var rows []struct {
		Symbol   string      `json:"symbol"`
		Type     string      `json:"type"`
		ClosePos interface{} `json:"closePosition"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return false, fmt.Errorf("解析 openOrders 失败: %w", err)
	}
	want := strings.ToUpper(b.cfg.Symbol)
	for _, row := range rows {
		if strings.ToUpper(row.Symbol) != want {
			continue
		}
		if row.Type == "STOP_MARKET" || row.Type == "TAKE_PROFIT_MARKET" || row.Type == "STOP" || row.Type == "TAKE_PROFIT" {
			if protectiveFlag(row.ClosePos) {
				return true, nil
			}
		}
	}
	return false, nil
}

// protectiveFlag tolerates the exchange returning closePosition as either a
// boolean or a string, so the reconciliation check does not depend on the
// exact response encoding.
func protectiveFlag(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}

// deleteAllOpenOrders cancels all open orders on the symbol. It is used by
// CancelProtective; only protective orders exist in a well-formed session.
func (b *FuturesBroker) deleteAllOpenOrders() error {
	query := url.Values{}
	query.Set("symbol", strings.ToUpper(b.cfg.Symbol))
	if err := signQuery(b.cfg.SecretKey, query); err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodDelete,
		b.cfg.BaseURL+"/fapi/v1/allOpenOrders"+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-MBX-APIKEY", b.cfg.APIKey)
	response, err := b.http.Do(request)
	if err != nil {
		return fmt.Errorf("撤销保护单失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return httpError(response.StatusCode, body)
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
