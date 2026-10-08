// Package webui serves the interactive dashboard: a config form, a chart, a
// trade blotter and a run history, all backed by the same engine the CLI uses.
//
// Everything is embedded in the binary, so the UI works offline and there is
// no build step, no CDN and no JavaScript framework.
package webui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rdone44/trading-agent-go/internal/auth"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	livesession "github.com/rdone44/trading-agent-go/internal/live/session"
	"github.com/rdone44/trading-agent-go/internal/llm"
	"github.com/rdone44/trading-agent-go/internal/marketdata"
	"github.com/rdone44/trading-agent-go/internal/metrics"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/report"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/tune"
)

//go:embed static/*
var staticFiles embed.FS

// Server hosts the dashboard.
type Server struct {
	Config    config.Config
	OutputDir string
	// Desktop marks the embedded desktop build. The UI reads it from
	// /api/config and shows the "退出" control only when it is true.
	Desktop bool
	// Cancel, when non-nil, is invoked by POST /api/shutdown. The desktop
	// shell wires it to the context Serve runs under. The server build leaves
	// it nil, so a dashboard reachable over the network can never be stopped
	// by an HTTP request.
	Cancel context.CancelFunc
	// Log receives request and error lines. Defaults to os.Stdout; the
	// desktop build points it at a log file because a GUI process has no
	// console to print to.
	Log io.Writer
	// Token, when non-empty, is required on every request. The server build
	// uses it because the dashboard can be reached over a network; the local
	// desktop build leaves it empty so the window just opens.
	Token string
	// session owns the live trading loop the console starts and stops. It is
	// created lazily so a pure-backtest deployment never spins one up.
	session     *livesession.Session
	sessionOnce sync.Once
	// SeriesLoader loads market data for a run. It defaults to the public
	// Binance endpoint; tests inject an offline loader (internal/testfx) so the
	// suite never touches the network.
	SeriesLoader func(symbol string, days int, end time.Time) (model.Series, error)
	// TuneRunner runs the LLM parameter-tuning loop behind /api/tune. It
	// defaults to tune.Run; tests inject a stub so the suite never calls a
	// real model.
	TuneRunner   func(cfg config.Config, series model.Series, opts tune.Options) (tune.Report, error)
	MarketLoader func(symbol string, futures bool, interval string) (marketdata.MarketSnapshot, error)
	// SymbolList loads the venue's tradable pairs, liquidity-ranked. It
	// defaults to the public 24-hour ticker endpoint; tests inject a stub so
	// the /api/symbols handler stays offline.
	SymbolList func(venue string, limit int) ([]marketdata.SymbolInfo, error)
	// Auth, when set, enables account login: /api/auth/* routes plus a
	// session middleware on the credential-bearing routes. The credential
	// vault also carries each user's Binance and LLM keys, which the
	// session/backtest/tune handlers inject into the effective config.
	// Desktop and server builds without -users leave it nil, which keeps
	// every historical behaviour byte-for-byte intact.
	Auth        *auth.Service
	marketMu    sync.Mutex
	marketCache map[string]marketdata.MarketSnapshot
	// symbolsMu guards symbolsCache: the per-venue all-pair list, cached for
	// symbolsCacheTTL so the console cannot hammer the heavy ticker endpoint.
	symbolsMu    sync.Mutex
	symbolsCache map[string]cachedSymbols
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
		Log:       os.Stdout,
		SeriesLoader: func(symbol string, days int, end time.Time) (model.Series, error) {
			return marketdata.Binance(symbol, days, end)
		},
		TuneRunner: tune.Run,
		SymbolList: marketdata.AllSymbols,
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
	// Account routes are public by design: registration and login cannot
	// require the very session they create. The rest of /api is guarded
	// below only when accounts are enabled.
	mux.HandleFunc("/api/auth/register", s.handleAuthRegister)
	mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("/api/auth/me", s.handleAuthMe)
	mux.HandleFunc("/api/auth/credentials", s.handleAuthCredentials)
	protected := http.NewServeMux()
	protected.HandleFunc("/api/backtest", s.handleBacktest)
	protected.HandleFunc("/api/tune", s.handleTune)
	protected.HandleFunc("/api/runs", s.handleRuns)
	protected.HandleFunc("/api/run", s.handleRunDetail)
	// Trading-console routes: the live session, not a backtest.
	protected.HandleFunc("/api/session", s.handleSession)
	protected.HandleFunc("/api/session/start", s.handleSessionStart)
	protected.HandleFunc("/api/session/stop", s.handleSessionStop)
	protected.HandleFunc("/api/session/step", s.handleSessionStep)
	protected.HandleFunc("/api/market", s.handleMarket)
	protected.HandleFunc("/api/symbols", s.handleSymbols)
	mux.Handle("/api/", s.requireUserSession(protected))
	// /healthz is what a systemd unit, a container probe or a load balancer
	// polls; it touches no disk and no network, so it stays cheap.
	mux.HandleFunc("/healthz", s.handleHealth)
	if s.Cancel != nil {
		mux.HandleFunc("/api/shutdown", s.handleShutdown)
	}
	return s.logRequests(s.authenticate(mux))
}

// authCookie is the cookie a browser gets after presenting the token once in
// the query string, so the token does not have to stay in the address bar.
const authCookie = "ta_token"

// authenticate enforces the access token when one is configured. Three forms
// are accepted: an Authorization: Bearer header (scripts and curl), a cookie
// set by an earlier ?token= visit, or the ?token= query parameter itself,
// which is exchanged for the cookie and then stripped from the URL.
//
// With no token configured the middleware is a pass-through: that is the
// local desktop build, where the listener is bound to loopback anyway.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.Token == "" {
		return next
	}
	want := []byte(s.Token)
	match := func(got string) bool {
		// Constant time so a wrong token cannot be recovered byte by byte.
		return subtle.ConstantTimeCompare([]byte(got), want) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
			if match(strings.TrimPrefix(header, "Bearer ")) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if cookie, err := r.Cookie(authCookie); err == nil && match(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if query := r.URL.Query().Get("token"); query != "" && match(query) {
			http.SetCookie(w, &http.Cookie{
				Name:     authCookie,
				Value:    query,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			// Drop the token from the visible URL so it is not bookmarked or
			// left in the browser history as plain text.
			clean := *r.URL
			params := clean.Query()
			params.Del("token")
			clean.RawQuery = params.Encode()
			http.Redirect(w, r, clean.String(), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="trading-agent"`)
		writeError(w, http.StatusUnauthorized, fmt.Errorf("需要访问令牌：在地址后加 ?token=… 或使用 Authorization: Bearer"))
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// Session returns the lazily-created trading session.
func (s *Server) Session() *livesession.Session {
	s.sessionOnce.Do(func() { s.session = livesession.NewSession() })
	return s.session
}

// StartSessionRequest is the JSON body of /api/session/start.
type StartSessionRequest struct {
	BacktestRequest `json:",inline"`
	// IntervalSeconds is the poll period. The engine decides on closed daily
	// bars, so the default is a minute rather than seconds.
	IntervalSeconds int `json:"interval_seconds"`
	// Execute arms real orders. It additionally requires Confirm to equal the
	// confirmation phrase and the exchange keys to be in the environment.
	Execute bool   `json:"execute"`
	Confirm string `json:"confirm"`
	// Futures and Leverage select the perpetual venue.
	Futures  bool `json:"futures"`
	Leverage int  `json:"leverage"`
	// StatePath persists the session so a restart resumes the position.
	StatePath string `json:"state_path"`
}

// handleSession reports the current session state. The console polls it.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 GET 请求"))
		return
	}
	writeJSON(w, http.StatusOK, s.Session().Status())
}

// handleSessionStart begins the live trading loop.
func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var req StartSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	req.AuthUser = s.requestUsername(r)

	cfg := s.applyRequest(req.BacktestRequest)
	cfg.Agent.Symbol = marketdata.BinanceSymbol(cfg.Agent.Symbol)
	cfg.Live.Futures = req.Futures
	if req.Leverage < 0 || req.Leverage > 125 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("杠杆必须在 1–125 之间"))
		return
	}
	cfg.Risk.Leverage = max(req.Leverage, 1)
	if !req.Futures && cfg.Risk.Leverage > 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("现货不支持杠杆"))
		return
	}
	if !req.Futures && cfg.Risk.AllowShort && req.Execute {
		writeError(w, http.StatusBadRequest, fmt.Errorf("现货实盘不能做空"))
		return
	}
	// The live loop sizes positions against the same risk limits the form
	// shows, so the request's overrides apply here too.
	if req.Days > 0 {
		cfg.Live.LookbackDays = req.Days
	}
	cfg.Agent.HistoryDays = cfg.Live.LookbackDays
	if cfg.Agent.HistoryDays <= 0 {
		cfg.Agent.HistoryDays = 400
	}
	if cfg.Live.StateFile == "" {
		cfg.Live.StateFile = "trade-state.json"
	}
	if err := validateSessionConfig(cfg, req.IntervalSeconds); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	statePath := req.StatePath
	if statePath == "" {
		mode, venue := "paper", "spot"
		if req.Execute {
			mode = "live"
		}
		if req.Futures {
			venue = "futures"
		}
		statePath = filepath.Join(filepath.Dir(cfg.Live.StateFile), "sessions", mode+"-"+venue+"-"+cfg.Agent.Symbol+".json")
	}

	err := s.Session().Start(livesession.StartOptions{
		Config:    cfg,
		Interval:  time.Duration(req.IntervalSeconds) * time.Second,
		Execute:   req.Execute,
		StatePath: statePath,
		Confirm:   req.Confirm,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Session().Status())
}

// handleSessionStop ends the loop and persists the final state.
func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	if err := s.Session().Stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Session().Status())
}

// handleSessionStep runs one cycle immediately instead of waiting for the
// poll interval, which is what the 立即执行 button uses.
func (s *Server) handleSessionStep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	if err := s.Session().Step(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Session().Status())
}

// handleShutdown stops the process. It is registered only when the desktop
// shell set Cancel, so the control is unreachable from a networked server.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	cancel := s.Cancel
	// Let the response reach the browser before the listener goes away.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
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

// ServeListener serves on an already-bound listener and blocks until the
// context is cancelled or the listener fails. ready, when non-nil, is called
// with the actual address once the socket is open — which is how the desktop
// build learns the port it was given after asking for port 0.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener, ready func(addr string)) error {
	if ready != nil {
		ready(listener.Addr().String())
	}
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Shut down cleanly on cancellation (Ctrl+C, SIGTERM from systemd, or the
	// desktop window closing) so in-flight requests finish instead of being cut.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		_ = s.Session().Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()

	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		return nil
	}
	return err
}

// Serve binds addr and blocks until the context is cancelled.
func (s *Server) Serve(ctx context.Context, addr string, ready func(addr string)) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, listener, ready)
}

// Listen starts the dashboard on addr and blocks until the process is
// interrupted. It is the CLI entry point; the desktop shell uses Serve.
func (s *Server) Listen(addr string) error {
	fmt.Fprintf(s.logWriter(), "dashboard: http://%s\n", displayAddr(addr))
	return s.Serve(context.Background(), addr, nil)
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fmt.Fprintf(s.logWriter(), "  %-6s %-22s %s\n", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
		}
	})
}

func (s *Server) logWriter() io.Writer {
	if s.Log != nil {
		return s.Log
	}
	return os.Stdout
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
	// Review asks for an LLM post-mortem of the finished run. Degrades
	// silently (empty Review field) when no model is configured.
	Review bool `json:"review"`
	// AuthUser carries the session cookie's account name from the HTTP
	// handler into the config pipeline. json:"-" keeps the wire format
	// unchanged; an empty value means "no session / no injected
	// credentials".
	AuthUser string `json:"-"`
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
	// Review is the LLM post-mortem, present only when the request asked for
	// it and a model was available. ReviewUnavailable carries a short note
	// when the request asked for a review but the model could not be reached.
	Review            string `json:"review,omitempty"`
	ReviewUnavailable string `json:"review_unavailable,omitempty"`
}

// TuneRequest is the JSON body accepted by /api/tune: the same run description
// as BacktestRequest, plus the tuning options.
type TuneRequest struct {
	BacktestRequest `json:",inline"`
	Objective       string `json:"objective"`
	Rounds          int    `json:"rounds"`
	Stall           int    `json:"stall"`
	NoClamp         bool   `json:"no_clamp"`
	CVFolds         int    `json:"cv_folds"`
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
	view := map[string]any{
		"symbol":       cfg.Agent.Symbol,
		"strategy":     cfg.Strategy.Name,
		"days":         cfg.Agent.HistoryDays,
		"initial_cash": cfg.Risk.InitialCash,
		"warmup_bars":  cfg.Backtest.WarmupBars,
		"output_dir":   s.OutputDir,
		// desktop tells the page it is running inside the desktop edition, so
		// it shows the 退出 control and hides nothing else.
		"desktop": s.Desktop,
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
		"settings":   settingsView(cfg, cfg.Live.PollSeconds, false),
		"strategies": strategy.Specs(),
		"auth":       s.authStatusForRequest(r),
	}
	writeJSON(w, http.StatusOK, view)
}

// authStatusForRequest is the /api/config answer about accounts: whether
// login is enabled, who the session cookie belongs to (if anyone), and which
// credentials that account has stored. Secrets never appear in the payload.
func (s *Server) authStatusForRequest(r *http.Request) map[string]any {
	if s.Auth == nil {
		return map[string]any{"enabled": false}
	}
	view := s.authView(s.requestUsername(r))
	view["enabled"] = true
	return view
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
	req.AuthUser = s.requestUsername(r)

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
	// Optionally ask the model for a post-mortem. A missing key or a model
	// error is not fatal to the backtest: we surface it in a note instead.
	if req.Review {
		text, err := llm.Review(s.Config.LLM, engine.ReviewFacts(result, 5))
		if err != nil {
			response.ReviewUnavailable = err.Error()
		} else {
			response.Review = text
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// applyRequest overlays the request's overrides onto a copy of the server's
// base config. It is shared by Run (a single backtest) and handleTune (which
// needs the same effective config to run the baseline). When the request
// carries an authenticated account, that user's vault credentials are
// injected last, so stored keys win over any config-file value while the
// environment stays the historical fallback for CLI and desktop.
func (s *Server) applyRequest(req BacktestRequest) config.Config {
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
	s.applyUserCredentials(&cfg, req.AuthUser)
	return cfg
}

// handleTune runs the LLM parameter-tuning loop against the same effective
// config as /api/backtest and returns the round-by-round outcome. It shares
// tune.Run with the CLI, so the two surfaces behave identically. The loop is
// bounded (Rounds + the stall guard), so a slow model cannot hang the request.
func (s *Server) handleTune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("该接口只接受 POST 请求"))
		return
	}
	var req TuneRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法的 JSON: %w", err))
		return
	}
	req.AuthUser = s.requestUsername(r)
	if req.CVFolds < 0 || req.CVFolds > 32 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cv folds must be between 0 and 32"))
		return
	}
	if req.Objective == "" {
		req.Objective = "sharpe"
	}
	if req.Rounds < 0 {
		req.Rounds = 0
	}
	if req.Stall < 0 {
		req.Stall = 0
	}

	cfg := s.applyRequest(req.BacktestRequest)
	series, err := s.SeriesLoader(cfg.Agent.Symbol, cfg.Agent.HistoryDays, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	report, err := s.TuneRunner(cfg, series, tune.Options{
		Objective: req.Objective,
		Rounds:    req.Rounds,
		Stall:     req.Stall,
		NoClamp:   req.NoClamp,
		CVFolds:   req.CVFolds,
		Seed:      cfg.Strategy.Params,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// Run executes a backtest described by a request and persists its report.
// It is shared by the HTTP handler and any future caller (e.g. a scheduler).
func (s *Server) Run(req BacktestRequest) (engine.Result, string, error) {
	cfg := s.applyRequest(req)

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
