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

// Runner drives one live session.
type Runner struct {
	cfg        config.Config
	agent      *engine.Agent
	broker     broker.Broker
	statePath  string
	executed   bool
	futures    bool
	leverage   int
	marginMode string

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

// spotProtective adapts quantity-bound spot stops to the engine contract.
// It reads the book after ApplyFill, so base-asset fees are already deducted.
// Wiring it into New requires cycle-level protective-fill reconciliation first.
type spotProtective struct {
	b      *broker.BinanceBroker
	book   *portfolio.Portfolio
	symbol string
}

func (p *spotProtective) Open(side broker.Side, stop, target float64) error {
	pos := p.book.Position(p.symbol)
	if side != broker.Buy || pos.Quantity <= 0 || math.IsNaN(pos.Quantity) || math.IsInf(pos.Quantity, 0) {
		return fmt.Errorf("现货保护只允许净多头持仓")
	}
	return p.b.PlaceSpotStop(pos.Quantity, stop)
}

func (p *spotProtective) Has() (bool, error) { return p.b.HasSpotStops() }

// checkInventory detects possible protective fills without inventing ledger
// entries. Locked inventory is valid; only total net quantity must agree.
func (p *spotProtective) checkInventory() error {
	total, _, err := p.b.Balances()
	if err != nil {
		return fmt.Errorf("现货保护周期余额查询失败，需对账: %w", err)
	}
	qty := p.book.Position(p.symbol).Quantity
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 ||
		math.IsNaN(qty) || math.IsInf(qty, 0) || qty < 0 || math.Abs(total-qty) > 1e-9 {
		return fmt.Errorf("现货保护周期净持仓不一致，可能已有保护成交，需对账")
	}
	return nil
}

func (p *spotProtective) Cancel() error {
	if err := p.b.CancelSpotStops(); err != nil {
		return err
	}
	// An empty openOrders list does not prove the stop never filled. Confirm
	// both total inventory and released free inventory before a local sell.
	total, _, err := p.b.Balances()
	if err != nil {
		return err
	}
	free, _, err := p.b.AvailableBalances()
	if err != nil {
		return err
	}
	qty := p.book.Position(p.symbol).Quantity
	if math.IsNaN(total) || math.IsInf(total, 0) || math.IsNaN(free) || math.IsInf(free, 0) ||
		math.IsNaN(qty) || math.IsInf(qty, 0) || qty < 0 || math.Abs(total-qty) > 1e-9 || free < qty-1e-9 {
		return fmt.Errorf("现货保护撤单后净持仓或可用余额不一致，需对账")
	}
	return nil
}

// New builds a runner. execute=true places real orders on Binance (keys come
// from the environment); execute=false simulates fills locally. The venue
// (spot vs USDT-margined perpetual) and the leverage multiplier come from the
// config; either way the same agent and risk logic run.
func New(cfg config.Config, strat strategy.Strategy, execute bool, statePath string) (*Runner, error) {
	if err := CheckExecutionAllowed(execute); err != nil {
		return nil, err
	}
	cfg.Agent.Symbol = marketdata.BinanceSymbol(cfg.Agent.Symbol)
	if execute && statePath == "" {
		return nil, fmt.Errorf("实盘必须指定持久化状态文件")
	}
	// Credentials: the per-user vault values the webui injected win; when
	// they are empty (CLI, desktop, or a paper session) the environment is
	// the fallback, preserving the existing behaviour. Absence of both is a
	// runtime concern the session layer checks, not a construction failure.
	apiKey := cfg.Live.ExchangeAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("BINANCE_API_KEY")
	}
	secretKey := cfg.Live.ExchangeSecretKey
	if secretKey == "" {
		secretKey = os.Getenv("BINANCE_SECRET_KEY")
	}
	book := portfolio.New(cfg.Risk.InitialCash)
	if cfg.Live.Futures {
		book = portfolio.NewFutures(cfg.Risk.InitialCash, cfg.Risk.Leverage)
	}
	rm := risk.New(cfg.Risk)

	futures := cfg.Live.Futures
	leverage := cfg.Risk.Leverage
	if leverage < 1 {
		leverage = 1
	}
	marginMode := cfg.Live.MarginMode

	var bk broker.Broker
	switch {
	case futures:
		// Perpetual venue: one broker for both paper (DryRun) and execute.
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
		}
		bk = broker.NewFutures(fc)
	case execute:
		bk = broker.NewBinance(broker.BinanceConfig{
			Symbol:      cfg.Agent.Symbol,
			APIKey:      apiKey,
			SecretKey:   secretKey,
			JournalPath: statePath + ".order-pending.json",
		})
	default:
		bk = broker.New(cfg.Execution)
	}

	agent := engine.NewWithBroker(cfg, strat, bk, book, rm)

	// Exchange-side protective orders: only on a real order-placing perpetual
	// venue. The adapter is a no-op in dry-run and spot, so installing it there
	// would still be safe, but the cost of a protective-order placement (an
	// extra signed call) is only justified when we would otherwise be leaving a
	// naked position on the exchange.
	if execute && futures {
		if fb, ok := bk.(*broker.FuturesBroker); ok {
			agent.Protective = &futuresProtective{b: fb}
		}
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
		executed: execute, futures: futures, leverage: leverage, marginMode: marginMode,
	}, nil
}

// loadSeries fetches the lookback window, honouring a caller-supplied loader.
func (r *Runner) loadSeries(now time.Time) (model.Series, error) {
	if r.SeriesLoader != nil {
		return r.SeriesLoader(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
	}
	if r.futures {
		return marketdata.Futures(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
	}
	return marketdata.Binance(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
}

// loadPrice fetches the latest traded price, honouring a caller-supplied
// loader.
func (r *Runner) loadPrice() (float64, time.Time, error) {
	if r.PriceLoader != nil {
		return r.PriceLoader(r.agent.Symbol)
	}
	if r.futures {
		return marketdata.FuturesLastPrice(r.agent.Symbol)
	}
	return marketdata.LastPrice(r.agent.Symbol)
}

// IsExecuted reports whether this runner places real orders.
func (r *Runner) IsExecuted() bool { return r.executed }

// IsFutures reports whether this runner trades a perpetual venue.
func (r *Runner) IsFutures() bool { return r.futures }

// Leverage returns the configured leverage multiplier (1 = spot-like).
func (r *Runner) Leverage() int { return r.leverage }

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
	}
	if r.executed {
		if r.futures {
			b, ok := r.broker.(*broker.FuturesBroker)
			if !ok {
				return fmt.Errorf("execute=true 但 broker 不是 futures 实盘客户端")
			}
			if err := b.Init(); err != nil {
				return fmt.Errorf("初始化 futures broker 失败: %w", err)
			}
		} else {
			b, ok := r.broker.(*broker.BinanceBroker)
			if !ok {
				return fmt.Errorf("execute=true 但 broker 不是 Binance 实盘客户端")
			}
			if err := b.Init(); err != nil {
				return fmt.Errorf("获取交易所过滤器失败: %w", err)
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
			if marketdata.BinanceSymbol(saved.Symbol) != marketdata.BinanceSymbol(r.cfg.Agent.Symbol) {
				return fmt.Errorf("状态文件属于 %s，不能用于 %s；请使用独立状态文件", saved.Symbol, r.cfg.Agent.Symbol)
			}
			if saved.Executed != r.executed {
				return fmt.Errorf("纸面与实盘状态不能共用，请使用独立状态文件")
			}
			venue := "spot"
			if r.futures {
				venue = "futures"
			}
			savedVenue := saved.Venue
			if savedVenue == "" {
				savedVenue = "spot"
			}
			if savedVenue != venue {
				return fmt.Errorf("状态文件交易场所为 %s，当前为 %s", savedVenue, venue)
			}
			if r.futures && saved.Accounting != "futures-margin-v1" {
				return fmt.Errorf("旧合约账本需要人工核对，不能自动转换")
			}
			if r.futures && saved.Leverage != r.leverage && saved.Open != nil {
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
		if r.futures {
			balance, err := r.broker.(*broker.FuturesBroker).USDTBalance()
			if err != nil {
				return err
			}
			r.agent.Book.Cash = balance
		} else {
			_, cash, err := r.broker.(*broker.BinanceBroker).AvailableBalances()
			if err != nil {
				return err
			}
			r.agent.Book.Cash = cash
		}
		if r.agent.OpenTrade() == nil && r.agent.Risk.Snapshot().Day.IsZero() {
			r.agent.Book.InitialCash = r.agent.Book.Cash
			r.agent.PeakEquity = r.agent.Book.Cash
		}
		if r.agent.Risk.OrderUncertain {
			return fmt.Errorf("上次订单状态未确认：%s；请先在交易所核对状态文件", r.agent.Risk.HaltReason)
		}
	}
	return nil
}

// reconcile compares the local book with the exchange and refuses to continue
// if the two disagree, so a restart cannot trade on stale assumptions.
func (r *Runner) reconcile() error {
	if r.futures {
		return r.reconcileFutures()
	}
	b, ok := r.broker.(*broker.BinanceBroker)
	if !ok {
		return nil
	}
	base, quote, err := b.Balances()
	if err != nil {
		return fmt.Errorf("拉取交易所余额失败: %w", err)
	}
	pos := r.agent.Book.Position(r.agent.Symbol)
	haveLocal := pos.IsOpen()
	haveExchange := base > 0
	if haveLocal != haveExchange {
		return fmt.Errorf(
			"本地状态与交易所不一致: 本地持仓=%v, 交易所 %s 余额=%.6f/%.2f (base/quote)。请人工核对后再启动",
			haveLocal, r.agent.Symbol, base, quote)
	}
	if haveLocal && math.Abs(pos.Quantity-base) > 1e-6 {
		return fmt.Errorf("持仓数量不一致: 本地 %.6f, 交易所 %.6f，拒绝继续", pos.Quantity, base)
	}
	if r.agent.Protective != nil {
		// Total inventory includes the quantity locked by a surviving stop.
		// Never repair protection from an uncertain or invalid restored book.
		if math.IsNaN(base) || math.IsInf(base, 0) || base < 0 ||
			math.IsNaN(pos.Quantity) || math.IsInf(pos.Quantity, 0) || pos.Quantity < 0 ||
			math.Abs(pos.Quantity-base) > 1e-9 {
			return fmt.Errorf("现货保护恢复时净持仓不一致，需对账")
		}
		if r.agent.Risk.OrderUncertain {
			return fmt.Errorf("现货保护恢复时订单状态未确认，需对账")
		}
		if haveLocal {
			// Open validates the existing quantity/stop, or repairs a missing
			// stop using the persisted risk level. Has alone is insufficient.
			if err := r.agent.Protective.Open(broker.Buy, pos.StopPrice, pos.TakeProfitPrice); err != nil {
				return fmt.Errorf("重启后补挂现货保护单失败: %w", err)
			}
		} else {
			has, err := r.agent.Protective.Has()
			if err != nil {
				return fmt.Errorf("查询现货残留保护单失败: %w", err)
			}
			if has {
				if err := r.agent.Protective.Cancel(); err != nil {
					return fmt.Errorf("撤销现货残留保护单失败: %w", err)
				}
			}
		}
	}
	return nil
}

// reconcileFutures compares the local signed position with the exchange's
// positionRisk entry: on a perpetual venue a long and a short are both
// expressible, so both the side and the magnitude must agree.
func (r *Runner) reconcileFutures() error {
	b, ok := r.broker.(*broker.FuturesBroker)
	if !ok {
		return nil
	}
	side, quantity, _, err := b.OpenPosition()
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
	// Reconcile the exchange-side protective orders so a restart can never leave
	// a position naked, nor a stale protective order dangling:
	//   - local flat but protective orders still open  -> cancel the strays
	//   - local open -> inspect each leg and place only the missing ones
	// Open() inspects openAlgoOrders and validates surviving legs before writes.
	// Has() is only an any-leg check for orphan cleanup, not complete coverage.
	// The stop/target come straight from
	// the restored position — the same single source the risk engine used.
	if r.agent.Protective != nil {
		open := pos.IsOpen()
		has, err := r.agent.Protective.Has()
		if err != nil {
			return fmt.Errorf("查询交易所保护单失败: %w", err)
		}
		switch {
		case !open && has:
			// A crash left protective orders with no position behind them.
			if err := r.agent.Protective.Cancel(); err != nil {
				return fmt.Errorf("撤销残留保护单失败: %w", err)
			}
		case open:
			// A surviving target must not hide a missing stop (or vice versa).
			side := broker.Buy
			if pos.Quantity < 0 {
				side = broker.Sell
			}
			if err := r.agent.Protective.Open(side, pos.StopPrice, pos.TakeProfitPrice); err != nil {
				return fmt.Errorf("重启后补挂保护单失败: %w", err)
			}
		}
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
	// Quantity-bound spot stops can fill between polls without a local fill.
	// Check total inventory before history, strategy or local exits; never
	// infer execution price/fees from a balance delta or sell the stale book.
	// This seam is inactive until spot protection is explicitly installed.
	if !r.futures {
		if p, ok := r.agent.Protective.(*spotProtective); ok {
			if err := p.checkInventory(); err != nil {
				r.agent.Risk.RequireReconciliation(err.Error())
				if saveErr := r.Save(); saveErr != nil {
					return engine.StepResult{}, fmt.Errorf("%v；保存失败：%w", err, saveErr)
				}
				return engine.StepResult{}, err
			}
		}
	}
	price, _, err := r.loadPrice()
	if err != nil {
		return engine.StepResult{}, err
	}
	res, protectErr := r.agent.Protect(price, now)
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
	if err != nil {
		if saveErr := r.Save(); saveErr != nil {
			return res, fmt.Errorf("%v；保存失败：%w", err, saveErr)
		}
		return res, err
	}
	if err := r.Save(); err != nil {
		return res, err
	}
	return res, nil
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
	s.Venue = "spot"
	if r.futures {
		s.Venue = "futures"
		s.Accounting = "futures-margin-v1"
	}
	s.Leverage = r.leverage
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
	}
	return nil
}
