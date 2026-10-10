// FuturesBroker places orders on Binance USDT-margined perpetual futures,
// the only venue this program trades. It holds a signed position (long or
// short), so both entry directions the strategy emits are tradable, and a
// position is sized in base-coin quantity with leverage applied to the
// margin budget.
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
	Leverage      int    // 1 = unleveraged, up to the exchange max
	MarginMode    string // "ISOLATED" (default) or "CROSS"
	StepSize      float64
	MaxPriceDev   float64 // reject fills deviating more than this fraction
	CommissionBps *float64
	SlippageBps   *float64
	JournalPath   string
	// ProtectiveJournalPath is a durable intent file for conditional legs.
	// It is separate from the market-order intent because a position can be
	// filled even when the protective POST response is lost.
	ProtectiveJournalPath string
}

// FuturesBroker implements Broker against Binance USDT-margined perpetuals.
type FuturesBroker struct {
	cfg     FuturesConfig
	http    *http.Client
	Trades  []Fill
	StepSz  float64
	usedIDs map[string]bool
	// clock corrects the host clock against the exchange's; see clock.go.
	clock exchangeClock
	// Resolved by Init from the exchange contract info.
	QuantityPrecision int
}

type protectiveLeg struct {
	orderType string
	level     float64
	present   bool
	clientID  string
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
	// Align with the exchange before setLeverage, the first signed call: a host
	// clock more than a second off makes every signed request fail with -1021.
	b.trySyncClock()
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

// syncClock measures the offset between the host clock and the exchange's.
func (b *FuturesBroker) syncClock() error {
	return b.clock.measure(b.http, b.cfg.BaseURL+"/fapi/v1/time")
}

// trySyncClock aligns the clock, tolerating a failure; see BinanceBroker.
func (b *FuturesBroker) trySyncClock() {
	_ = b.syncClock()
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
	want := strings.ToUpper(b.cfg.Symbol)
	body, err := b.getSigned("/fapi/v2/positionRisk", url.Values{"symbol": {want}})
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
	var matched []struct {
		Symbol       string
		PositionAmt  json.Number
		EntryPrice   json.Number
		PositionSide string
	}
	for _, row := range rows {
		if strings.ToUpper(row.Symbol) == want {
			matched = append(matched, struct {
				Symbol       string
				PositionAmt  json.Number
				EntryPrice   json.Number
				PositionSide string
			}{row.Symbol, row.PositionAmt, row.EntryPrice, row.PositionSide})
		}
	}
	if len(matched) > 1 {
		return "", 0, 0, fmt.Errorf("交易所返回重复的 %s 持仓记录", want)
	}
	if len(matched) == 0 {
		return "", 0, 0, fmt.Errorf("交易所未返回 %s 的持仓记录", want)
	}
	row := matched[0]
	amt, parseErr := row.PositionAmt.Float64()
	if parseErr != nil || math.IsNaN(amt) || math.IsInf(amt, 0) {
		return "", 0, 0, fmt.Errorf("交易所持仓数量无效")
	}
	if row.PositionSide != "" && row.PositionSide != "BOTH" {
		return "", 0, 0, fmt.Errorf("当前仅支持单向持仓模式，不支持 Hedge Mode")
	}
	entr, parseErr := row.EntryPrice.Float64()
	if parseErr != nil || math.IsNaN(entr) || math.IsInf(entr, 0) || entr < 0 {
		return "", 0, 0, fmt.Errorf("交易所持仓开仓均价无效")
	}
	switch {
	case amt > 0:
		if entr <= 0 {
			return "", 0, 0, fmt.Errorf("交易所多头持仓缺少开仓均价")
		}
		return Buy, amt, entr, nil
	case amt < 0:
		if entr <= 0 {
			return "", 0, 0, fmt.Errorf("交易所空头持仓缺少开仓均价")
		}
		return Sell, -amt, entr, nil
	case entr != 0:
		return "", 0, 0, fmt.Errorf("交易所空仓却返回非零开仓均价")
	default:
		return "", 0, 0, nil
	}
}

// USDTBalance returns the wallet balance used by the existing live book.
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
	found := false
	var balance float64
	for _, row := range rows {
		if strings.ToUpper(row.Asset) == "USDT" {
			if found {
				return 0, fmt.Errorf("合约账户返回重复的 USDT 余额")
			}
			value, err := positiveAccountNumber(row.WalletBalance)
			if err != nil {
				return 0, fmt.Errorf("USDT 钱包余额无效: %w", err)
			}
			found, balance = true, value
		}
	}
	if !found {
		return 0, fmt.Errorf("合约账户未返回 USDT 余额")
	}
	return balance, nil
}

// AccountBalance is a read-only snapshot of the USDT-margined futures wallet.
// It must not be confused with the local paper/session portfolio.
type AccountBalance struct {
	Wallet          float64 `json:"wallet"`
	Available       float64 `json:"available"`
	CrossUnrealized float64 `json:"cross_unrealized"`
}

// Account reads Binance's signed balance endpoint without initializing the
// broker; in particular it never changes leverage, margin mode, or orders.
func (b *FuturesBroker) Account() (AccountBalance, error) {
	if b.cfg.APIKey == "" || b.cfg.SecretKey == "" {
		return AccountBalance{}, fmt.Errorf("Binance API Key 与 Secret Key 未配置")
	}
	b.trySyncClock()
	body, err := b.getSigned("/fapi/v2/balance", url.Values{})
	if err != nil {
		return AccountBalance{}, err
	}
	var rows []struct {
		Asset         string      `json:"asset"`
		WalletBalance json.Number `json:"balance"`
		Available     json.Number `json:"availableBalance"`
		Unrealized    json.Number `json:"crossUnPnl"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return AccountBalance{}, fmt.Errorf("解析余额失败: %w", err)
	}
	for _, row := range rows {
		if strings.ToUpper(row.Asset) == "USDT" {
			wallet, err := positiveAccountNumber(row.WalletBalance)
			if err != nil {
				return AccountBalance{}, fmt.Errorf("USDT 钱包余额无效: %w", err)
			}
			available, err := positiveAccountNumber(row.Available)
			if err != nil {
				return AccountBalance{}, fmt.Errorf("USDT 可用余额无效: %w", err)
			}
			if row.Unrealized == "" {
				return AccountBalance{}, fmt.Errorf("USDT 全仓未实现盈亏缺失")
			}
			unrealized, err := row.Unrealized.Float64()
			if err != nil || math.IsNaN(unrealized) || math.IsInf(unrealized, 0) {
				return AccountBalance{}, fmt.Errorf("USDT 全仓未实现盈亏无效")
			}
			return AccountBalance{Wallet: wallet, Available: available, CrossUnrealized: unrealized}, nil
		}
	}
	return AccountBalance{}, fmt.Errorf("合约账户未返回 USDT 余额")
}

func positiveAccountNumber(value json.Number) (float64, error) {
	if value == "" {
		return 0, fmt.Errorf("缺少字段")
	}
	n, err := value.Float64()
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("数值无效")
	}
	return n, nil
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
// requiring no quantity. These are independent legs, not an OCO group;
// reconciliation must clean up a remaining leg after the position is flat.
// Prices are taken verbatim from the risk engine (single source), never recomputed
// here, so the local and exchange stops cannot drift apart. No-op in dry-run.
//
// USD-M conditional orders use the Algo Order API, not the regular order
// endpoint. Both legs use triggerPrice; price is only a limit execution price.
// Independent close-all legs are not an OCO list and do not guarantee fills
// under outages or extreme market conditions.
func (b *FuturesBroker) PlaceProtective(side Side, stop, takeProfit float64) error {
	if b.cfg.DryRun {
		return nil
	}
	if side != Buy && side != Sell {
		return fmt.Errorf("保护单持仓方向无效: %q", side)
	}
	if !validProtectionLevel(stop) || (takeProfit != 0 && !validProtectionLevel(takeProfit)) {
		return fmt.Errorf("保护单触发价无效，拒绝下单")
	}
	rows, err := b.protectiveOrders()
	if err != nil {
		return err // Never write when the exchange state cannot be inspected.
	}
	closeSide := "SELL"
	if side == Sell {
		closeSide = "BUY"
	}
	legs := []protectiveLeg{{orderType: "STOP_MARKET", level: stop}, {orderType: "TAKE_PROFIT_MARKET", level: takeProfit}}
	// Reuse an intent left by a crash. The client IDs are the idempotency key;
	// replacing them here would turn a lost response into a duplicate order.
	intent, hasIntent, err := loadProtectiveIntent(b.cfg.ProtectiveJournalPath)
	if err != nil {
		return err
	}
	if hasIntent {
		if strings.ToUpper(intent.Symbol) != strings.ToUpper(b.cfg.Symbol) || intent.Side != side {
			return fmt.Errorf("保护单意图与当前持仓不一致，需人工对账")
		}
		for _, pending := range intent.Legs {
			for i := range legs {
				if legs[i].orderType == pending.OrderType {
					if legs[i].level != pending.TriggerPrice {
						return fmt.Errorf("保护单意图 %s 触发价与当前持仓不一致", pending.OrderType)
					}
					legs[i].clientID = pending.ClientAlgoID
				}
			}
		}
	}
	// Validate all existing legs before any write. A stale, duplicate or
	// malformed owned order requires reconciliation, not another close-all leg.
	for _, row := range rows {
		matched := false
		for i := range legs {
			leg := &legs[i]
			if row.OrderType != leg.orderType {
				continue
			}
			matched = true
			level, parseErr := strconv.ParseFloat(row.TriggerPrice, 64)
			if row.AlgoID <= 0 || row.AlgoStatus != "NEW" || !protectiveFlag(row.ClosePos) ||
				row.Side != closeSide || row.PositionSide != "BOTH" ||
				row.WorkingType != "MARK_PRICE" || parseErr != nil ||
				math.IsNaN(level) || math.IsInf(level, 0) || level != leg.level ||
				leg.level <= 0 || leg.present {
				return fmt.Errorf("交易所保护单 %s 与本地持仓不一致，拒绝重复补挂", row.OrderType)
			}
			leg.present = true
			if hasIntent && leg.clientID != "" && row.ClientAlgoID != leg.clientID {
				return fmt.Errorf("保护单 %s 身份与持久化意图不一致，需人工对账", row.OrderType)
			}
		}
		if !matched {
			return fmt.Errorf("发现未知类型的本程序保护单 %s，需人工对账", row.OrderType)
		}
	}
	if hasIntent {
		for _, pending := range intent.Legs {
			for _, leg := range legs {
				if leg.orderType != pending.OrderType || leg.present {
					continue
				}
				// A missing open leg may have been accepted just before a crash.
				// Query its original identity for diagnosis, but never POST it
				// again: even a not-found answer is not a fill/position proof.
				_, queryErr := b.getSigned("/fapi/v1/algoOrder", url.Values{"clientAlgoId": {pending.ClientAlgoID}})
				if queryErr != nil {
					return fmt.Errorf("保护单 %s 未在开放列表中，按原 ID 查询失败，需人工对账: %w", pending.ClientAlgoID, queryErr)
				}
				return fmt.Errorf("保护单 %s 未在开放列表中，需核对终态与成交后人工恢复", pending.ClientAlgoID)
			}
		}
		for _, leg := range legs {
			if leg.level > 0 && !leg.present {
				return fmt.Errorf("保护单意图未覆盖缺失的 %s 腿，需人工对账", leg.orderType)
			}
		}
		return nil
	}
	for i := range legs {
		if legs[i].level > 0 && !legs[i].present && legs[i].clientID == "" {
			legs[i].clientID = newClientID("tap-")
		}
	}
	if err := b.ensureProtectiveIntent(side, legs); err != nil {
		return err
	}
	for i := range legs {
		leg := &legs[i]
		if leg.level > 0 && !leg.present {
			if err := b.placeClosePosition(leg.orderType, side, leg.level, leg.clientID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureProtectiveIntent writes all missing legs before the first POST.
func (b *FuturesBroker) ensureProtectiveIntent(side Side, legs []protectiveLeg) error {
	if b.cfg.ProtectiveJournalPath == "" {
		return nil
	}
	if _, ok, err := loadProtectiveIntent(b.cfg.ProtectiveJournalPath); err != nil {
		return err
	} else if ok {
		return nil
	}
	var pending []protectiveIntentLeg
	for i := range legs {
		leg := &legs[i]
		if leg.level > 0 && !leg.present {
			pending = append(pending, protectiveIntentLeg{
				OrderType: leg.orderType, ClientAlgoID: leg.clientID, TriggerPrice: leg.level,
			})
		}
	}
	if len(pending) == 0 {
		return nil
	}
	_, err := createProtectiveIntent(b.cfg.ProtectiveJournalPath, b.cfg.Symbol, side, pending)
	return err
}

// placeClosePosition places one closePosition conditional leg using triggerPrice.
// The close side is opposite to the position side (closing a long = SELL).
func (b *FuturesBroker) placeClosePosition(orderType string, side Side, level float64, clientID string) error {
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
	q.Set("algoType", "CONDITIONAL")
	q.Set("triggerPrice", strconv.FormatFloat(level, 'f', -1, 64))
	q.Set("clientAlgoId", clientID)
	q.Set("newOrderRespType", "ACK")
	_, err := b.postSigned("/fapi/v1/algoOrder", q)
	if err == nil {
		return nil
	}
	// A failed response does not prove that placement failed. Query the exact
	// submitted identity once; never retry POST with a fresh clientAlgoId.
	body, queryErr := b.getSigned("/fapi/v1/algoOrder", url.Values{"clientAlgoId": {clientID}})
	if queryErr != nil {
		return fmt.Errorf("保护单 %s 写入失败且按 ID 查单失败，需对账: %w", clientID, queryErr)
	}
	var order protectiveAlgoOrder
	if json.Unmarshal(body, &order) != nil || order.AlgoID <= 0 ||
		order.ClientAlgoID != clientID || order.Symbol != strings.ToUpper(b.cfg.Symbol) ||
		order.OrderType != orderType || order.Side != closeSide ||
		order.PositionSide != "BOTH" || order.WorkingType != "MARK_PRICE" ||
		!protectiveFlag(order.ClosePos) || order.AlgoStatus != "NEW" {
		return fmt.Errorf("保护单 %s 按 ID 查单无法确认有效保护，需对账", clientID)
	}
	trigger, parseErr := strconv.ParseFloat(order.TriggerPrice, 64)
	if parseErr != nil || math.IsNaN(trigger) || math.IsInf(trigger, 0) || trigger != level {
		return fmt.Errorf("保护单 %s 查单触发价与本地不一致，需对账", clientID)
	}
	return nil
}

// CancelProtective cancels only this agent's close-all conditional legs, by
// algo ID. Manual orders and orders on other symbols must remain untouched.
func (b *FuturesBroker) CancelProtective() error {
	if b.cfg.DryRun {
		return nil
	}
	rows, err := b.protectiveOrders()
	if err != nil {
		return err
	}
	// Validate the complete set before the first DELETE. Discovering a bad
	// second leg after cancelling a valid first leg would reduce protection
	// while the caller still believes cancellation failed atomically.
	seenAlgo := map[int64]bool{}
	seenClient := map[string]bool{}
	seenType := map[string]bool{}
	closeSide := ""
	for _, row := range rows {
		trigger, parseErr := strconv.ParseFloat(row.TriggerPrice, 64)
		if row.AlgoID <= 0 || seenAlgo[row.AlgoID] || row.ClientAlgoID == "" || seenClient[row.ClientAlgoID] ||
			seenType[row.OrderType] || row.AlgoStatus != "NEW" || !protectiveFlag(row.ClosePos) ||
			(row.OrderType != "STOP_MARKET" && row.OrderType != "TAKE_PROFIT_MARKET") ||
			(row.Side != "BUY" && row.Side != "SELL") || row.PositionSide != "BOTH" ||
			row.WorkingType != "MARK_PRICE" || parseErr != nil || !validProtectionLevel(trigger) {
			return fmt.Errorf("保护单字段或状态不完整，拒绝撤单")
		}
		if closeSide == "" {
			closeSide = row.Side
		} else if row.Side != closeSide {
			return fmt.Errorf("保护单平仓方向不一致，拒绝撤单")
		}
		seenAlgo[row.AlgoID] = true
		seenClient[row.ClientAlgoID] = true
		seenType[row.OrderType] = true
	}
	// The open list cannot prove that an intended leg never triggered. Check
	// persisted identities before removing any surviving protection; a missing
	// or replaced leg requires terminal/fill reconciliation, not a local exit.
	if b.cfg.Leverage > 1 {
		intent, pending, err := loadProtectiveIntent(b.cfg.ProtectiveJournalPath)
		if err != nil {
			return err
		}
		if pending {
			if intent.Symbol != strings.ToUpper(b.cfg.Symbol) {
				return fmt.Errorf("保护单意图币种不一致，拒绝撤单，需对账")
			}
			expectedSide := "SELL"
			if intent.Side == Sell {
				expectedSide = "BUY"
			}
			for _, leg := range intent.Legs {
				matched := false
				for _, row := range rows {
					if row.OrderType != leg.OrderType {
						continue
					}
					expectedTrigger := strconv.FormatFloat(leg.TriggerPrice, 'f', -1, 64)
					if row.ClientAlgoID != leg.ClientAlgoID || row.Side != expectedSide ||
						!equalPositiveDecimal(row.TriggerPrice, expectedTrigger) {
						return fmt.Errorf("保护单与持久化意图不一致，拒绝撤单，需对账")
					}
					matched = true
				}
				if !matched {
					return fmt.Errorf("保护单 %s 未在开放列表中，需核对终态与成交后再撤单", leg.ClientAlgoID)
				}
			}
		}
	}
	for _, row := range rows {
		if err := b.cancelAlgoOrder(row); err != nil {
			return err
		}
		if b.cfg.Leverage > 1 {
			if err := b.confirmUntriggeredCancellation(row); err != nil {
				return err
			}
		}
	}
	return nil
}

// HasProtective preserves the existing any-leg reconciliation contract.
func (b *FuturesBroker) HasProtective() (bool, error) {
	if b.cfg.DryRun {
		return false, nil
	}
	rows, err := b.protectiveOrders()
	return len(rows) > 0, err
}

// InspectProtective verifies the exact protection expected for the current
// position. An empty side means the local book is flat; any owned exchange
// leg is then reported as a residual conflict rather than as coverage.
func (b *FuturesBroker) InspectProtective(side Side, stop, takeProfit float64) (ProtectionSnapshot, error) {
	checked := ProtectionSnapshot{CheckedAt: time.Now().UTC()}
	if b.cfg.DryRun {
		checked.State = ProtectionNotRequired
		return checked, nil
	}
	intent, pending, err := loadProtectiveIntent(b.cfg.ProtectiveJournalPath)
	if err != nil {
		checked.State = ProtectionUnknown
		checked.Reason = "保护单意图无法读取"
		return checked, err
	}
	rows, err := b.protectiveOrders()
	if err != nil {
		checked.State = ProtectionUnknown
		checked.Reason = "无法读取交易所保护单"
		return checked, err
	}
	closeSide := ""
	if side == Buy {
		closeSide = "SELL"
	} else if side == Sell {
		closeSide = "BUY"
	} else if side != "" {
		checked.State = ProtectionConflict
		checked.Reason = "本地持仓方向无效"
		return checked, nil
	}
	expected := map[string]float64{}
	if side != "" {
		if !validProtectionLevel(stop) {
			checked.State = ProtectionConflict
			checked.Reason = "本地止损价无效"
			return checked, nil
		}
		expected["STOP_MARKET"] = stop
		if takeProfit > 0 {
			if !validProtectionLevel(takeProfit) {
				checked.State = ProtectionConflict
				checked.Reason = "本地止盈价无效"
				return checked, nil
			}
			expected["TAKE_PROFIT_MARKET"] = takeProfit
		}
	}
	if pending {
		if intent.Symbol != strings.ToUpper(b.cfg.Symbol) || intent.Side != side {
			checked.State = ProtectionConflict
			checked.Reason = "保护单意图与当前持仓不一致"
			return checked, nil
		}
		for _, leg := range intent.Legs {
			if expected[leg.OrderType] != leg.TriggerPrice {
				checked.State = ProtectionConflict
				checked.Reason = "保护单意图触发价与本地持仓不一致"
				return checked, nil
			}
		}
	}
	seen := map[string]bool{}
	for _, row := range rows {
		level, parseErr := strconv.ParseFloat(row.TriggerPrice, 64)
		valid := row.AlgoID > 0 && row.ClientAlgoID != "" && protectiveFlag(row.ClosePos) && row.Side == closeSide &&
			row.PositionSide == "BOTH" && row.WorkingType == "MARK_PRICE" &&
			parseErr == nil && validProtectionLevel(level) && row.AlgoStatus == "NEW"
		if !valid {
			checked.State = ProtectionConflict
			checked.Reason = "交易所保护单字段冲突或状态无效"
			return checked, nil
		}
		want, isExpected := expected[row.OrderType]
		priceMatches := want == level
		if b.cfg.Leverage > 1 {
			// Use the POST encoding, not a float comparison, at recovery and
			// cycle gates too. Rounded conflicts must never become verified.
			priceMatches = equalPositiveDecimal(row.TriggerPrice, strconv.FormatFloat(want, 'f', -1, 64))
		}
		if !isExpected || seen[row.OrderType] || !priceMatches {
			checked.State = ProtectionConflict
			checked.Reason = "交易所保护单类型或触发价与本地不一致"
			return checked, nil
		}
		if pending {
			for _, leg := range intent.Legs {
				if leg.OrderType == row.OrderType && leg.ClientAlgoID != row.ClientAlgoID {
					checked.State = ProtectionConflict
					checked.Reason = "保护单身份与持久化意图不一致"
					return checked, nil
				}
			}
		}
		seen[row.OrderType] = true
		leg := ProtectionLeg{Present: true, AlgoID: row.AlgoID, ClientAlgoID: row.ClientAlgoID,
			OrderType: row.OrderType, TriggerPrice: level, Status: row.AlgoStatus}
		if row.OrderType == "STOP_MARKET" {
			checked.Stop = leg
		} else if row.OrderType == "TAKE_PROFIT_MARKET" {
			checked.Target = leg
		}
	}
	if len(expected) == 0 {
		if len(rows) == 0 {
			checked.State = ProtectionNotRequired
			return checked, nil
		}
		checked.State = ProtectionConflict
		checked.Reason = "本地已空仓但交易所仍有保护单"
		return checked, nil
	}
	if len(seen) == len(expected) {
		checked.State = ProtectionVerified
		return checked, nil
	}
	if len(seen) == 0 {
		checked.State = ProtectionMissing
		checked.Reason = "交易所保护单缺失"
	} else {
		checked.State = ProtectionPartial
		checked.Reason = "交易所保护单只存在部分保护腿"
	}
	return checked, nil
}

type protectiveAlgoOrder struct {
	AlgoID       int64       `json:"algoId"`
	AlgoStatus   string      `json:"algoStatus"`
	ClientAlgoID string      `json:"clientAlgoId"`
	Symbol       string      `json:"symbol"`
	OrderType    string      `json:"orderType"`
	Side         string      `json:"side"`
	PositionSide string      `json:"positionSide"`
	WorkingType  string      `json:"workingType"`
	TriggerPrice string      `json:"triggerPrice"`
	ClosePos     interface{} `json:"closePosition"`
}

func (b *FuturesBroker) protectiveOrders() ([]protectiveAlgoOrder, error) {
	body, err := b.getSigned("/fapi/v1/openAlgoOrders", url.Values{
		"symbol": {strings.ToUpper(b.cfg.Symbol)}, "algoType": {"CONDITIONAL"},
	})
	if err != nil {
		return nil, err
	}
	var rows []protectiveAlgoOrder
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("解析 openAlgoOrders 失败: %w", err)
	}
	var owned []protectiveAlgoOrder
	for _, row := range rows {
		if strings.ToUpper(row.Symbol) != strings.ToUpper(b.cfg.Symbol) ||
			!strings.HasPrefix(row.ClientAlgoID, "tap-") {
			continue
		}
		owned = append(owned, row)
	}
	return owned, nil
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

// cancelAlgoOrder deletes one identified conditional leg, never all orders.
func (b *FuturesBroker) cancelAlgoOrder(order protectiveAlgoOrder) error {
	query := url.Values{"algoId": {strconv.FormatInt(order.AlgoID, 10)}}
	body, err := b.signedRequest(http.MethodDelete, "/fapi/v1/algoOrder", query)
	if err != nil {
		return fmt.Errorf("撤销保护单失败: %w", err)
	}
	// Binance's cancel endpoint returns an acknowledgement, not the full
	// terminal order. HTTP 200 alone (or an unrelated order's ACK) cannot
	// authorize a local exit. Accept the documented string code and the
	// numeric encoding, but never infer success from missing fields.
	var ack struct {
		AlgoID       int64           `json:"algoId"`
		ClientAlgoID string          `json:"clientAlgoId"`
		Code         json.RawMessage `json:"code"`
		Message      string          `json:"msg"`
	}
	if json.Unmarshal(body, &ack) != nil || ack.AlgoID != order.AlgoID ||
		ack.ClientAlgoID != order.ClientAlgoID || ack.Message != "success" ||
		(string(ack.Code) != "200" && string(ack.Code) != `"200"`) {
		return fmt.Errorf("撤销保护单响应无法确认原订单成功撤销，需对账")
	}
	return nil
}

// confirmUntriggeredCancellation reads terminal evidence after an exact ACK.
// A triggered child can fill even when the conditional leg is no longer open;
// never authorize a local flatten based on disappearance or the ACK alone.
func (b *FuturesBroker) confirmUntriggeredCancellation(original protectiveAlgoOrder) error {
	body, err := b.getSigned("/fapi/v1/algoOrder", url.Values{
		"algoId": {strconv.FormatInt(original.AlgoID, 10)},
	})
	if err != nil {
		return fmt.Errorf("读取保护单撤销终态失败，需对账: %w", err)
	}
	var terminal struct {
		protectiveAlgoOrder
		ActualOrderID *string         `json:"actualOrderId"`
		ActualPrice   *string         `json:"actualPrice"`
		ActualQty     json.RawMessage `json:"actualQty"`
		TriggerTime   *int64          `json:"triggerTime"`
	}
	if json.Unmarshal(body, &terminal) != nil || terminal.AlgoID != original.AlgoID ||
		terminal.ClientAlgoID != original.ClientAlgoID || terminal.Symbol != original.Symbol ||
		terminal.OrderType != original.OrderType || terminal.Side != original.Side ||
		terminal.PositionSide != original.PositionSide || terminal.WorkingType != original.WorkingType ||
		!protectiveFlag(terminal.ClosePos) || terminal.AlgoStatus != "CANCELED" ||
		terminal.ActualOrderID == nil || *terminal.ActualOrderID != "" ||
		terminal.TriggerTime == nil || *terminal.TriggerTime != 0 || terminal.ActualPrice == nil {
		return fmt.Errorf("保护单撤销终态或触发证据无法确认，需对账")
	}
	if !equalPositiveDecimal(terminal.TriggerPrice, original.TriggerPrice) || !explicitDecimalZero(*terminal.ActualPrice) {
		return fmt.Errorf("保护单撤销价格证据不一致，需对账")
	}
	// actualQty is optional when untriggered. If supplied, require explicit
	// zero rather than accepting malformed/null/non-finite fill evidence.
	if len(terminal.ActualQty) > 0 {
		var quantity string
		if json.Unmarshal(terminal.ActualQty, &quantity) != nil {
			return fmt.Errorf("保护单撤销成交量无法确认，需对账")
		}
		if !explicitDecimalZero(quantity) {
			return fmt.Errorf("保护单已成交或成交量无效，需对账")
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
	// exchangeInfo can exceed 1 MiB when the network egress returns the
	// full symbol list despite the symbol= filter (observed: ~1.15 MB),
	// so cap well above that instead of truncating the JSON mid-stream.
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
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
	return b.signedRequest(http.MethodGet, path, query)
}

// postSigned signs and posts a futures trading request.
func (b *FuturesBroker) postSigned(path string, query url.Values) ([]byte, error) {
	return b.signedRequest(http.MethodPost, path, query)
}

// signedRequest signs and sends one private futures request, re-syncing the
// clock and retrying once when the exchange rejects the timestamp.
func (b *FuturesBroker) signedRequest(method, path string, query url.Values) ([]byte, error) {
	return signAndRetry(b.syncClock, func() ([]byte, error) {
		return b.signedOnce(method, path, query)
	})
}

// signedOnce performs a single signed attempt with the current clock offset.
func (b *FuturesBroker) signedOnce(method, path string, query url.Values) ([]byte, error) {
	signed := signedQuery(b.cfg.SecretKey, b.clock.timestamp(), query)
	request, err := http.NewRequest(method,
		b.cfg.BaseURL+path+"?"+signed, nil)
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
