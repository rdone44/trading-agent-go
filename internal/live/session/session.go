// Package session owns the dashboard's live trading loop: the state machine
// that starts, steps and stops a single live runner and reports its status.
// It lives under internal/live so the webui package only adapts HTTP routing
// to it, without owning the session state (P2-1). Its view types carry JSON
// tags identical to the historical webui types, so the console wire format is
// unchanged.
package session

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
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
	// Reason is why the cycle did what it did: the model's own one-line
	// explanation on an LLM strategy, the protective exit's trigger, or the
	// failure detail when the action is a degradation such as ai_unavailable.
	// Without it the log says "AI 不可用" and leaves the user to guess whether
	// the key is wrong, the endpoint is unreachable or the answer did not
	// parse — which is exactly the question the log exists to answer.
	Reason string `json:"reason,omitempty"`
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
	Venue    string `json:"venue"` // always "futures": the perpetual venue
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

	// The AI's face: which model is deciding and its own one-line
	// explanation of the latest call. Empty for non-LLM strategies.
	AIModel  string `json:"ai_model,omitempty"`
	AIReason string `json:"ai_reason,omitempty"`
	// VetoEnabled mirrors cfg.LLM.VetoEnabled for the running session, so the
	// console can show whether a new entry would actually face a second
	// opinion instead of guessing from the form.
	VetoEnabled bool `json:"veto_enabled"`

	Equity         float64 `json:"equity"`
	Cash           float64 `json:"cash"`
	InitialCash    float64 `json:"initial_cash"`
	TotalReturnPct float64 `json:"total_return_pct"`
	PeakEquity     float64 `json:"peak_equity"`

	Position         PositionView              `json:"position"`
	Risk             RiskView                  `json:"risk"`
	Log              []CycleRecord             `json:"log"`
	Trades           []TradeView               `json:"trades"`
	Orders           []ExecutionView           `json:"orders"`
	Settings         *Settings                 `json:"settings,omitempty"`
	ProtectionActive bool                      `json:"protection_active"`
	Protection       broker.ProtectionSnapshot `json:"protection"`
	RecoveryRequired bool                      `json:"recovery_required"`
	Starting         bool                      `json:"starting"`
	Recovering       bool                      `json:"recovering"`
	Wallet           float64                   `json:"wallet"`
	MarginUsed       float64                   `json:"margin_used"`
	StatePath        string                    `json:"state_path,omitempty"`
	// LogPath is where the run log is persisted, and LogError is set when the
	// last append failed. The console shows both so "the history is empty"
	// can be told apart from "the history could not be written".
	LogPath  string `json:"log_path,omitempty"`
	LogError string `json:"log_error,omitempty"`
}

// ExecutionView is one fill as the console's order blotter displays it.
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

// TradeView is a closed round trip formatted for the blotter. Its JSON tags are
// identical to webui.TradeView so the console sees the same shape whether the
// rows came from a backtest or a live session.
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

// SessionRiskView is the risk-override block inside Settings. Its JSON is
// identical to webui.RiskOverrides, which settingsView used to fill.
type SessionRiskView struct {
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

// Settings is the session configuration echoed back in SessionStatus so the
// console can prefill its form. Its JSON is byte-identical to the historical
// webui StartSessionRequest view: a flattened run description plus the poll
// and venue options.
type Settings struct {
	Symbol          string             `json:"symbol"`
	Strategy        string             `json:"strategy"`
	Params          map[string]float64 `json:"params"`
	Days            int                `json:"days"`
	InitialCash     float64            `json:"initial_cash"`
	WarmupBars      int                `json:"warmup_bars"`
	Risk            *SessionRiskView   `json:"risk"`
	Review          bool               `json:"review"`
	IntervalSeconds int                `json:"interval_seconds"`
	Execute         bool               `json:"execute"`
	Confirm         string             `json:"confirm"`
	Leverage        int                `json:"leverage"`
	StatePath       string             `json:"state_path"`
	// Veto is the per-session entry-veto switch (cfg.LLM.VetoEnabled).
	Veto bool `json:"veto"`
}

// Session owns the one live trading loop the dashboard can run. The agent
// underneath is not safe for concurrent use, so every touch of it — a cycle,
// a status read — goes through this mutex.
type Session struct {
	// mu guards the bookkeeping fields below. It is deliberately NEVER held
	// across a cycle: one cycle calls the model and the exchange, which can
	// take minutes, and a status poll that waited on it would look to the
	// browser like the server had died ("无法读取会话状态：Failed to fetch").
	mu sync.Mutex
	// cycleMu serializes cycles so only one goroutine ever touches the runner
	// (the loop and the 立即执行 button both start them). Status does not take
	// it, which is what keeps polling responsive during a slow model call.
	cycleMu sync.Mutex
	runner  *live.Runner
	cfg     config.Config
	// snapshot is the runner-derived part of SessionStatus, published at the
	// end of every cycle. Status serves it instead of reading the live agent,
	// so a poll never races a running cycle and never blocks on one. The data
	// is exactly as fresh as the last completed cycle, which is the only time
	// the runner changes anyway.
	snapshot SessionStatus
	// statePath and interval are kept even after a stop so the console can
	// show where the session was persisted.
	statePath string
	interval  time.Duration
	execute   bool

	cancel     context.CancelFunc
	done       chan struct{}
	running    bool
	starting   bool
	stopping   bool
	recovering bool
	stopDone   chan struct{}
	stopErr    error
	startedAt  time.Time
	stoppedAt  time.Time

	cycles     int
	lastTick   time.Time
	lastPrice  float64
	lastAction string
	lastError  string
	lastReason string // the LLM's explanation of the latest decision
	log        []CycleRecord
	// journalPath is where the log is persisted, derived from the ledger path.
	// journalAppends counts rows written since the last compaction and
	// journalErr is the last write failure, reported on the status so a
	// read-only or full disk is visible instead of silently losing history.
	journalPath    string
	journalAppends int
	journalErr     string

	// SeriesLoader and PriceLoader are injected by tests so the suite stays
	// offline; production leaves them nil and the runner uses Binance.
	SeriesLoader func(symbol string, days int, end time.Time) (model.Series, error)
	PriceLoader  func(symbol string) (float64, time.Time, error)
	// InitRunner is an internal seam for offline concurrency tests. Production
	// leaves it nil and Start calls Runner.Init directly.
	InitRunner func(*live.Runner) error
}

// NewSession returns an idle session. It holds no runner until Start.
func NewSession() *Session { return &Session{} }

// RecoverLog loads the newest persisted run log in dir as this session's
// starting history. The console calls it when it creates a session so a freshly
// opened page shows what the previous run did, instead of an empty table that
// is indistinguishable from lost history. It is a no-op once a session has
// started, since Start then owns the log for its own ledger.
func (s *Session) RecoverLog(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil || s.journalPath != "" {
		return
	}
	path, records := LatestLog(dir)
	if path == "" {
		return
	}
	s.journalPath = path
	s.log = records
}

// StartOptions describes a session start request.
type StartOptions struct {
	Config    config.Config
	Interval  time.Duration
	Execute   bool
	StatePath string
	// Confirm must equal liveConfirmPhrase when Execute is true.
	Confirm string
	// Owner is the account the ledger belongs to ("" in the single-user
	// desktop / CLI builds). It is persisted with the state so a different
	// account can never resume this position.
	Owner string
}

// Start builds a runner and begins polling in the background. It refuses to
// start twice, and refuses real trading without the confirmation phrase and
// the exchange keys in the environment.
func (s *Session) Start(opts StartOptions) error {
	s.mu.Lock()
	if s.running || s.starting || s.recovering {
		defer s.mu.Unlock()
		if s.starting {
			return fmt.Errorf("交易会话正在初始化，请稍候")
		}
		if s.recovering {
			return fmt.Errorf("保护单恢复核验正在进行，请稍候")
		}
		return fmt.Errorf("已有交易会话在运行，请先停止")
	}
	if s.snapshot.RecoveryRequired || (s.runner != nil && s.runner.ProtectionRecoveryRequired()) {
		s.mu.Unlock()
		return fmt.Errorf("存在待人工核验的保护单恢复锁，请先完成恢复，不能用新会话覆盖")
	}
	s.mu.Unlock()
	opts.Config.Agent.Symbol = marketdata.BinanceSymbol(opts.Config.Agent.Symbol)

	if opts.Execute {
		if opts.Confirm != liveConfirmPhrase {
			return fmt.Errorf("实盘交易需要在确认框输入「%s」", liveConfirmPhrase)
		}
		if err := live.CheckExecutionAllowed(true); err != nil {
			return err
		}
		if !hasExchangeKeys(opts.Config) {
			return fmt.Errorf("实盘交易需要 Binance API 密钥：在设置里填写，或配置 BINANCE_API_KEY / BINANCE_SECRET_KEY 环境变量")
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

	s.mu.Lock()
	if s.running || s.starting || s.recovering {
		s.mu.Unlock()
		return fmt.Errorf("已有交易会话正在运行或初始化")
	}
	if s.snapshot.RecoveryRequired || (s.runner != nil && s.runner.ProtectionRecoveryRequired()) {
		s.mu.Unlock()
		return fmt.Errorf("存在待人工核验的保护单恢复锁，请先完成恢复，不能用新会话覆盖")
	}
	s.starting = true
	s.lastAction = "正在初始化"
	s.lastError = ""
	seriesLoader := s.SeriesLoader
	priceLoader := s.PriceLoader
	initRunner := s.InitRunner
	s.mu.Unlock()

	runner, err := live.NewForOwner(opts.Config, strat, opts.Execute, opts.StatePath, opts.Owner)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.lastAction = "启动失败"
		s.lastError = err.Error()
		s.mu.Unlock()
		return err
	}
	runner.SeriesLoader = seriesLoader
	runner.PriceLoader = priceLoader
	// Init reconciles against the exchange when executing; doing it before the
	// loop starts means a mismatched book fails the start instead of trading.
	if initRunner == nil {
		initRunner = func(r *live.Runner) error { return r.Init() }
	}
	if err := initRunner(runner); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.starting = false
		if !runner.ProtectionRecoveryRequired() {
			s.lastAction = "启动失败"
			s.lastError = err.Error()
			return err
		}
		// Preserve a blocked runner as a read-only recovery target. The page can
		// then show the restored position and exact protection evidence instead
		// of reducing a safety stop to a transient error toast.
		s.runner = runner
		s.cfg = opts.Config
		s.statePath = opts.StatePath
		s.interval = opts.Interval
		s.execute = opts.Execute
		s.running = false
		s.stopping = false
		s.lastAction = "启动被阻断"
		s.lastError = err.Error()
		s.journalPath = journalPathFor(opts.StatePath)
		s.log = loadJournal(s.journalPath, cycleLogLimit)
		s.captureLocked()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	journalPath := journalPathFor(opts.StatePath)
	log := loadJournal(journalPath, cycleLogLimit)

	s.mu.Lock()
	s.runner = runner
	s.cfg = opts.Config
	s.statePath = opts.StatePath
	s.interval = opts.Interval
	s.execute = opts.Execute
	s.cancel = cancel
	s.done = make(chan struct{})
	s.running = true
	s.starting = false
	s.stopping = false
	s.startedAt = time.Now()
	s.stoppedAt = time.Time{}
	s.cycles = 0
	s.lastError = ""
	s.lastAction = "启动"
	s.lastPrice = 0
	s.lastReason = ""
	// Resume the previous run's log instead of wiping it: the rows that
	// explain why the last run stopped are the first thing an operator needs
	// after a restart. A journal that cannot be read degrades to an empty log
	// rather than blocking the start.
	s.journalPath = journalPath
	s.journalErr = ""
	s.journalAppends = 0
	s.log = log
	if len(s.log) == 0 {
		s.log = nil
	}
	// Publish the starting book immediately: the console's first poll arrives
	// before the first cycle finishes (which can take minutes with a slow
	// model), and it should show the session's opening state rather than a row
	// of zeroes that looks like a failed start.
	s.captureLocked()
	s.mu.Unlock()

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
//
// The cycle deliberately runs WITHOUT s.mu. A cycle calls the model and the
// exchange, which can take minutes, and holding the status lock across it
// froze every /api/session poll for that whole time — the browser gave up and
// the console showed "无法读取会话状态：Failed to fetch" while the session was
// in fact healthy and trading. cycleMu serializes the cycles themselves (the
// loop and the 立即执行 button both start one, and the runner is not safe for
// concurrent use); s.mu is then taken only for the bookkeeping at the end.
func (s *Session) cycleOnce() {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	s.mu.Lock()
	if s.runner == nil || !s.running || s.stopping {
		s.mu.Unlock()
		return
	}
	runner := s.runner
	s.mu.Unlock()

	now := time.Now()
	result, err := runner.Cycle(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	// A new session may have replaced the runner while this cycle was in
	// flight (stop, then start again). The result then describes a run that is
	// no longer current, so it must not be written into the new session's
	// state.
	if s.runner != runner {
		return
	}
	s.cycles++
	s.lastTick = now

	agent := runner.Agent()
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
		s.captureLocked()
		return
	}

	s.lastError = ""
	s.lastAction = result.Action
	// Carry the strategy's explanation of this decision so the console can
	// show what the AI is thinking (empty for indicator strategies).
	s.lastReason = result.Reason
	// A successful cycle always fetched a live price; the mark-to-market
	// equity the engine returned is the authoritative number for this tick.

	s.appendLocked(CycleRecord{
		Time:     now.Format("15:04:05"),
		Action:   result.Action,
		Price:    s.lastPrice,
		Equity:   result.Equity,
		Cash:     result.Cash,
		Position: positionSummary(result.Open),
		// The strategy's explanation, including the failure detail when the
		// action is a degradation such as ai_unavailable.
		Reason: result.Reason,
	})
	s.captureLocked()
}

// captureLocked refreshes the runner-derived part of the status so Status can
// answer without touching the runner. The caller must hold mu AND must
// guarantee that no cycle is running (hold cycleMu, or be inside Start/Stop
// where the runner is not being cycled).
//
// Every slice is rebuilt rather than mutated, so a caller that is already
// holding a previously returned SessionStatus keeps reading a consistent
// snapshot instead of watching it change underneath.
func (s *Session) captureLocked() {
	view := SessionStatus{}
	if s.runner != nil {
		agent := s.runner.Agent()
		view.Equity = s.currentEquityLocked()
		view.Cash = agent.Book.AvailableCash()
		view.Wallet = agent.Book.Cash
		view.MarginUsed = agent.Book.MarginUsed()
		view.InitialCash = agent.Book.InitialCash
		view.PeakEquity = agent.PeakEquity
		view.Position = s.positionLocked()
		view.Risk = s.riskLocked(view.Equity)
		view.Protection = s.runner.ProtectionSnapshot()
		view.RecoveryRequired = s.runner.ProtectionRecoveryRequired()
		view.Trades = tradeViews(agent.Trades())
		if view.InitialCash > 0 {
			view.TotalReturnPct = (view.Equity/view.InitialCash - 1) * 100
		}
		interval := int(s.interval.Seconds())
		if interval == 0 {
			interval = 60
		}
		view.Settings = sessionSettings(s.cfg, interval, s.execute)
		fills := agent.Broker.Fills()
		if len(fills) > 200 {
			fills = fills[len(fills)-200:]
		}
		for _, f := range fills {
			view.Orders = append(view.Orders, ExecutionView{
				Time: f.Time.Format(time.RFC3339), Side: string(f.Side),
				Quantity: f.Quantity, Price: f.Price, Fee: f.Commission,
				Status: f.Status, Reason: f.Reason, OrderID: f.OrderID,
				ClientOrderID: f.ClientOrderID, Uncertain: f.Uncertain,
			})
		}
	}
	s.snapshot = view
}

// Step runs one cycle on demand, which is what the 立即执行 button uses.
// It runs synchronously — the caller asked for the result and waits for it —
// but it does not hold the status lock, so the console keeps polling normally
// while a slow model is thinking.
func (s *Session) Step() error {
	s.mu.Lock()
	if !s.running || s.stopping || s.runner == nil {
		s.mu.Unlock()
		return fmt.Errorf("没有正在运行的交易会话")
	}
	s.mu.Unlock()
	s.cycleOnce()
	return nil
}

// RecoverProtection clears a crash-era protection lock after an explicit
// operator confirmation and read-only exchange verification. It never starts
// the loop; starting a new session remains a separate action.
func (s *Session) RecoverProtection(confirm string) error {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	s.mu.Lock()
	if s.running || s.starting || s.stopping || s.recovering {
		s.mu.Unlock()
		return fmt.Errorf("交易会话运行中，不能执行恢复")
	}
	if s.runner == nil {
		s.mu.Unlock()
		return fmt.Errorf("没有待恢复的交易会话")
	}
	runner := s.runner
	s.recovering = true
	s.mu.Unlock()

	err := runner.RecoverProtectiveIntent(confirm)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovering = false
	if s.runner != runner {
		return fmt.Errorf("恢复期间交易会话已变化，请重新核验")
	}
	if err != nil {
		s.lastError = err.Error()
		s.captureLocked()
		return err
	}
	s.lastError = ""
	s.lastAction = "保护单已人工确认恢复"
	now := time.Now()
	s.appendLocked(CycleRecord{
		Time: now.Format("15:04:05"), Action: "人工恢复",
		Reason:   "本地账本、Binance 持仓和原始保护单 ID 已完成只读核验；未启动策略、未发送订单",
		Equity:   s.currentEquityLocked(),
		Position: positionSummary(s.runner.Agent().OpenTrade()),
	})
	s.captureLocked()
	return nil
}

// Stop cancels the loop and waits for it to finish. It is safe to call on an
// idle session.
func (s *Session) Stop() error {
	s.mu.Lock()
	if s.starting || s.recovering {
		s.mu.Unlock()
		return fmt.Errorf("交易会话正在初始化或恢复核验，请稍候")
	}
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

	// Wait out an in-flight on-demand cycle so the final save cannot race it.
	// A cycle started by the loop has already returned when done closed.
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

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
	s.captureLocked()
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

// ReviewFacts renders the trades this session actually made into the compact
// fact blob llm.Review consumes. It answers the live question ("how is my real
// position doing and why?") instead of the backtest one. The second return is
// false when no runner has ever started, so the caller can say so instead of
// sending the model an empty run.
func (s *Session) ReviewFacts(topN int) (string, bool) {
	// Serialize against a running cycle: the runner is not safe for concurrent
	// use, and this reads its whole result snapshot. The wait is bounded by the
	// model timeout, and the console calls this from an explicit button press
	// rather than a poll, so blocking here is acceptable.
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner == nil {
		return "", false
	}
	return engine.ReviewFacts(s.runner.Agent().ResultSnapshot(), topN), true
}

// Status builds the JSON view the console polls.
//
// It never touches the runner and never waits for a cycle: everything that
// comes from the runner is read from the snapshot published at the end of the
// last cycle. That is what lets the console keep polling while the model is
// thinking for a minute, instead of freezing until the cycle finishes.
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := SessionStatus{
		Running:     s.running,
		Mode:        "paper",
		Venue:       "futures",
		Symbol:      s.cfg.Agent.Symbol,
		Strategy:    s.cfg.Strategy.Name,
		Interval:    int(s.interval.Seconds()),
		Cycles:      s.cycles,
		LastPrice:   s.lastPrice,
		LastAction:  s.lastAction,
		LastError:   s.lastError,
		AIModel:     s.cfg.LLM.Model,
		AIReason:    s.lastReason,
		VetoEnabled: s.cfg.LLM.VetoEnabled,
		InitialCash: s.cfg.Risk.InitialCash,
		Leverage:    s.cfg.Risk.Leverage,
		Log:         append([]CycleRecord(nil), s.log...),
		StatePath:   s.statePath,
		LogPath:     s.journalPath,
		LogError:    s.journalErr,

		// The last cycle's runner-derived figures, published under this same
		// lock, so reading them cannot race a running cycle.
		Equity:           s.snapshot.Equity,
		Cash:             s.snapshot.Cash,
		Wallet:           s.snapshot.Wallet,
		MarginUsed:       s.snapshot.MarginUsed,
		PeakEquity:       s.snapshot.PeakEquity,
		TotalReturnPct:   s.snapshot.TotalReturnPct,
		Position:         s.snapshot.Position,
		Risk:             s.snapshot.Risk,
		Trades:           s.snapshot.Trades,
		Orders:           s.snapshot.Orders,
		Settings:         s.snapshot.Settings,
		Protection:       s.snapshot.Protection,
		RecoveryRequired: s.snapshot.RecoveryRequired,
		Starting:         s.starting,
		Recovering:       s.recovering,
		ProtectionActive: s.running && !s.stopping && s.execute && s.snapshot.Position.Open &&
			s.snapshot.Protection.State == broker.ProtectionVerified &&
			!s.snapshot.Risk.OrderUncertain && !s.snapshot.RecoveryRequired,
	}
	if status.Protection.State == "" {
		status.Protection.State = broker.ProtectionUnknown
		if !s.execute {
			status.Protection.State = broker.ProtectionNotRequired
		}
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
	if !s.startedAt.IsZero() {
		status.StartedAt = s.startedAt.Format(time.RFC3339)
	}
	if !s.stoppedAt.IsZero() {
		status.StoppedAt = s.stoppedAt.Format(time.RFC3339)
	}
	if !s.lastTick.IsZero() {
		status.LastTick = s.lastTick.Format(time.RFC3339)
	}

	// InitialCash is the session's own setting and stays authoritative even
	// before the first cycle has published a book.
	if s.snapshot.InitialCash > 0 {
		status.InitialCash = s.snapshot.InitialCash
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
	s.persistLogLocked(record)
}

// persistLogLocked appends one record to the on-disk journal. A failure is
// recorded on the status and never propagated: losing a log line must not stop
// a session that is managing a live position.
func (s *Session) persistLogLocked(record CycleRecord) {
	if s.journalPath == "" {
		return
	}
	if err := appendJournal(s.journalPath, record); err != nil {
		s.journalErr = err.Error()
		return
	}
	s.journalAppends++
	// Past the size cap, rewrite the file from the tail. The append above
	// already dropped that one row; compaction makes the loss permanent and
	// bounded instead of letting the file grow forever.
	if s.journalAppends >= cycleLogLimit {
		s.journalAppends = 0
		if err := rewriteJournal(s.journalPath, s.log); err != nil {
			s.journalErr = err.Error()
			return
		}
	}
	s.journalErr = ""
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

// hasExchangeKeys reports whether the Binance credentials are available: the
// per-user vault values injected by the webui (cfg.Live.Exchange*) first,
// then the environment unless accounts mode disabled that fallback. Secrets
// are never read from a config file.
func hasExchangeKeys(cfg config.Config) bool {
	apiKey := cfg.Live.ExchangeAPIKey
	if apiKey == "" && !cfg.Live.NoEnvKeys {
		apiKey = os.Getenv("BINANCE_API_KEY")
	}
	secretKey := cfg.Live.ExchangeSecretKey
	if secretKey == "" && !cfg.Live.NoEnvKeys {
		secretKey = os.Getenv("BINANCE_SECRET_KEY")
	}
	return apiKey != "" && secretKey != ""
}

// tradeViews renders closed round trips for the blotter. Its output JSON is
// identical to webui.tradeViews so a live session and a backtest share a shape.
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

// sessionSettings builds the Settings view echoed in SessionStatus. It mirrors
// webui.settingsView exactly, so the console prefills from the same JSON.
func sessionSettings(cfg config.Config, interval int, execute bool) *Settings {
	params := map[string]float64{}
	for k, v := range cfg.Strategy.Params {
		params[k] = v
	}
	return &Settings{
		Symbol: cfg.Agent.Symbol, Strategy: cfg.Strategy.Name, Params: params,
		Days: cfg.Live.LookbackDays, InitialCash: cfg.Risk.InitialCash,
		Risk: &SessionRiskView{MaxPositionPct: &cfg.Risk.MaxPositionPct, MaxRiskPerTradePct: &cfg.Risk.MaxRiskPerTradePct,
			StopLossPct: cfg.Risk.StopLossPct, TakeProfitPct: cfg.Risk.TakeProfitPct, MaxDrawdownPct: cfg.Risk.MaxDrawdownPct,
			MaxDailyLossPct: cfg.Risk.MaxDailyLossPct, AllowShort: &cfg.Risk.AllowShort,
			CommissionBps: &cfg.Execution.CommissionBps, SlippageBps: &cfg.Execution.SlippageBps},
		IntervalSeconds: interval, Execute: execute, Leverage: cfg.Risk.Leverage,
		Veto: cfg.LLM.VetoEnabled,
	}
}
