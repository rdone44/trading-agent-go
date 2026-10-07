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

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/llm"
	"github.com/huijun/trading-agent-go/internal/marketdata"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/portfolio"
	"github.com/huijun/trading-agent-go/internal/risk"
	"github.com/huijun/trading-agent-go/internal/state"
	"github.com/huijun/trading-agent-go/internal/strategy"
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
}

// New builds a runner. execute=true places real orders on Binance (keys come
// from the environment); execute=false simulates fills locally. The venue
// (spot vs USDT-margined perpetual) and the leverage multiplier come from the
// config; either way the same agent and risk logic run.
func New(cfg config.Config, strat strategy.Strategy, execute bool, statePath string) (*Runner, error) {
	book := portfolio.New(cfg.Risk.InitialCash)
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
			Symbol:     cfg.Agent.Symbol,
			Leverage:   leverage,
			MarginMode: marginMode,
			DryRun:     !execute,
		}
		if execute {
			// Keys are read from the environment only — never from config files.
			fc.APIKey = os.Getenv("BINANCE_API_KEY")
			fc.SecretKey = os.Getenv("BINANCE_SECRET_KEY")
		}
		bk = broker.NewFutures(fc)
	case execute:
		bk = broker.NewBinance(broker.BinanceConfig{
			Symbol:    cfg.Agent.Symbol,
			APIKey:    os.Getenv("BINANCE_API_KEY"),
			SecretKey: os.Getenv("BINANCE_SECRET_KEY"),
		})
	default:
		bk = broker.New(cfg.Execution)
	}

	agent := engine.NewWithBroker(cfg, strat, bk, book, rm)

	// Wire the LLM second-opinion gate on new entries. It is fail-open (a
	// missing key or a model error lets the trade through), so it is safe to
	// install; it is only *active* when the user opted in via live.veto.
	if cfg.LLM.VetoEnabled {
		llmCfg := cfg.LLM
		agent.Veto = func(now time.Time, ctx engine.VetoContext) (bool, string) {
			req := llm.VetoRequest{
				Side: string(ctx.Side), Symbol: ctx.Symbol, Quantity: ctx.Quantity,
				EntryPrice: ctx.EntryPrice, StopPrice: ctx.StopPrice, TargetPrice: ctx.TargetPrice,
				Equity: ctx.Equity, Cash: ctx.Cash, Leverage: ctx.Leverage, Reason: ctx.Reason,
			}
			return llm.VetoDecision(llmCfg, req)
		}
	}

	return &Runner{
		cfg: cfg, agent: agent, broker: bk, statePath: statePath,
		executed: execute, futures: futures, leverage: leverage, marginMode: marginMode,
	}, nil
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
			cash, peak, open, stop, target, riskState := saved.ToEngine()
			r.agent.RestoreState(cash, peak, open, stop, target, riskState)
		}
	}

	// Reconcile against the exchange when placing real orders.
	if r.executed {
		return r.reconcile()
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
	return nil
}

// Cycle runs one decision loop: fetch the series and latest price for the
// active venue, feed them through the agent, and persist the result. It
// returns what the cycle did.
func (r *Runner) Cycle(now time.Time) (engine.StepResult, error) {
	var series model.Series
	var err error
	if r.futures {
		series, err = marketdata.Futures(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
	} else {
		series, err = marketdata.Binance(r.agent.Symbol, r.cfg.Live.LookbackDays, now)
	}
	if err != nil {
		return engine.StepResult{}, err
	}

	var price float64
	if r.futures {
		price, _, err = marketdata.FuturesLastPrice(r.agent.Symbol)
	} else {
		price, _, err = marketdata.LastPrice(r.agent.Symbol)
	}
	if err != nil {
		return engine.StepResult{}, err
	}

	res, err := r.agent.LiveStep(series, price, now)
	if err != nil {
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
	if dir := filepath.Dir(r.statePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return state.Save(r.statePath, s)
}
