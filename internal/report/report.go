// Package report writes CSV, JSON, Markdown and a self-contained HTML page.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/metrics"
)

// Paths lists the artifacts written by Write.
type Paths struct {
	Directory   string
	EquityCSV   string
	TradesCSV   string
	OrdersCSV   string
	MetricsJSON string
	RunJSON     string
	Markdown    string
	HTML        string
}

// RunMeta is a small index file describing a saved run. The dashboard reads it
// to list past runs without parsing every report.
type RunMeta struct {
	Name       string          `json:"name"`
	Symbol     string          `json:"symbol"`
	Strategy   string          `json:"strategy"`
	DataSource string          `json:"data_source"`
	Start      string          `json:"start"`
	End        string          `json:"end"`
	Bars       int             `json:"bars"`
	Generated  string          `json:"generated"`
	Metrics    metrics.Metrics `json:"metrics"`
}

// EquityPoint is one sampled point of the chart payload.
type EquityPoint struct {
	T        string  `json:"t"`
	Equity   float64 `json:"equity"`
	Drawdown float64 `json:"drawdown"`
}

type templateData struct {
	Result      engine.Result
	Metrics     any
	Points      []EquityPoint
	PointsJS    template.JS
	Generated   string
	TotalReturn string
	FinalEquity string
	MaxDrawdown string
	Sharpe      string
	WinRate     string
	Positive    bool
}

// Write persists every artifact of a run into dir/<run-name>/.
func Write(result engine.Result, outputDir, runName string) (Paths, error) {
	if outputDir == "" {
		outputDir = "reports"
	}
	if runName == "" {
		runName = fmt.Sprintf("%s-%s-%s",
			lower(result.Symbol), result.Strategy, time.Now().Format("20060102-150405"))
	}
	dir := filepath.Join(outputDir, runName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Paths{}, fmt.Errorf("create report dir: %w", err)
	}

	paths := Paths{
		Directory:   dir,
		EquityCSV:   filepath.Join(dir, "equity.csv"),
		TradesCSV:   filepath.Join(dir, "trades.csv"),
		OrdersCSV:   filepath.Join(dir, "orders.csv"),
		MetricsJSON: filepath.Join(dir, "metrics.json"),
		RunJSON:     filepath.Join(dir, "run.json"),
		Markdown:    filepath.Join(dir, "report.md"),
		HTML:        filepath.Join(dir, "report.html"),
	}

	if err := writeEquity(paths.EquityCSV, result); err != nil {
		return Paths{}, err
	}
	if err := writeTrades(paths.TradesCSV, result); err != nil {
		return Paths{}, err
	}
	if err := writeOrders(paths.OrdersCSV, result); err != nil {
		return Paths{}, err
	}

	metricsJSON, err := json.MarshalIndent(result.Metrics, "", "  ")
	if err != nil {
		return Paths{}, fmt.Errorf("encode metrics: %w", err)
	}
	if err := os.WriteFile(paths.MetricsJSON, metricsJSON, 0o644); err != nil {
		return Paths{}, err
	}
	if err := os.WriteFile(paths.Markdown, []byte(markdown(result)), 0o644); err != nil {
		return Paths{}, err
	}
	if err := writeRunMeta(paths.RunJSON, runName, result); err != nil {
		return Paths{}, err
	}
	if err := writeHTML(paths.HTML, result); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func writeRunMeta(path, name string, result engine.Result) error {
	meta := RunMeta{
		Name:       name,
		Symbol:     result.Symbol,
		Strategy:   result.Strategy,
		DataSource: result.DataSource,
		Start:      result.Start.Format("2006-01-02"),
		End:        result.End.Format("2006-01-02"),
		Bars:       result.Bars,
		Generated:  time.Now().Format("2006-01-02 15:04:05"),
		Metrics:    result.Metrics,
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run metadata: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ListRuns reads every run.json under outputDir, newest first. Missing or
// unreadable entries are skipped: a half-written run must not break the list.
func ListRuns(outputDir string) ([]RunMeta, error) {
	if outputDir == "" {
		outputDir = "reports"
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read report directory %s: %w", outputDir, err)
	}

	runs := make([]RunMeta, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(outputDir, entry.Name(), "run.json"))
		if err != nil {
			continue
		}
		var meta RunMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		if meta.Name == "" {
			meta.Name = entry.Name()
		}
		runs = append(runs, meta)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name > runs[j].Name })
	return runs, nil
}

func writeEquity(path string, result engine.Result) error {
	rows := [][]string{{"timestamp", "cash", "market_value", "equity", "realized_pnl", "open_positions"}}
	for _, p := range result.Equity {
		rows = append(rows, []string{
			p.Time.Format("2006-01-02"),
			num(p.Cash), num(p.MarketValue), num(p.Equity), num(p.RealizedPnL),
			strconv.Itoa(p.OpenPositions),
		})
	}
	return writeCSV(path, rows)
}

func writeTrades(path string, result engine.Result) error {
	rows := [][]string{{
		"entry_time", "exit_time", "side", "quantity", "entry_price",
		"exit_price", "gross_pnl", "commission", "pnl", "return_pct", "reason",
	}}
	for _, t := range result.Trades {
		rows = append(rows, []string{
			t.EntryTime.Format("2006-01-02"), t.ExitTime.Format("2006-01-02"),
			string(t.Side), num(t.Quantity), num(t.EntryPrice), num(t.ExitPrice),
			num(t.GrossPnL), num(t.Commission), num(t.PnL), num(t.ReturnPct), t.Reason,
		})
	}
	return writeCSV(path, rows)
}

func writeOrders(path string, result engine.Result) error {
	rows := [][]string{{
		"timestamp", "symbol", "side", "quantity", "price", "commission", "notional", "reason", "rejected",
	}}
	for _, f := range result.Orders {
		rows = append(rows, []string{
			f.Time.Format("2006-01-02"), f.Symbol, string(f.Side), num(f.Quantity),
			num(f.Price), num(f.Commission), num(f.Notional), f.Reason,
			strconv.FormatBool(f.Rejected),
		})
	}
	return writeCSV(path, rows)
}

func writeCSV(path string, rows [][]string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	if err := writer.WriteAll(rows); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return writer.Error()
}

func markdown(result engine.Result) string {
	m := result.Metrics
	var b []byte
	add := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}
	add("# 回测报告 - %s\n\n", result.Symbol)
	add("- 策略: `%s`\n", result.Strategy)
	add("- 数据源: `%s`\n", result.DataSource)
	add("- 区间: %s 至 %s（%d 根K线）\n", result.Start.Format("2006-01-02"), result.End.Format("2006-01-02"), result.Bars)
	add("- 生成时间: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	add("## 绩效指标\n\n| 指标 | 数值 |\n| --- | ---: |\n")
	add("| 期末权益 | %s |\n", s(m.FinalEquity, 2, ""))
	add("| 总收益率 | %s |\n", s(m.TotalReturnPct, 2, "%"))
	add("| 年化收益率 | %s |\n", s(m.AnnualReturnPct, 2, "%"))
	add("| 年化波动率 | %s |\n", s(m.AnnualVolatilityPct, 2, "%"))
	add("| 夏普比率 | %s |\n", s(m.Sharpe, 2, ""))
	add("| 索提诺比率 | %s |\n", s(m.Sortino, 2, ""))
	add("| 卡玛比率 | %s |\n", s(m.Calmar, 2, ""))
	add("| 最大回撤 | %s |\n", s(m.MaxDrawdownPct, 2, "%"))
	add("| 持仓暴露 | %s |\n", s(m.ExposurePct, 2, "%"))
	add("| 成交笔数 | %d |\n", m.NumTrades)
	add("| 胜率 | %s |\n", s(m.WinRatePct, 2, "%"))
	add("| 盈亏比 | %s |\n", s(m.ProfitFactor, 2, ""))
	add("| 单笔期望 | %s |\n", s(m.Expectancy, 2, ""))
	add("| 总费用 | %.2f |\n\n", m.TotalFees)

	add("## 风控事件\n\n")
	if len(result.RiskEvents) == 0 {
		add("无 —— 没有触发任何风控限制。\n")
	} else {
		for _, e := range result.RiskEvents {
			add("- `%s` %s: %s\n", e.Time.Format("2006-01-02 15:04:05"),
				riskTypeLabel(e.Type), reasonLabel(e.Reason))
		}
	}

	add("\n## 最近 10 笔成交\n\n")
	if len(result.Trades) == 0 {
		add("没有已平仓的交易。\n")
	} else {
		add("| 开仓日 | 平仓日 | 方向 | 数量 | 开仓价 | 平仓价 | 盈亏 | 离场原因 |\n")
		add("| --- | --- | --- | ---: | ---: | ---: | ---: | --- |\n")
		start := max(len(result.Trades)-10, 0)
		for _, t := range result.Trades[start:] {
			add("| %s | %s | %s | %.4f | %.2f | %.2f | %.2f | %s |\n",
				t.EntryTime.Format("2006-01-02"), t.ExitTime.Format("2006-01-02"),
				sideLabel(t.Side), t.Quantity, t.EntryPrice, t.ExitPrice, t.PnL, reasonLabel(t.Reason))
		}
	}
	add("\n## 免责声明\n\n")
	add("本报告是一次模拟回测的结果，成本与成交假设均做了简化，不构成任何投资建议；")
	add("历史表现不代表未来收益。\n")
	return string(b)
}

func writeHTML(path string, result engine.Result) error {
	points := sampleEquity(result, 260)
	encoded, err := json.Marshal(points)
	if err != nil {
		return fmt.Errorf("encode chart points: %w", err)
	}
	m := result.Metrics
	data := templateData{
		Result:      result,
		Metrics:     m,
		Points:      points,
		PointsJS:    template.JS(encoded),
		Generated:   time.Now().Format("2006-01-02 15:04:05"),
		TotalReturn: s(m.TotalReturnPct, 2, "%"),
		FinalEquity: s(m.FinalEquity, 2, ""),
		MaxDrawdown: s(m.MaxDrawdownPct, 2, "%"),
		Sharpe:      s(m.Sharpe, 2, ""),
		WinRate:     s(m.WinRatePct, 2, "%"),
		Positive:    m.TotalReturnPct != nil && *m.TotalReturnPct >= 0,
	}

	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"sideLabel":     sideLabel,
		"reasonLabel":   reasonLabel,
		"riskTypeLabel": riskTypeLabel,
	}).Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer file.Close()
	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	return nil
}

func sampleEquity(result engine.Result, limit int) []EquityPoint {
	if len(result.Equity) == 0 {
		return nil
	}
	step := len(result.Equity) / limit
	if step < 1 {
		step = 1
	}
	out := make([]EquityPoint, 0, limit+1)
	peak := result.Equity[0].Equity
	for i, p := range result.Equity {
		if p.Equity > peak {
			peak = p.Equity
		}
		if i%step != 0 && i != len(result.Equity)-1 {
			continue
		}
		dd := 0.0
		if peak != 0 {
			dd = p.Equity/peak - 1
		}
		out = append(out, EquityPoint{T: p.Time.Format("2006-01-02"), Equity: round2(p.Equity), Drawdown: dd})
	}
	return out
}

func num(v float64) string { return strconv.FormatFloat(round6(v), 'f', -1, 64) }

func s(v *float64, digits int, suffix string) string {
	if v == nil {
		return "n/a"
	}
	return strconv.FormatFloat(round(*v, digits), 'f', digits, 64) + suffix
}

func round(v float64, digits int) float64 {
	scale := 1.0
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	return float64(int64(v*scale+sign(v)*0.5)) / scale
}

func round2(v float64) float64 { return round(v, 2) }
func round6(v float64) float64 { return round(v, 6) }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
