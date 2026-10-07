package broker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

type exchangeOrder struct {
	OrderID  int64               `json:"orderId"`
	Executed json.Number         `json:"executedQty"`
	Quote    json.Number         `json:"cummulativeQuoteQty"`
	AvgPrice json.Number         `json:"avgPrice"`
	Status   string              `json:"status"`
	Fills    []exchangeExecution `json:"fills"`
}

type exchangeExecution struct {
	Price      json.Number `json:"price"`
	Quantity   json.Number `json:"qty"`
	Commission json.Number `json:"commission"`
	Asset      string      `json:"commissionAsset"`
}

func newClientID(prefix string) string {
	var bytes [8]byte
	if len(prefix) > 19 {
		prefix = prefix[:19]
	}
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%s-%x", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(bytes[:])
}

func positive(n json.Number) float64 {
	v, _ := n.Float64()
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func (o exchangeOrder) detail() orderDetail {
	d := orderDetail{executed: positive(o.Executed), avgPrice: positive(o.AvgPrice), status: o.Status}
	if d.executed > 0 && positive(o.Quote) > 0 {
		d.avgPrice = positive(o.Quote) / d.executed
	}
	var qty, cost float64
	for _, f := range o.Fills {
		qty += positive(f.Quantity)
		cost += positive(f.Quantity) * positive(f.Price)
	}
	if qty > 0 {
		d.avgPrice = cost / qty
	}
	return d
}

func (o exchangeOrder) fees(base, quote string) (total, baseFee float64, assets map[string]float64) {
	assets = map[string]float64{}
	price := o.detail().avgPrice
	for _, f := range o.Fills {
		fee := positive(f.Commission)
		asset := strings.ToUpper(f.Asset)
		assets[asset] += fee
		switch asset {
		case strings.ToUpper(base):
			baseFee += fee
			total += fee * price
		case strings.ToUpper(quote):
			total += fee
		}
	}
	return
}

func orderUnknown(ts time.Time, symbol string, side Side, price float64, id, message string) Fill {
	return Fill{Time: ts, Symbol: symbol, Side: side, Price: price, ClientOrderID: id,
		Status: "unknown", Uncertain: true, Rejected: true, Reason: "成交状态待核对（" + id + "）：" + message}
}

func terminalOrder(status string) bool {
	switch status {
	case "FILLED", "CANCELED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED":
		return true
	}
	return false
}

func orderQuery(symbol, clientID string) url.Values {
	return url.Values{"symbol": {strings.ToUpper(symbol)}, "origClientOrderId": {clientID}}
}
