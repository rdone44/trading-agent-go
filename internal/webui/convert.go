package webui

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/portfolio"
	"github.com/huijun/trading-agent-go/internal/risk"
)

// sampleEquity thins the equity curve for the chart while always keeping the
// first and last point, so the endpoints are exact.
func sampleEquity(curve []portfolio.EquityPoint, limit int) []EquityPoint {
	if len(curve) == 0 {
		return nil
	}
	step := len(curve) / limit
	if step < 1 {
		step = 1
	}
	peak := curve[0].Equity
	out := make([]EquityPoint, 0, limit+1)
	for i, p := range curve {
		if p.Equity > peak {
			peak = p.Equity
		}
		if i%step != 0 && i != len(curve)-1 {
			continue
		}
		drawdown := 0.0
		if peak != 0 {
			drawdown = p.Equity/peak - 1
		}
		out = append(out, EquityPoint{
			T:        p.Time.Format("2006-01-02"),
			Equity:   p.Equity,
			Drawdown: drawdown,
			Cash:     p.Cash,
		})
	}
	return out
}

func tradeViews(trades []engine.Trade) []TradeView {
	out := make([]TradeView, 0, len(trades))
	for _, t := range trades {
		out = append(out, TradeView{
			EntryTime:  t.EntryTime.Format("2006-01-02"),
			ExitTime:   t.ExitTime.Format("2006-01-02"),
			Side:       string(t.Side),
			Quantity:   t.Quantity,
			EntryPrice: t.EntryPrice,
			ExitPrice:  t.ExitPrice,
			PnL:        t.PnL,
			ReturnPct:  t.ReturnPct,
			Reason:     t.Reason,
		})
	}
	return out
}

func orderViews(orders []broker.Fill) []OrderView {
	out := make([]OrderView, 0, len(orders))
	for _, f := range orders {
		out = append(out, OrderView{
			Time:     f.Time.Format("2006-01-02"),
			Side:     string(f.Side),
			Quantity: f.Quantity,
			Price:    f.Price,
			Reason:   f.Reason,
			Rejected: f.Rejected,
		})
	}
	return out
}

func riskEventViews(events []risk.Event) []RiskEventView {
	out := make([]RiskEventView, 0, len(events))
	for _, e := range events {
		out = append(out, RiskEventView{
			Time:   e.Time.Format("2006-01-02"),
			Type:   e.Type,
			Reason: e.Reason,
		})
	}
	return out
}

// ------------------------------------------------------------------- readers

func readEquityCSV(path string) ([]EquityPoint, error) {
	rows, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	out := make([]EquityPoint, 0, len(rows))
	peak := 0.0
	for _, row := range rows {
		equity, err := parseNumber(row["equity"])
		if err != nil {
			continue
		}
		// The CSV has no peak column, so track the running peak as we read.
		if equity > peak {
			peak = equity
		}
		drawdown := 0.0
		if peak != 0 {
			drawdown = equity/peak - 1
		}
		cash, _ := parseNumber(row["cash"])
		out = append(out, EquityPoint{T: row["timestamp"], Equity: equity, Drawdown: drawdown, Cash: cash})
	}
	return out, nil
}

func readTradesCSV(path string) ([]TradeView, error) {
	rows, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	out := make([]TradeView, 0, len(rows))
	for _, row := range rows {
		pnl, _ := parseNumber(row["pnl"])
		quantity, _ := parseNumber(row["quantity"])
		entry, _ := parseNumber(row["entry_price"])
		exit, _ := parseNumber(row["exit_price"])
		returnPct, _ := parseNumber(row["return_pct"])
		out = append(out, TradeView{
			EntryTime:  row["entry_time"],
			ExitTime:   row["exit_time"],
			Side:       row["side"],
			Quantity:   quantity,
			EntryPrice: entry,
			ExitPrice:  exit,
			PnL:        pnl,
			ReturnPct:  returnPct,
			Reason:     row["reason"],
		})
	}
	return out, nil
}

func readOrdersCSV(path string) ([]OrderView, error) {
	rows, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, 0, len(rows))
	for _, row := range rows {
		quantity, _ := parseNumber(row["quantity"])
		price, _ := parseNumber(row["price"])
		out = append(out, OrderView{
			Time:     row["timestamp"],
			Side:     row["side"],
			Quantity: quantity,
			Price:    price,
			Reason:   row["reason"],
			Rejected: strings.EqualFold(row["rejected"], "true"),
		})
	}
	return out, nil
}

// readCSV reads a file with a header row into a slice of column->value maps.
func readCSV(path string) ([]map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header of %s: %w", path, err)
	}
	for i, name := range header {
		header[i] = strings.TrimSpace(name)
	}

	rows := []map[string]string{}
	for {
		record, err := reader.Read()
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(record) {
				row[name] = strings.TrimSpace(record[i])
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseNumber(text string) (float64, error) {
	if text == "" {
		return 0, fmt.Errorf("empty number")
	}
	return strconv.ParseFloat(text, 64)
}
