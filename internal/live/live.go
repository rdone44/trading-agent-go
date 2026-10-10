// Package live orchestrates one live trading session: it wires the broker
// (paper or real), persists state across restarts, reconciles the local
// book against the exchange on startup, and runs the polling loop.
package live

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/rdone44/trading-agent-go/internal/broker"
	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/llm"
	"github.com/rdone44/trading-agent-go/internal/marketdata"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/portfolio"
	"github.com/rdone44/trading-agent-go/internal/risk"
	"github.com/rdone44/trading-agent-go/internal/state"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// ProtectionRecoveryPhrase clears a local crash lock after read-only
// exchange verification. It does not start a session or authorize orders.
const ProtectionRecoveryPhrase = "确认恢复"

// Runner drives one live session.
type Runner struct {
	cfg        config.Config
	agent      *engine.Agent
	broker     broker.Broker
	statePath  string
	executed   bool
	leverage   int
	marginMode string
	protection broker.ProtectionSnapshot
	// protectiveRecoveryRequired is set when the process starts with a
	// protective intent already on disk. That file is crash evidence: normal
	// Save/Stop paths must never clear it before an explicit operator recovery.
	protectiveRecoveryRequired bool
	// owner is the account this runner's ledger belongs to. It is empty in
	// the single-user desktop / CLI builds and set in accounts mode, where a
	// state file must not be resumed by a different account.
	owner string

	// SeriesLoader and PriceLoader are the market-data edges of the loop. They
	// default to the public Binance endpoints; tests replace them so the suite
	// never touches the network, and a caller could point them at another
	// venue without touching the decision logic.
	SeriesLoader func(symbol string, days int, end time.Time) (model.Series, error)
	PriceLoader  func(symbol string) (float64, time.Time, error)
}

// futuresProtective adapts a real futures broker to the engine's Protective
// interface, covering an open position with exchange-side stop/target orders
// so it survives a process crash, network blip or restart. Every method is a
// no-op in dry-run (the broker returns nil / false immediately), so paper and
// backtest paths are untouched.
type futuresProtective struct{ b *broker.FuturesBroker }

// Open implements engine.Protective.
func (p *futuresProtective) Open(side broker.Side, stop, target float64) error {
	return p.b.PlaceProtective(side, stop, target)
}

// Cancel implements engine.Protective.
func (p *futuresProtective) Cancel() error { return p.b.CancelProtective() }

// Has implements engine.Protective.
func (p *futuresProtective) Has() (bool, error) { return p.b.HasProtective() }

// New builds a runner. execute=true places real orders on Binance USDT-margined
// perpetuals (keys come from the environment); execute=false simulates fills
// locally through the same broker. The leverage multiplier comes from the
// config; either way the same agent and risk logic run.
func New(cfg config.Config, strat strategy.Strategy, execute bool, statePath string) (*Runner, error) {
	return NewForOwner(cfg, strat, execute, statePath, "")
}

// NewForOwner is New with an explicit ledger owner. Accounts-mode callers
// pass the logged-in username so a state file can never be resumed by, or
// shared with, a different account.
func NewForOwner(cfg config.Config, strat strategy.Strategy, execute bool, statePath, owner string) (*Runner, error) {
	if err := CheckExecutionAllowed(execute); err != nil {
		return nil, err
	}
	cfg.Agent.Symbol = marketdata.BinanceSymbol(cfg.Agent.Symbol)
	if execute && statePath == "" {
		return nil, fmt.Errorf("实盘必须指定持久化状态文件")
	}
	// Credentials: the per-user vault values the webui injected win; when
	// they are empty (CLI, desktop, or a paper session) the environment is
	// the fallback, preserving the existing behaviour. In accounts mode
	// (cfg.Live.NoEnvKeys) that fallback is off: an account with no stored
	// keys must never trade the deployer's environment credentials. Absence
	// of both is a runtime concern the session layer checks, not a
	// construction failure.
	apiKey := cfg.Live.ExchangeAPIKey
	if apiKey == "" && !cfg.Live.NoEnvKeys {
		apiKey = os.Getenv("BINANCE_API_KEY")
	}
	secretKey := cfg.Live.ExchangeSecretKey
	if secretKey == "" && !cfg.Live.NoEnvKeys {
		secretKey = os.Getenv("BINANCE_SECRET_KEY")
	}
	book := portfolio.New(cfg.Risk.InitialCash, cfg.Risk.Leverage)
	rm := risk.New(cfg.Risk)

	leverage := cfg.Risk.Leverage
	if leverage < 1 {
		leverage = 1
	}
	marginMode := cfg.Live.MarginMode

	// One broker for both paper (DryRun) and execute: the perpetual venue is
	// the only one this program trades.
	fc := broker.FuturesConfig{
		Symbol:        cfg.Agent.Symbol,
		Leverage:      leverage,
		MarginMode:    marginMode,
		DryRun:        !execute,
		CommissionBps: &cfg.Execution.CommissionBps, SlippageBps: &cfg.Execution.SlippageBps,
	}
	if execute {
		fc.APIKey = apiKey
		fc.SecretKey = secretKey
		fc.JournalPath = statePath + ".order-pending.json"
		fc.ProtectiveJournalPath = statePath + ".protective-pending.json"
	}
	bk := broker.NewFutures(fc)

	agent := engine.NewWithBroker(cfg, strat, bk, book, rm)

	// Exchange-side protective orders, on a real order-placing venue only: the
	// adapter is a no-op in dry-run, and the cost of a protective placement (an
	// extra signed call) is only justified when we would otherwise be leaving a
	// naked position on the exchange.
	if execute {
		agent.Protective = &futuresProtective{b: bk}
	}

	// Wire the LLM second-opinion gate on new entries. It is fail-open (a
	// missing key or a model error lets the trade through), so it is safe to
	// install; it is only *active* when the user opted in via live.veto.
	// The gate memoizes a verdict for cfg.LLM.VetoCacheSec seconds so a
	// repeated, identical entry proposal does not re-bill the model every
	// poll; the hard risk limits are still re-checked each cycle.
	if cfg.LLM.VetoEnabled {
		gate := llm.NewVetoGate(cfg.LLM)
		agent.Veto = func(now time.Time, ctx engine.VetoContext) (bool, string) {
			req := llm.VetoRequest{
				Side: string(ctx.Side), Symbol: ctx.Symbol, Quantity: ctx.Quantity,
				EntryPrice: ctx.EntryPrice, StopPrice: ctx.StopPrice, TargetPrice: ctx.TargetPrice,
				Equity: ctx.Equity, Cash: ctx.Cash, Leverage: ctx.Leverage, Reason: ctx.Reason,
			}
			return gate.Decide(req)
		}
	}

	return &Runner{
		cfg: cfg, agent: agent, broker: bk, statePath: statePath,
		executed: execute, leverage: leverage, marginMode: marginMode,
		owner: owner,
	}, nil
}

// loadSeries fetches the lookback window, honouring a caller-supplied loader.
func (r *Runner) loadSeries(now time.Time) (model.Series, error) {
	if r.SeriesLoader != nil {
		return r.SeriesLoader(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
	}
	return marketdata.Load(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
}

// loadPrice fetches the latest traded price, honouring a caller-supplied
// loader.
func (r *Runner) loadPrice() (float64, time.Time, error) {
	if r.PriceLoader != nil {
		return r.PriceLoader(r.agent.Symbol)
	}
	return marketdata.LastPrice(r.agent.Symbol)
}

// IsExecuted reports whether this runner places real orders.
func (r *Runner) IsExecuted() bool { return r.executed }

// Leverage returns the configured leverage multiplier.
func (r *Runner) Leverage() int { return r.leverage }

// ProtectionSnapshot returns the last exchange-side protection inspection.
// The zero value is intentionally not treated as verified by callers.
func (r *Runner) ProtectionSnapshot() broker.ProtectionSnapshot { return r.protection }

// ProtectionRecoveryRequired reports whether a crash-era protective intent
// still prevents this runner from trading.
func (r *Runner) ProtectionRecoveryRequired() bool { return r.protectiveRecoveryRequired }

// RecoverProtectiveIntent clears crash evidence only after the local ledger,
// exchange position and original clientAlgoIds all agree. It performs no
// exchange writes; starting the strategy remains a separate operator action.
func (r *Runner) RecoverProtectiveIntent(confirm string) error {
	if confirm != ProtectionRecoveryPhrase {
		return fmt.Errorf("恢复保护单需要输入「%s」", ProtectionRecoveryPhrase)
	}
	if !r.executed || r.statePath == "" {
		return fmt.Errorf("当前没有可恢复的实盘状态")
	}
	if _, err := os.Stat(r.statePath + ".order-pending.json"); err == nil {
		return fmt.Errorf("仍有普通订单待核对，不能只恢复保护单")
	} else if !os.IsNotExist(err) {
		return err
	}
	if !r.hasProtectiveIntent() {
		return fmt.Errorf("没有遗留保护单意图需要恢复")
	}
	if err := broker.ValidateProtectiveIntent(r.statePath + ".protective-pending.json"); err != nil {
		return err
	}
	r.protectiveRecoveryRequired = true
	if err := r.reconcileFutures(); err != nil {
		return err
	}
	if r.protection.State != broker.ProtectionVerified {
		return fmt.Errorf("保护单尚未完整核验: %s", r.protection.Reason)
	}

	previous := r.agent.Risk.Snapshot()
	if previous.OrderUncertain && !protectiveRecoveryReason(previous.HaltReason) {
		return fmt.Errorf("当前还有非保护单的不确定状态，不能自动解除: %s", previous.HaltReason)
	}
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok {
		return fmt.Errorf("实盘 broker 类型无效，不能恢复")
	}
	wallet, err := b.USDTBalance()
	if err != nil {
		return fmt.Errorf("恢复前读取 Binance USDT 钱包失败: %w", err)
	}
	previousCash := r.agent.Book.Cash
	r.agent.Book.Cash = wallet
	recovered := previous
	recovered.OrderUncertain = false
	if recovered.Halted && protectiveRecoveryReason(recovered.HaltReason) {
		recovered.Halted = false
		recovered.HaltReason = ""
	}
	r.agent.Risk.Restore(recovered)
	r.protectiveRecoveryRequired = false
	if err := r.Save(); err != nil {
		r.protectiveRecoveryRequired = true
		r.agent.Risk.Restore(previous)
		r.agent.Book.Cash = previousCash
		return fmt.Errorf("保存恢复后的账本失败: %w", err)
	}
	return nil
}

func protectiveRecoveryReason(reason string) bool {
	for _, prefix := range []string{
		"保护单意图", "保护单提交状态待核对", "存在未核对的保护单意图",
		"读取遗留保护单", "遗留保护单", "交易所保护单", "交易所侧保护单",
	} {
		if len(reason) >= len(prefix) && reason[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// Agent exposes the underlying agent (its state and book) for reporting.
func (r *Runner) Agent() *engine.Agent { return r.agent }

// Init prepares the session: fetch exchange filters when executing, restore
// persisted state, and reconcile the local book against the exchange.
func (r *Runner) Init() error {
	if r.executed {
		if _, err := os.Stat(r.statePath + ".order-pending.json"); err == nil {
			return fmt.Errorf("存在未核对订单日志 %s.order-pending.json，请先在交易所确认成交状态", r.statePath)
		} else if !os.IsNotExist(err) {
			return err
		}
		r.protectiveRecoveryRequired = r.hasProtectiveIntent()
		if err := broker.ValidateProtectiveIntent(r.statePath + ".protective-pending.json"); err != nil {
			return fmt.Errorf("保护单意图需要人工核对: %w", err)
		}
	}
	if r.executed {
		b, ok := r.broker.(*broker.FuturesBroker)
		if !ok {
			return fmt.Errorf("execute=true 但 broker 不是 futures 实盘客户端")
		}
		// A protective pending file is crash evidence. Recovery must remain
		// read-only, so do not change leverage or margin mode before the
		// operator has reviewed the original algo IDs.
		if !r.protectiveRecoveryRequired {
			if err := b.Init(); err != nil {
				return fmt.Errorf("初始化 futures broker 失败: %w", err)
			}
		}
	}

	// Restore a previous session, if any.
	if r.statePath != "" {
		saved, ok, err := state.Load(r.statePath)
		if err != nil {
			return err
		}
		if ok {
			// Accounts mode: a ledger is private to the account that created
			// it. Refuse to adopt another user's file (copied, guessed or
			// left behind by a previous deployment) instead of loading their
			// position into this account's book.
			if r.owner != "" && saved.Owner != "" && saved.Owner != r.owner {
				return fmt.Errorf("状态文件属于账号 %q，当前账号 %q 无权使用", saved.Owner, r.owner)
			}
			if marketdata.BinanceSymbol(saved.Symbol) != marketdata.BinanceSymbol(r.cfg.Agent.Symbol) {
				return fmt.Errorf("状态文件属于 %s，不能用于 %s；请使用独立状态文件", saved.Symbol, r.cfg.Agent.Symbol)
			}
			if saved.Executed != r.executed {
				return fmt.Errorf("纸面与实盘状态不能共用，请使用独立状态文件")
			}
			// Every runner is a perpetual venue now. A file written by the
			// retired spot runner (an empty venue, or "spot") describes a
			// different accounting model entirely — adopting it would size a
			// leveraged position off a spot ledger — so it is refused.
			if saved.Venue != "futures" {
				return fmt.Errorf(
					"状态文件交易场所为 %s，本程序只支持 futures；现货账本不能自动转换，请使用独立状态文件",
					saved.Venue)
			}
			if saved.Accounting != "futures-margin-v1" {
				return fmt.Errorf("旧合约账本需要人工核对，不能自动转换")
			}
			if saved.Leverage != r.leverage && saved.Open != nil {
				return fmt.Errorf("持仓恢复时不能改变杠杆")
			}
			cash, peak, open, stop, target, riskState := saved.ToEngine()
			r.agent.RestoreState(cash, peak, open, stop, target, riskState)
			if saved.Initial > 0 {
				r.agent.Book.InitialCash = saved.Initial
			}
		}
	}

	// Reconcile against the exchange when placing real orders.
	if r.executed {
		if err := r.reconcile(); err != nil {
			return err
		}
		if r.protectiveRecoveryRequired {
			return fmt.Errorf(
				"发现遗留保护单意图，已完成只读核验（%s）；请在 Binance 人工确认持仓与保护单后再执行恢复，当前拒绝启动",
				r.protection.State)
		}
		balance, err := r.broker.(*broker.FuturesBroker).USDTBalance()
		if err != nil {
			return err
		}
		r.agent.Book.Cash = balance
		if r.agent.OpenTrade() == nil && r.agent.Risk.Snapshot().Day.IsZero() {
			r.agent.Book.InitialCash = r.agent.Book.Cash
			r.agent.PeakEquity = r.agent.Book.Cash
		}
		if r.agent.Risk.OrderUncertain {
			return fmt.Errorf("上次订单状态未确认：%s；请先在交易所核对状态文件", r.agent.Risk.HaltReason)
		}
		if err := r.checkpointProtectionIntent(); err != nil {
			return fmt.Errorf("保存保护单修复结果失败: %w", err)
		}
	}
	return nil
}

// reconcile compares the local book with the exchange and refuses to continue
// if the two disagree, so a restart cannot trade on stale assumptions.
func (r *Runner) reconcile() error {
	return r.reconcileFutures()
}

// reconcileFutures compares the local signed position with the exchange's
// positionRisk entry: on a perpetual venue a long and a short are both
// expressible, so both the side and the magnitude must agree.
func (r *Runner) reconcileFutures() error {
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok {
		return nil
	}
	side, quantity, entry, err := b.OpenPosition()
	if err != nil {
		return fmt.Errorf("拉取交易所持仓失败: %w", err)
	}
	pos := r.agent.Book.Position(r.agent.Symbol)
	localSigned := pos.Quantity
	// Normalise the exchange's position to the same signed convention
	// (long positive, short negative) the book uses.
	var exchangeSigned float64
	switch side {
	case broker.Buy:
		exchangeSigned = quantity
	case broker.Sell:
		exchangeSigned = -quantity
	}
	if math.Abs(localSigned-exchangeSigned) > 1e-6 {
		return fmt.Errorf(
			"本地状态与交易所不一致: 本地持仓=%.6f, 交易所 %s=%.6f。请人工核对后再启动",
			localSigned, r.agent.Symbol, exchangeSigned)
	}
	if pos.IsOpen() && !sameEntryPrice(pos.AvgPrice, entry) {
		return fmt.Errorf(
			"本地开仓均价与交易所不一致: 本地 %.8f, 交易所 %.8f。请核对成交明细后再启动",
			pos.AvgPrice, entry)
	}
	if r.protectiveRecoveryRequired {
		return r.inspectFuturesProtection(pos)
	}
	return r.reconcileFuturesProtection(pos)
}

// inspectFuturesProtection is the read-only recovery path. A pending intent
// may represent a POST whose response was lost, so startup must not place or
// cancel anything until an operator has reviewed the original IDs.
func (r *Runner) inspectFuturesProtection(pos *portfolio.Position) error {
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok || r.agent.Protective == nil {
		r.protection = broker.ProtectionSnapshot{State: broker.ProtectionUnknown, CheckedAt: time.Now().UTC(), Reason: "实盘保护单适配器不可用"}
		return fmt.Errorf("无法核验遗留保护单意图")
	}
	side := broker.Side("")
	if pos.IsOpen() {
		side = broker.Buy
		if pos.Quantity < 0 {
			side = broker.Sell
		}
	}
	snapshot, err := b.InspectProtective(side, pos.StopPrice, pos.TakeProfitPrice)
	r.protection = snapshot
	if err != nil {
		return fmt.Errorf("读取遗留保护单失败: %w", err)
	}
	if snapshot.State != broker.ProtectionVerified && snapshot.State != broker.ProtectionNotRequired {
		return fmt.Errorf("遗留保护单尚未形成完整证据: %s", snapshot.Reason)
	}
	return nil
}

// reconcileFuturesProtection is the single exchange-protection seam used at
// startup and before live decisions. It reports exact coverage, repairs only
// missing legs, and refuses to continue on conflicts or unknown exchange
// state. A boolean "has any leg" is not enough to authorize a new order.
func (r *Runner) reconcileFuturesProtection(pos *portfolio.Position) error {
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok || r.agent.Protective == nil {
		r.protection = broker.ProtectionSnapshot{State: broker.ProtectionNotRequired, CheckedAt: time.Now().UTC()}
		return nil
	}
	side := broker.Side("")
	if pos.IsOpen() {
		side = broker.Buy
		if pos.Quantity < 0 {
			side = broker.Sell
		}
	}
	snapshot, err := b.InspectProtective(side, pos.StopPrice, pos.TakeProfitPrice)
	r.protection = snapshot
	if err != nil {
		return fmt.Errorf("查询交易所保护单失败: %w", err)
	}
	switch snapshot.State {
	case broker.ProtectionNotRequired, broker.ProtectionVerified:
		return nil
	case broker.ProtectionMissing, broker.ProtectionPartial:
		if !pos.IsOpen() {
			return fmt.Errorf("空仓保护状态异常: %s", snapshot.Reason)
		}
		if err := r.agent.Protective.Open(side, pos.StopPrice, pos.TakeProfitPrice); err != nil {
			return fmt.Errorf("补挂缺失保护单失败: %w", err)
		}
		return r.recheckProtection(b, side, pos)
	case broker.ProtectionConflict:
		if !pos.IsOpen() {
			// A flat book may safely remove only the agent-owned legs. The
			// broker still validates every ID before sending DELETE.
			if err := r.agent.Protective.Cancel(); err != nil {
				return fmt.Errorf("撤销残留保护单失败: %w", err)
			}
			return r.recheckProtection(b, side, pos)
		}
		return fmt.Errorf("交易所保护单与本地持仓冲突: %s", snapshot.Reason)
	case broker.ProtectionUnknown:
		return fmt.Errorf("交易所保护状态未知: %s", snapshot.Reason)
	default:
		return fmt.Errorf("未知保护状态 %q", snapshot.State)
	}
}

func (r *Runner) recheckProtection(b *broker.FuturesBroker, side broker.Side, pos *portfolio.Position) error {
	snapshot, err := b.InspectProtective(side, pos.StopPrice, pos.TakeProfitPrice)
	r.protection = snapshot
	if err != nil {
		return fmt.Errorf("补挂后核验保护单失败: %w", err)
	}
	if snapshot.State != broker.ProtectionVerified && snapshot.State != broker.ProtectionNotRequired {
		return fmt.Errorf("补挂后保护单仍未闭环: %s", snapshot.Reason)
	}
	return nil
}

// Cycle runs one decision loop: fetch the series and latest price for the
// active venue, feed them through the agent, and persist the result. It
// returns what the cycle did.
func (r *Runner) Cycle(now time.Time) (engine.StepResult, error) {
	if r.agent.Risk.OrderUncertain {
		return engine.StepResult{}, fmt.Errorf("订单状态待核对：%s", r.agent.Risk.HaltReason)
	}
	pendingAtStart := r.executed && (r.protectiveRecoveryRequired || r.hasProtectiveIntent())
	if pendingAtStart {
		r.protectiveRecoveryRequired = true
	}
	if r.executed {
		if err := broker.ValidateProtectiveIntent(r.statePath + ".protective-pending.json"); err != nil {
			return r.requireReconciliation(fmt.Errorf("保护单意图待核对: %w", err))
		}
	}
	// Exchange-side protection can fill between polls without any local
	// fill, leaving the book describing a position that no longer exists.
	// Verify the venue's position before price, strategy or orders; never
	// infer an execution price/fee from a position delta, and never keep
	// trading a phantom position.
	if r.executed {
		if err := r.checkFuturesInventory(); err != nil {
			return r.requireReconciliation(err)
		}
		var protectionErr error
		if pendingAtStart {
			protectionErr = r.inspectFuturesProtection(r.agent.Book.Position(r.agent.Symbol))
		} else {
			protectionErr = r.reconcileFuturesProtection(r.agent.Book.Position(r.agent.Symbol))
		}
		if protectionErr != nil {
			return r.requireReconciliation(protectionErr)
		}
		if pendingAtStart {
			return r.requireReconciliation(fmt.Errorf("保护单意图已按原始 ID 核验；请在 Binance 人工确认持仓与保护单后重新恢复会话"))
		}
		if err := r.checkpointProtectionIntent(); err != nil {
			return r.requireReconciliation(fmt.Errorf("保存保护单修复结果失败: %w", err))
		}
	}
	price, _, err := r.loadPrice()
	if err != nil {
		return engine.StepResult{}, err
	}
	res, protectErr := r.agent.Protect(price, now)
	// A failed cancellation can set OrderUncertain without adding a fill or
	// returning a Protect error. Do not repair missing protection against a
	// stale ledger after a conditional child may already have triggered.
	if r.executed && protectErr == nil && !(r.leverage > 1 && r.agent.Risk.OrderUncertain) {
		if err := r.reconcileFuturesProtection(r.agent.Book.Position(r.agent.Symbol)); err != nil {
			return r.requireReconciliation(err)
		}
	}
	if protectErr != nil || res.Exited || r.agent.Risk.Halted {
		if saveErr := r.Save(); saveErr != nil {
			return res, saveErr
		}
		return res, protectErr
	}
	series, err := r.loadSeries(now)
	if err != nil {
		if saveErr := r.Save(); saveErr != nil {
			return res, saveErr
		}
		return res, err
	}

	res, err = r.agent.Decide(series, price, now, res)
	if r.executed && r.hasProtectiveIntent() && r.agent.Risk.Halted {
		return r.requireReconciliation(fmt.Errorf("保护单提交状态待核对，禁止自动撤单或平仓"))
	}
	if err != nil {
		if saveErr := r.Save(); saveErr != nil {
			return res, fmt.Errorf("%v；保存失败：%w", err, saveErr)
		}
		return res, err
	}
	if r.executed {
		if err := r.reconcileFuturesProtection(r.agent.Book.Position(r.agent.Symbol)); err != nil {
			return r.requireReconciliation(err)
		}
	}
	if err := r.Save(); err != nil {
		return res, err
	}
	return res, nil
}

func (r *Runner) hasProtectiveIntent() bool {
	if r.statePath == "" {
		return false
	}
	_, err := os.Stat(r.statePath + ".protective-pending.json")
	return err == nil
}

// checkpointProtectionIntent closes the normal (non-recovery) crash window
// before the cycle can fetch a price or submit another order. A protection
// repair is only complete after both the exchange snapshot and local ledger
// are durable; Save then clears the intent.
func (r *Runner) checkpointProtectionIntent() error {
	if !r.executed || r.protectiveRecoveryRequired || !r.hasProtectiveIntent() {
		return nil
	}
	pos := r.agent.Book.Position(r.agent.Symbol)
	ready := (pos.IsOpen() && r.protection.State == broker.ProtectionVerified) ||
		(!pos.IsOpen() && r.protection.State == broker.ProtectionNotRequired)
	if !ready {
		return fmt.Errorf("保护单尚未形成可保存的完整证据（%s）", r.protection.State)
	}
	return r.Save()
}

// requireReconciliation persists the halt and returns the error, so a
// detected inconsistency survives a restart instead of being forgotten.
func (r *Runner) requireReconciliation(err error) (engine.StepResult, error) {
	r.agent.Risk.RequireReconciliation(err.Error())
	if saveErr := r.Save(); saveErr != nil {
		return engine.StepResult{}, fmt.Errorf("%v；保存失败：%w", err, saveErr)
	}
	return engine.StepResult{}, err
}

// checkFuturesInventory compares the local signed position with the
// exchange's. A disagreement means a protective leg filled (or an order was
// placed outside this process) since the last cycle. There is no safe way to
// reconstruct the fill from the position delta alone — the entry/exit price
// and fees would be invented — so the session halts for reconciliation
// instead of trading a book that no longer matches the venue.
func (r *Runner) checkFuturesInventory() error {
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok {
		return nil
	}
	if r.statePath != "" {
		if _, err := os.Stat(r.statePath + ".order-pending.json"); err == nil {
			return fmt.Errorf("存在未核对的订单日志，需先在交易所确认成交状态")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	side, quantity, entry, err := b.OpenPosition()
	if err != nil {
		return fmt.Errorf("合约周期持仓查询失败，需对账: %w", err)
	}
	var exchangeSigned float64
	switch side {
	case broker.Buy:
		exchangeSigned = quantity
	case broker.Sell:
		exchangeSigned = -quantity
	}
	pos := r.agent.Book.Position(r.agent.Symbol)
	local := pos.Quantity
	if math.IsNaN(local) || math.IsInf(local, 0) || math.IsNaN(exchangeSigned) || math.IsInf(exchangeSigned, 0) {
		return fmt.Errorf("合约周期持仓数值无效，需对账")
	}
	if math.Abs(local-exchangeSigned) > 1e-6 {
		return fmt.Errorf(
			"交易所持仓与本地账本不一致（本地 %.6f，交易所 %.6f），可能已有保护单成交，需人工对账后重启会话",
			local, exchangeSigned)
	}
	if pos.IsOpen() && !sameEntryPrice(pos.AvgPrice, entry) {
		return fmt.Errorf(
			"交易所持仓均价与本地账本不一致（本地 %.8f，交易所 %.8f），需核对成交明细后重启会话",
			pos.AvgPrice, entry)
	}
	return nil
}

func sameEntryPrice(local, exchange float64) bool {
	if local <= 0 || exchange <= 0 || math.IsNaN(local) || math.IsNaN(exchange) ||
		math.IsInf(local, 0) || math.IsInf(exchange, 0) {
		return false
	}
	tolerance := math.Max(1e-8, math.Max(math.Abs(local), math.Abs(exchange))*1e-8)
	return math.Abs(local-exchange) <= tolerance
}

// Save persists the current session state.
func (r *Runner) Save() error {
	if r.statePath == "" {
		return nil
	}
	pos := r.agent.Book.Position(r.agent.Symbol)
	var stop, target float64
	if pos.IsOpen() {
		stop, target = pos.StopPrice, pos.TakeProfitPrice
	}
	s := state.FromEngine(
		r.agent.Symbol, r.agent.Strategy.Describe(),
		r.agent.Book.InitialCash, r.agent.Book.Cash, r.agent.PeakEquity,
		r.agent.OpenTrade(), stop, target, r.agent.Risk.Snapshot(), r.executed,
	)
	s.Venue = "futures"
	s.Accounting = "futures-margin-v1"
	s.Leverage = r.leverage
	s.Owner = r.owner
	if dir := filepath.Dir(r.statePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.agent.Risk.RequireReconciliation("持仓目录不可写：" + err.Error())
			return err
		}
	}
	if err := state.Save(r.statePath, s); err != nil {
		r.agent.Risk.RequireReconciliation("持仓保存失败：" + err.Error())
		return err
	}
	if r.executed && !r.agent.Risk.OrderUncertain {
		if err := broker.ClearIntent(r.statePath + ".order-pending.json"); err != nil {
			r.agent.Risk.RequireReconciliation("订单日志清理失败：" + err.Error())
			return err
		}
		pos := r.agent.Book.Position(r.agent.Symbol)
		protectionReady := (!pos.IsOpen() && r.protection.State == broker.ProtectionNotRequired) ||
			(pos.IsOpen() && r.protection.State == broker.ProtectionVerified)
		if protectionReady && !r.protectiveRecoveryRequired {
			if err := broker.ClearProtectiveIntent(r.statePath + ".protective-pending.json"); err != nil {
				r.agent.Risk.RequireReconciliation("保护单意图清理失败：" + err.Error())
				return err
			}
		}
	}
	return nil
}
