package webui

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/live"
	"github.com/rdone44/trading-agent-go/internal/marketdata"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// cycleLogLimit caps the in-memory cycle log so a session left running for
// weeks cannot grow without bound. The console only ever shows the tail.
const cycleLogLimit = 500

// liveConfirmPhrase is what the UI must send to arm real orders. A real-money
// switch must never be a single accidental click.
const liveConfirmPhrase = "确认实盘"

// CycleRecord is one decision cycle as the console displays it.
type CycleRecord struct {
	Time     string  `json:"time"`
	Action   string  `json:"action"`
	Price    float64 `json:"price"`
	Equity   float64 `json:"equity"`
	Cash     float64 `json:"cash"`
	Position string  `json:"position"`
	Error    string  `json:"error,omitempty"`
}

// PositionView describes the open position, if any.
type PositionView struct {
	Open        bool    `json:"open"`
	Side        string  `json:"side"`
	Quantity    float64 `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	StopPrice   float64 `json:"stop_price"`
	TargetPrice float64 `json:"target_price"`
	OpenedAt    string  `json:"opened_at"`
	MarkPrice   float64 `json:"mark_price"`
	Unrealized  float64 `json:"unrealized"`
	ReturnPct   float64 `json:"return_pct"`
	// StopDistancePct and TargetDistancePct are how far the mark has to move to
	// reach each level. A live position's most useful number is "how much room
	// is left before the stop", which none of the absolute prices answer.
	StopDistancePct   float64 `json:"stop_distance_pct"`
	TargetDistancePct float64 `json:"target_distance_pct"`
	// Notional is quantity * mark, so the exposure is readable without mental
	// arithmetic against the cash figure.
	Notional float64 `json:"notional"`
}

// RiskView is the risk manager's live state: whether trading is still allowed
// and how much of each budget is spent. The engine enforces these every cycle
// but the console had no way to show them, so a halted session looked
// identical to a quiet one.
type RiskView struct {
	Halted            bool    `json:"halted"`
	HaltReason        string  `json:"halt_reason,omitempty"`
	EntriesBlockedDay bool    `json:"entries_blocked_day"`
	DayStartEquity    float64 `json:"day_start_equity"`
	DayPnL            float64 `json:"day_pnl"`
	DayReturnPct      float64 `json:"day_return_pct"`
	MaxDailyLossPct   float64 `json:"max_daily_loss_pct"`
	MaxDrawdownPct    float64 `json:"max_drawdown_pct"`
	DrawdownPct       float64 `json:"drawdown_pct"`
	StopLossPct       float64 `json:"stop_loss_pct"`
	TakeProfitPct     float64 `json:"take_profit_pct"`
	OrderUncertain    bool    `json:"order_uncertain"`
}

// SessionStatus is the JSON the console polls.
type SessionStatus struct {
	Running  bool   `json:"running"`
	Mode     string `json:"mode"`  // paper | live
	Venue    string `json:"venue"` // spot | futures
	Leverage int    `json:"leverage"`
	Symbol   string `json:"symbol"`
	Strategy string `json:"strategy"`
	Interval int    `json:"interval_seconds"`

	StartedAt  string  `json:"started_at,omitempty"`
	StoppedAt  string  `json:"stopped_at,omitempty"`
	Cycles     int     `json:"cycles"`
	LastTick   string  `json:"last_tick,omitempty"`
	LastPrice  float64 `json:"last_price"`
	LastAction string  `json:"last_action"`
	LastError  string  `json:"last_error,omitempty"`

	Equity         float64 `json:"equity"`
	Cash           float64 `json:"cash"`
	InitialCash    float64 `json:"initial_cash"`
	TotalReturnPct float64 `json:"total_return_pct"`
	PeakEquity     float64 `json:"peak_equity"`

	Position         PositionView         `json:"position"`
	Risk             RiskView             `json:"risk"`
	Log              []CycleRecord        `json:"log"`
	Trades           []TradeView          `json:"trades"`
	Orders           []ExecutionView      `json:"orders"`
	Settings         *StartSessionRequest `json:"settings,omitempty"`
	ProtectionActive bool                 `json:"protection_active"`
	Wallet           float64              `json:"wallet"`
	MarginUsed       float64              `json:"margin_used"`
	StatePath        string               `json:"state_path,omitempty"`
}

type ExecutionView struct {
	Time          string  `json:"time"`
	Side          string  `json:"side"`
	Quantity      float64 `json:"quantity"`
	Price         float64 `json:"price"`
	Fee           float64 `json:"fee"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason"`
	OrderID       string  `json:"order_id"`
	ClientOrderID string  `json:"client_order_id"`
	Uncertain     bool    `json:"uncertain"`
}

// Session owns the one live trading loop the dashboard can run. The agent
// underneath is not safe for concurrent use, so every touch of it — a cycle,
// a status read — goes through this mutex.
type Session struct {
	mu     sync.Mutex
	runner *live.Runner
	cfg    config.Config
	// statePath and interval are kept even after a stop so the console can
	// show where the session was persisted.
	statePath string
	interval  time.Duration
	execute   bool

	cancel    context.CancelFunc
	done      chan struct{}
	running   bool
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	startedAt time.Time
	stoppedAt time.Time

	cycles     int
	lastTick   time.Time
	lastPrice  float64
	lastAction string
	lastError  string
	log        []CycleRecord

	// SeriesLoader and PriceLoader are injected by tests so the suite stays
	// offline; production leaves them nil and the runner uses Binance.
	SeriesLoader func(symbol string, days int, end time.Time) (model.Series, error)
	PriceLoader  func(symbol string) (float64, time.Time, error)
}

// NewSession returns an idle session. It holds no runner until Start.
func NewSession() *Session { return &Session{} }

// StartOptions describes a session start request.
type StartOptions struct {
	Config    config.Config
	Interval  time.Duration
	Execute   bool
	StatePath string
	// Confirm must equal liveConfirmPhrase when Execute is true.
	Confirm string
}

// Start builds a runner and begins polling in the background. It refuses to
// start twice, and refuses real trading without the confirmation phrase and
// the exchange keys in the environment.
func (s *Session) Start(opts StartOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("已有交易会话在运行，请先停止")
	}
	opts.Config.Agent.Symbol = marketdata.BinanceSymbol(opts.Config.Agent.Symbol)

	if opts.Execute {
		if opts.Confirm != liveConfirmPhrase {
			return fmt.Errorf("实盘交易需要在确认框输入「%s」", liveConfirmPhrase)
		}
		if !hasExchangeKeys() {
			return fmt.Errorf("实盘交易需要环境变量 BINANCE_API_KEY 与 BINANCE_SECRET_KEY")
		}
	}
	if opts.Interval < time.Second {
		opts.Interval = 60 * time.Second
	}
	if opts.StatePath == "" {
		opts.StatePath = "trade-state.json"
	}

	strat, err := strategy.New(opts.Config.Strategy.Name, opts.Config)
	if err != nil {
		return err
	}
	runner, err := live.New(opts.Config, strat, opts.Execute, opts.StatePath)
	if err != nil {
		return err
	}
	runner.SeriesLoader = s.SeriesLoader
	runner.PriceLoader = s.PriceLoader
	// Init reconciles against the exchange when executing; doing it before the
	// loop starts means a mismatched book fails the start instead of trading.
	if err := runner.Init(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.runner = runner
	s.cfg = opts.Config
	s.statePath = opts.StatePath
	s.interval = opts.Interval
	s.execute = opts.Execute
	s.cancel = cancel
	s.done = make(chan struct{})
	s.running = true
	s.stopping = false
	s.startedAt = time.Now()
	s.stoppedAt = time.Time{}
	s.cycles = 0
	s.lastError = ""
	s.lastAction = "启动"
	s.lastPrice = 0
	s.log = nil

	go s.loop(ctx)
	return nil
}

// loop polls until the context is cancelled. The first cycle runs immediately
// so the console shows something without waiting a full interval.
func (s *Session) loop(ctx context.Context) {
	defer close(s.done)
	s.cycleOnce()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cycleOnce()
		}
	}
}

// cycleOnce runs one decision cycle and records the outcome. Errors are
// captured rather than fatal: a transient network failure should not kill a
// session that is managing a position.
func (s *Session) cycleOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cycleLocked()
}

func (s *Session) cycleLocked() {
	if s.runner == nil {
		return
	}
	now := time.Now()
	result, err := s.runner.Cycle(now)
	s.cycles++
	s.lastTick = now

	agent := s.runner.Agent()
	if result.MarkPrice > 0 {
		s.lastPrice = result.MarkPrice
	}

	if err != nil {
		s.lastError = err.Error()
		s.appendLocked(CycleRecord{
			Time:     now.Format("15:04:05"),
			Action:   "错误",
			Error:    err.Error(),
			Equity:   s.currentEquityLocked(),
			Cash:     agent.Book.AvailableCash(),
			Price:    s.lastPrice,
			Position: positionSummary(agent.OpenTrade()),
		})
		return
	}

	s.lastError = ""
	s.lastAction = result.Action
	// A successful cycle always fetched a live price; the mark-to-market
	// equity the engine returned is the authoritative number for this tick.

	s.appendLocked(CycleRecord{
		Time:     now.Format("15:04:05"),
		Action:   result.Action,
		Price:    s.lastPrice,
		Equity:   result.Equity,
		Cash:     result.Cash,
		Position: positionSummary(result.Open),
	})
}

// Step runs one cycle on demand, which is what the 立即执行 button uses.
func (s *Session) Step() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.stopping || s.runner == nil {
		return fmt.Errorf("没有正在运行的交易会话")
	}
	s.cycleLocked()
	return nil
}

// Stop cancels the loop and waits for it to finish. It is safe to call on an
// idle session.
func (s *Session) Stop() error {
	s.mu.Lock()
	if s.stopping {
		done := s.stopDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopErr
	}
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	cancel := s.cancel
	done := s.done
	runner := s.runner
	s.stopping = true
	s.stopDone = make(chan struct{})
	s.mu.Unlock()

	cancel()
	<-done

	// Persist the final state so a restart resumes this session exactly.
	var saveErr error
	s.mu.Lock()
	if runner != nil {
		saveErr = runner.Save()
	}

	s.running = false
	s.stopping = false
	s.stoppedAt = time.Now()
	s.cancel = nil
	// Stopping does not liquidate: carry the cash and open position through so
	// the log row cannot be misread as a flat book.
	record := CycleRecord{
		Time:   s.stoppedAt.Format("15:04:05"),
		Action: "已停止",
		Equity: s.currentEquityLocked(),
		Price:  s.lastPrice,
	}
	if s.runner != nil {
		agent := s.runner.Agent()
		record.Cash = agent.Book.AvailableCash()
		record.Position = positionSummary(agent.OpenTrade())
	}
	s.appendLocked(record)
	s.stopErr = saveErr
	close(s.stopDone)
	s.mu.Unlock()

	if saveErr != nil {
		return fmt.Errorf("保存会话状态失败: %w", saveErr)
	}
	return nil
}

// Running reports whether a loop is active.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Status builds the JSON view the console polls.
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := SessionStatus{
		Running:     s.running,
		Mode:        "paper",
		Venue:       "spot",
		Symbol:      s.cfg.Agent.Symbol,
		Strategy:    s.cfg.Strategy.Name,
		Interval:    int(s.interval.Seconds()),
		Cycles:      s.cycles,
		LastPrice:   s.lastPrice,
		LastAction:  s.lastAction,
		LastError:   s.lastError,
		InitialCash: s.cfg.Risk.InitialCash,
		Leverage:    s.cfg.Risk.Leverage,
		Log:         append([]CycleRecord(nil), s.log...),
		StatePath:   s.statePath,
	}
	if status.Interval == 0 {
		status.Interval = 60
	}
	if status.Symbol == "" {
		status.Symbol = "—"
	}
	if s.execute {
		status.Mode = "live"
	}
	if s.cfg.Live.Futures {
		status.Venue = "futures"
	}
	if !s.startedAt.IsZero() {
		status.StartedAt = s.startedAt.Format(time.RFC3339)
	}
	if !s.stoppedAt.IsZero() {
		status.StoppedAt = s.stoppedAt.Format(time.RFC3339)
	}
	if !s.lastTick.IsZero() {
		status.LastTick = s.lastTick.Format(time.RFC3339)
	}

	if s.runner != nil {
		agent := s.runner.Agent()
		status.Equity = s.currentEquityLocked()
		status.Cash = agent.Book.AvailableCash()
		status.Wallet = agent.Book.Cash
		status.MarginUsed = agent.Book.MarginUsed()
		status.InitialCash = agent.Book.InitialCash
		status.PeakEquity = agent.PeakEquity
		status.Position = s.positionLocked()
		status.Risk = s.riskLocked(status.Equity)
		status.Trades = tradeViews(agent.Trades())
		if status.InitialCash > 0 {
			status.TotalReturnPct = (status.Equity/status.InitialCash - 1) * 100
		}
		status.ProtectionActive = s.running && !s.stopping && !status.Risk.OrderUncertain
		status.Settings = settingsView(s.cfg, status.Interval, s.execute)
		fills := agent.Broker.Fills()
		if len(fills) > 200 {
			fills = fills[len(fills)-200:]
		}
		for _, f := range fills {
			status.Orders = append(status.Orders, ExecutionView{Time: f.Time.Format(time.RFC3339), Side: string(f.Side), Quantity: f.Quantity, Price: f.Price, Fee: f.Commission, Status: f.Status, Reason: f.Reason, OrderID: f.OrderID, ClientOrderID: f.ClientOrderID, Uncertain: f.Uncertain})
		}
	}
	return status
}

// riskLocked reports how much of each risk budget is spent. Equity is passed
// in because the caller has already marked the book to the last price.
func (s *Session) riskLocked(equity float64) RiskView {
	if s.runner == nil {
		return RiskView{}
	}
	agent := s.runner.Agent()
	snapshot := agent.Risk.Snapshot()

	view := RiskView{
		Halted:            snapshot.Halted,
		HaltReason:        snapshot.HaltReason,
		EntriesBlockedDay: snapshot.EntriesBlockedDay,
		DayStartEquity:    snapshot.DayStartEquity,
		OrderUncertain:    snapshot.OrderUncertain,
	}
	// A percentage limit the user left blank is "no limit"; report 0 so the
	// UI can tell the difference between "0% allowed" and "unset".
	if pct := s.cfg.Risk.MaxDailyLossPct; pct != nil {
		view.MaxDailyLossPct = *pct * 100
	}
	if pct := s.cfg.Risk.MaxDrawdownPct; pct != nil {
		view.MaxDrawdownPct = *pct * 100
	}
	if pct := s.cfg.Risk.StopLossPct; pct != nil {
		view.StopLossPct = *pct * 100
	}
	if pct := s.cfg.Risk.TakeProfitPct; pct != nil {
		view.TakeProfitPct = *pct * 100
	}

	if snapshot.DayStartEquity > 0 {
		view.DayPnL = equity - snapshot.DayStartEquity
		view.DayReturnPct = (equity/snapshot.DayStartEquity - 1) * 100
	}
	if agent.PeakEquity > 0 {
		view.DrawdownPct = (equity/agent.PeakEquity - 1) * 100
	}
	return view
}

// currentEquityLocked marks the book against the last known price.
func (s *Session) currentEquityLocked() float64 {
	if s.runner == nil {
		return 0
	}
	agent := s.runner.Agent()
	prices := map[string]float64{}
	if s.lastPrice > 0 {
		prices[agent.Symbol] = s.lastPrice
	}
	return agent.Book.Equity(prices)
}

// positionLocked renders the open position with its mark-to-market value.
func (s *Session) positionLocked() PositionView {
	if s.runner == nil {
		return PositionView{}
	}
	agent := s.runner.Agent()
	pos := agent.Book.Position(agent.Symbol)
	if !pos.IsOpen() {
		return PositionView{}
	}

	view := PositionView{
		Open:       true,
		Side:       sideFromQuantity(pos.Quantity),
		Quantity:   pos.Quantity,
		EntryPrice: pos.AvgPrice,
		MarkPrice:  s.lastPrice,
	}
	if !math.IsNaN(pos.StopPrice) {
		view.StopPrice = pos.StopPrice
	}
	if !math.IsNaN(pos.TakeProfitPrice) {
		view.TargetPrice = pos.TakeProfitPrice
	}
	if !pos.OpenedAt.IsZero() {
		view.OpenedAt = pos.OpenedAt.Format(time.RFC3339)
	}
	if s.lastPrice > 0 && pos.AvgPrice > 0 {
		view.Unrealized = pos.Unrealized(s.lastPrice)
		direction := 1.0
		if pos.Quantity < 0 {
			direction = -1.0
		}
		view.ReturnPct = direction * (s.lastPrice/pos.AvgPrice - 1) * 100
		view.Notional = math.Abs(pos.Quantity) * s.lastPrice
		// Distances are signed the same way as the position: a long whose stop
		// sits below the mark shows a positive percentage of room left.
		if !math.IsNaN(pos.StopPrice) && pos.StopPrice > 0 {
			view.StopDistancePct = direction * (1 - pos.StopPrice/s.lastPrice) * 100
		}
		if !math.IsNaN(pos.TakeProfitPrice) && pos.TakeProfitPrice > 0 {
			view.TargetDistancePct = direction * (pos.TakeProfitPrice/s.lastPrice - 1) * 100
		}
	}
	return view
}

// appendLocked pushes a record, dropping the oldest once the cap is reached.
func (s *Session) appendLocked(record CycleRecord) {
	s.log = append(s.log, record)
	if len(s.log) > cycleLogLimit {
		s.log = s.log[len(s.log)-cycleLogLimit:]
	}
}

// positionSummary is the one-line position text in a cycle row.
func positionSummary(open *engine.OpenTrade) string {
	if open == nil {
		return "空仓"
	}
	return fmt.Sprintf("%s %.6f @ %.2f", sideLabel(string(open.Side)), open.Quantity, open.EntryPrice)
}

func sideLabel(side string) string {
	if side == "sell" {
		return "卖出"
	}
	return "买入"
}

// sideFromQuantity derives the direction from the signed quantity: the book
// stores a short as a negative position, matching the exchange's convention.
func sideFromQuantity(quantity float64) string {
	if quantity < 0 {
		return "sell"
	}
	return "buy"
}

// hasExchangeKeys reports whether the Binance credentials are in the
// environment. Keys are never read from the config file.
func hasExchangeKeys() bool {
	return os.Getenv("BINANCE_API_KEY") != "" && os.Getenv("BINANCE_SECRET_KEY") != ""
}
