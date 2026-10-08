package broker

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Spot protection uses one quantity-bound STOP_LOSS, not independent stop and
// target SELL legs (which would reserve the same inventory twice). Callers must
// supply the net position after base-asset fees. Wiring into live is separate.
type spotProtectiveOrder struct {
	OrderID  int64  `json:"orderId"`
	ClientID string `json:"clientOrderId"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Quantity string `json:"origQty"`
	Executed string `json:"executedQty"`
	Stop     string `json:"stopPrice"`
	ListID   int64  `json:"orderListId"`
}

func (b *BinanceBroker) spotProtectiveOrders() ([]spotProtectiveOrder, error) {
	body, err := b.getSigned("/api/v3/openOrders", url.Values{"symbol": {strings.ToUpper(b.cfg.Symbol)}})
	if err != nil {
		return nil, err
	}
	var rows []spotProtectiveOrder
	if err := json.Unmarshal(body, &rows); err != nil || rows == nil {
		return nil, fmt.Errorf("现货 openOrders 响应无效")
	}
	var owned []spotProtectiveOrder
	for _, row := range rows {
		if row.Symbol == strings.ToUpper(b.cfg.Symbol) && strings.HasPrefix(row.ClientID, "tas-") && row.Type == "STOP_LOSS" {
			owned = append(owned, row)
		}
	}
	return owned, nil
}

func spotNumber(text string, want float64) bool {
	got, err := strconv.ParseFloat(text, 64)
	return err == nil && !math.IsNaN(got) && !math.IsInf(got, 0) && got == want
}

func (b *BinanceBroker) validSpotStop(row spotProtectiveOrder, id string, qty, stop float64) bool {
	return row.OrderID > 0 && row.ClientID == id && row.Symbol == strings.ToUpper(b.cfg.Symbol) &&
		row.Type == "STOP_LOSS" && row.Side == "SELL" && row.Status == "NEW" && row.ListID == -1 &&
		spotNumber(row.Quantity, qty) && spotNumber(row.Stop, stop) && spotNumber(row.Executed, 0)
}

// PlaceSpotStop inspects before writing and confirms ambiguous writes by their
// original identity once. It never blindly resubmits an uncertain order.
func (b *BinanceBroker) PlaceSpotStop(quantity, stop float64) error {
	if b.cfg.DryRun {
		return nil
	}
	if quantity <= 0 || stop <= 0 || math.IsNaN(quantity) || math.IsInf(quantity, 0) || math.IsNaN(stop) || math.IsInf(stop, 0) {
		return fmt.Errorf("现货保护单数量/触发价无效")
	}
	qty := b.roundQuantity(quantity)
	// Use exactly the representation submitted on the wire for reconciliation.
	qty, _ = strconv.ParseFloat(formatQty(qty), 64)
	if qty <= 0 {
		return fmt.Errorf("现货保护单数量低于步长")
	}
	rows, err := b.spotProtectiveOrders()
	if err != nil {
		return err
	}
	if len(rows) > 1 {
		return fmt.Errorf("现货保护单重复，需对账")
	}
	if len(rows) == 1 {
		if !b.validSpotStop(rows[0], rows[0].ClientID, qty, stop) {
			return fmt.Errorf("现货保护单与本地持仓不一致，需对账")
		}
		return nil
	}
	id := newClientID("tas-")
	q := url.Values{"symbol": {strings.ToUpper(b.cfg.Symbol)}, "side": {"SELL"}, "type": {"STOP_LOSS"},
		"quantity": {formatQty(qty)}, "stopPrice": {strconv.FormatFloat(stop, 'f', -1, 64)}, "newClientOrderId": {id}, "newOrderRespType": {"RESULT"}}
	body, postErr := b.postSigned("/api/v3/order", q)
	if postErr != nil {
		body, err = b.getSigned("/api/v3/order", url.Values{"symbol": {strings.ToUpper(b.cfg.Symbol)}, "origClientOrderId": {id}})
		if err != nil {
			return fmt.Errorf("现货保护单写入/查单未确认，需对账: %w", err)
		}
	}
	var row spotProtectiveOrder
	if json.Unmarshal(body, &row) != nil || !b.validSpotStop(row, id, qty, stop) {
		return fmt.Errorf("现货保护单响应无法确认有效保护，需对账")
	}
	return nil
}

// HasSpotStops reports agent-owned open stops, not validated full coverage.
// Call PlaceSpotStop to validate quantity and trigger price before reuse.
func (b *BinanceBroker) HasSpotStops() (bool, error) {
	if b.cfg.DryRun {
		return false, nil
	}
	rows, err := b.spotProtectiveOrders()
	return len(rows) > 0, err
}

// CancelSpotStops cancels only owned standalone stop orders. Confirm zero fills
// before allowing a caller to flatten: a stop racing cancellation can sell part
// of the inventory, so blindly selling the original local quantity is unsafe.
func (b *BinanceBroker) CancelSpotStops() error {
	if b.cfg.DryRun {
		return nil
	}
	rows, err := b.spotProtectiveOrders()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.OrderID <= 0 || row.Side != "SELL" || row.ListID != -1 || !spotNumber(row.Executed, 0) {
			return fmt.Errorf("现货保护单状态不安全，需对账")
		}
	}
	for _, row := range rows {
		q := url.Values{"symbol": {strings.ToUpper(b.cfg.Symbol)}, "orderId": {strconv.FormatInt(row.OrderID, 10)}}
		if err := signQuery(b.cfg.SecretKey, q); err != nil {
			return err
		}
		response, err := b.do(http.MethodDelete, b.cfg.BaseURL+"/api/v3/order?"+q.Encode(), &b.cfg.APIKey, false)
		if err != nil {
			return err
		}
		body, err := b.readBody(response)
		if err != nil {
			return err
		}
		var canceled spotProtectiveOrder
		// A terminal status alone is not proof that the requested stop was
		// canceled. Confirm the full original identity and immutable fields.
		qty, qtyErr := strconv.ParseFloat(row.Quantity, 64)
		stop, stopErr := strconv.ParseFloat(row.Stop, 64)
		if json.Unmarshal(body, &canceled) != nil || qtyErr != nil || stopErr != nil || qty <= 0 || stop <= 0 ||
			canceled.OrderID != row.OrderID || canceled.ClientID != row.ClientID || canceled.Symbol != row.Symbol ||
			canceled.Side != "SELL" || canceled.Type != "STOP_LOSS" || canceled.ListID != -1 ||
			canceled.Status != "CANCELED" || !spotNumber(canceled.Executed, 0) ||
			!spotNumber(canceled.Quantity, qty) || !spotNumber(canceled.Stop, stop) {
			return fmt.Errorf("现货保护单撤单未确认或已有成交，需对账")
		}
	}
	return nil
}
