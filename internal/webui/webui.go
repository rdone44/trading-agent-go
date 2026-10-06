// Package webui serves the interactive dashboard: a config form, a chart, a
// trade blotter and a run history, all backed by the same engine the CLI uses.
//
// Everything is embedded in the binary, so the UI works offline and there is
// no build step, no CDN and no JavaScript framework.
package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/marketdata"
	"github.com/huijun/trading-agent-go/internal/metrics"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/report"
	"github.com/huijun/trading-agent-go/internal/strategy"
)

//go:embed static/*
var staticFiles embed.FS

// Server hosts the dashboard.
type Server struct {
	Config    config.Config
	OutputDir string
	// SeriesLoader loads market data for a run. It defaults to the public
	// Binance endpoint; tests inject an offline loader (internal/testfx) so the
	// suite never touches the network.
	SeriesLoader func(symbol string, days int, end time.Time) (model.Series, error)
}

// New builds a server from the effective configuration.
func New(cfg config.Config) *Server {
	output := cfg.Backtest.OutputDir
	if output == "" {
		output = "reports"
	}
	return &Server{
		Config:    cfg,
		OutputDir: output,
		SeriesLoader: func(symbol string, days int, end time.Time) (model.Series, error) {
			return marketdata.Binance(symbol, days, end)
		},
	}
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Only possible if the embed pattern is wrong, which is a build error.
		panic(err)
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(assets))))
	// Saved reports live outside the embedded assets, so serve them explicitly.
	// http.Dir plus the traversal guard below keeps requests inside outputDir.
	mux.Handle("/runs/", noCache(http.StripPrefix("/runs/", http.FileServer(http.Dir(s.OutputDir)))))
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/strategies", s.handleStrategies)
	mux.HandleFunc("/api/backtest", s.handleBacktest)
	mux.HandleFunc("/api/runs", s.handleRuns)
	mux.HandleFunc("/api/run", s.handleRunDetail)
	return logRequests(mux)
}

// noCache forces revalidation of the embedded assets. They have no
// Last-Modified time (embed.FS reports a zero mod time), so without this a
// browser may keep serving a stale app.css after the binary is rebuilt.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// Listen starts the dashboard and blocks.
func (s *Server) Listen(addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("dashboard: http://%s\n", displayAddr(addr))
	return server.ListenAndServe()
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fmt.Printf("  %-6s %-22s %s\n", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
		}
	})
}

// ------------------------------------------------------------------ requests

// BacktestRequest is the JSON body accepted by /api/backtest.
type BacktestRequest struct {
	Symbol      string             `json:"symbol"`
	Strategy    string             `json:"strategy"`
	Params      map[string]float64 `json:"params"`
	Days        int                `json:"days"`
	InitialCash float64            `json:"initial_cash"`
	WarmupBars  int                `json:"warmup_bars"`
	Risk        *RiskOverrides     `json:"risk"`
}

// RiskOverrides lets the UI override the config defaults per run.
type RiskOverrides struct {
	MaxPositionPct     *float64 `json:"max_position_pct"`
	MaxRiskPerTradePct *float64 `json:"max_risk_per_trade_pct"`
	StopLossPct        *float64 `json:"stop_loss_pct"`
	TakeProfitPct      *float64 `json:"take_profit_pct"`
	MaxDrawdownPct     *float64 `json:"max_drawdown_pct"`
	MaxDailyLossPct    *float64 `json:"max_daily_loss_pct"`
	AllowShort         *bool    `json:"allow_short"`
	CommissionBps      *float64 `json:"commission_bps"`
	SlippageBps        *float64 `json:"slippage_bps"`
}

// BacktestResponse is what the dashboard renders after a run.
type BacktestResponse struct {
	Symbol     string          `json:"symbol"`
	Strategy   string          `json:"strategy"`
	DataSource string          `json:"data_source"`
	Start      string          `json:"start"`
	End        string          `json:"end"`
	Bars       int             `json:"bars"`
	Metrics    metrics.Metrics `json:"metrics"`
	Equity     []EquityPoint   `json:"equity"`
	Price      []PricePoint    `json:"price"`
	Trades     []TradeView     `json:"trades"`
	Orders     []OrderView     `json:"orders"`
	RiskEvents []RiskEventView `json:"risk_events"`
	RunName    string          `json:"run_name"`
	ReportURL  string          `json:"report_url"`
	Config     map[string]any  `json:"config"`
}

// EquityPoint is one sampled point of the equity and drawdown curves.
type EquityPoint struct {
	T        string  `json:"t"`
	Equity   float64 `json:"equity"`
	Drawdown float64 `json:"drawdown"`
	Cash     float64 `json:"cash"`
}

// PricePoint is one sampled close, used to draw the price context chart.
type PricePoint struct {
	T     string  `json:"t"`
	Close float64 `json:"close"`
}

// TradeView is a closed round trip formatted for the blotter.
type TradeView struct {
	EntryTime  string  `json:"entry_time"`
	ExitTime   string  `json:"exit_time"`
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	PnL        float64 `json:"pnl"`
	ReturnPct  float64 `json:"return_pct"`
	Reason     string  `json:"reason"`
}

// OrderView is one broker fill.
type OrderView struct {
	Time     string  `json:"time"`
	Side     string  `json:"side"`
	Quantity float64 `json:"quantity"`
	Price    float64 `json:"price"`
	Reason   string  `json:"reason"`
	Rejected bool    `json:"rejected"`
}

// RiskEventView is one breached limit.
type RiskEventView struct {
	Time   string `json:"time"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// ------------------------------------------------------------------ handlers

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config
	writeJSON(w, http.StatusOK, map[string]any{
		"symbol":       cfg.Agent.Symbol,
		"strategy":     cfg.Strategy.Name,
		"days":         cfg.Agent.HistoryDays,
		"initial_cash": cfg.Risk.InitialCash,
		"warmup_bars":  cfg.Backtest.WarmupBars,
		"output_dir":   s.OutputDir,
		"risk": map[string]any{
			"max_position_pct":       cfg.Risk.MaxPositionPct,
			"max_risk_per_trade_pct": cfg.Risk.MaxRiskPerTradePct,
			"stop_loss_pct":          cfg.Risk.StopLossPct,
			"take_profit_pct":        cfg.Risk.TakeProfitPct,
			"max_drawdown_pct":       cfg.Risk.MaxDrawdownPct,
			"max_daily_loss_pct":     cfg.Risk.MaxDailyLossPct,
			"allow_short":            cfg.Risk.AllowShort,
			"commission_bps":         cfg.Execution.CommissionBps,
			"slippage_bps":           cfg.Execution.SlippageBps,
		},
		"strategies": strategy.Specs(),
	})
}

func (s *Server) handleStrategies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, strategy.Specs())
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := report.ListRuns(s.OutputDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("缺少回测名称参数"))
		return
	}
	// Guard against path traversal: a run name is a single directory segment.
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		writeError(w, http.StatusBadRequest, fmt.Errorf("回测名称不合法"))
		return
	}

	dir := filepath.Join(s.OutputDir, name)
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("找不到回测 %q", name))
		return
	}
	var meta report.RunMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The equity curve and blotter are re-read from the CSVs the run wrote.
	equity, err := readEquityCSV(filepath.Join(dir, "equity.csv"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	trades, err := readTradesCSV(filepath.Join(dir, "trades.csv"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	orders, err := readOrdersCSV(filepath.Join(dir, "orders.csv"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_name":    meta.Name,
		"symbol":      meta.Symbol,
		"strategy":    meta.Strategy,
		"data_source": meta.DataSource,
		"start":       meta.Start,
		"end":         meta.End,
		"bars":        meta.Bars,
		"metrics":     meta.Metrics,
		"equity":      equity,
		"trades":      trades,
		"orders":      orders,
		"report_url":  "/runs/" + name + "/report.html",
	})
}

func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var req BacktestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}

	result, runName, err := s.Run(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	response := BacktestResponse{
		Symbol:     result.Symbol,
		Strategy:   result.Strategy,
		DataSource: result.DataSource,
		Start:      result.Start.Format("2006-01-02"),
		End:        result.End.Format("2006-01-02"),
		Bars:       result.Bars,
		Metrics:    result.Metrics,
		Equity:     sampleEquity(result.Equity, 400),
		Trades:     tradeViews(result.Trades),
		Orders:     orderViews(result.Orders),
		RiskEvents: riskEventViews(result.RiskEvents),
		RunName:    runName,
		ReportURL:  "/runs/" + runName + "/report.html",
	}
	writeJSON(w, http.StatusOK, response)
}

// Run executes a backtest described by a request and persists its report.
// It is shared by the HTTP handler and any future caller (e.g. a scheduler).
func (s *Server) Run(req BacktestRequest) (engine.Result, string, error) {
	cfg := s.Config
	if req.Symbol != "" {
		cfg.Agent.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
	}
	if req.Strategy != "" {
		cfg.Strategy.Name = req.Strategy
	}
	if req.Params != nil {
		cfg.Strategy.Params = req.Params
	}
	if req.Days > 0 {
		cfg.Agent.HistoryDays = req.Days
	}
	if req.InitialCash > 0 {
		cfg.Risk.InitialCash = req.InitialCash
	}
	if req.WarmupBars > 0 {
		cfg.Backtest.WarmupBars = req.WarmupBars
	}
	if req.Risk != nil {
		applyRiskOverrides(&cfg, *req.Risk)
	}

	series, err := s.SeriesLoader(cfg.Agent.Symbol, cfg.Agent.HistoryDays, time.Now().UTC())
	if err != nil {
		return engine.Result{}, "", err
	}
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		return engine.Result{}, "", err
	}
	result, err := engine.New(cfg, strat).RunBacktest(series)
	if err != nil {
		return engine.Result{}, "", err
	}

	runName := fmt.Sprintf("%s-%s-%s",
		strings.ToLower(result.Symbol), strings.Split(result.Strategy, "(")[0],
		time.Now().Format("20060102-150405"))
	if _, err := report.Write(result, s.OutputDir, runName); err != nil {
		return engine.Result{}, "", err
	}
	return result, runName, nil
}

func applyRiskOverrides(cfg *config.Config, o RiskOverrides) {
	if o.MaxPositionPct != nil {
		cfg.Risk.MaxPositionPct = *o.MaxPositionPct
	}
	if o.MaxRiskPerTradePct != nil {
		cfg.Risk.MaxRiskPerTradePct = *o.MaxRiskPerTradePct
	}
	if o.StopLossPct != nil {
		cfg.Risk.StopLossPct = o.StopLossPct
	}
	if o.TakeProfitPct != nil {
		cfg.Risk.TakeProfitPct = o.TakeProfitPct
	}
	if o.MaxDrawdownPct != nil {
		cfg.Risk.MaxDrawdownPct = o.MaxDrawdownPct
	}
	if o.MaxDailyLossPct != nil {
		cfg.Risk.MaxDailyLossPct = o.MaxDailyLossPct
	}
	if o.AllowShort != nil {
		cfg.Risk.AllowShort = *o.AllowShort
	}
	if o.CommissionBps != nil {
		cfg.Execution.CommissionBps = *o.CommissionBps
	}
	if o.SlippageBps != nil {
		cfg.Execution.SlippageBps = *o.SlippageBps
	}
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		fmt.Printf("  encode response: %v\n", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
