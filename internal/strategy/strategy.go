// Package strategy turns OHLCV bars into a target position per bar.
//
// A strategy never touches cash or order sizing: it only says "I want to be
// long / flat / short" and optionally supplies a stop and a profit target. The
// engine compares the target with the current position and trades the
// difference.
package strategy

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/indicators"
	"github.com/rdone44/trading-agent-go/internal/model"
)

// Signals is the per-bar output of a strategy, aligned with the input series.
// Stop and TakeProfit are NaN when unused.
type Signals struct {
	Signal     []float64
	Stop       []float64
	TakeProfit []float64
}

// Normalize guarantees that every slice is aligned with the series and that
// missing levels read as NaN, so the engine can index all three without
// special cases.
func (s Signals) Normalize(n int) Signals {
	return Signals{
		Signal:     align(s.Signal, n, 0),
		Stop:       align(s.Stop, n, math.NaN()),
		TakeProfit: align(s.TakeProfit, n, math.NaN()),
	}
}

// align pads or truncates a slice to exactly n values.
func align(values []float64, n int, fill float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = fill
	}
	copy(out, values)
	return out
}

// Strategy is the interface every strategy implements.
type Strategy interface {
	Name() string
	Generate(series model.Series, cfg config.Config) (Signals, error)
	Describe() string
}

// LiveDecision is implemented by strategies that are expensive to regenerate
// across a whole series — an LLM that would otherwise be called once per
// historical bar on every live poll. The live loop asks such a strategy for a
// single decision on the most recent bar instead of regenerating the full
// signal, so a poll bills the model once rather than O(bars) times. Backtests
// keep using Generate, which genuinely needs every bar. reason carries the
// model's own one-line explanation of the call (empty for non-LLM
// implementations) so the console can show what the AI is thinking instead of
// a bare position number.
type LiveDecision interface {
	LastDecision(series model.Series, cfg config.Config) (signal, stop, target float64, reason string)
}

// New builds a strategy by name.
func New(name string, cfg config.Config) (Strategy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ma_cross", "":
		return MovingAverageCross{}, nil
	case "rsi_reversion":
		return RSIReversion{}, nil
	case "breakout":
		return Breakout{}, nil
	case "llm":
		// A fresh client is built from the config inside Generate; the
		// key is resolved from the environment only when a call happens.
		return LLM{}, nil
	default:
		return nil, fmt.Errorf("未知策略 %q，可用策略：%s", name, strings.Join(Available(), "、"))
	}
}

// Available lists the built-in strategy names.
func Available() []string {
	names := []string{"breakout", "llm", "ma_cross", "rsi_reversion"}
	sort.Strings(names)
	return names
}

// --------------------------------------------------------------------------
// ma_cross

// MovingAverageCross is long while the fast SMA is above the slow SMA.
type MovingAverageCross struct{}

func (MovingAverageCross) Name() string { return "ma_cross" }

func (s MovingAverageCross) Describe() string {
	return "ma_cross"
}

func (MovingAverageCross) Generate(series model.Series, cfg config.Config) (Signals, error) {
	fast := cfg.IntParam("fast", 10)
	slow := cfg.IntParam("slow", 30)
	minGap := cfg.Param("min_gap_pct", 0) / 100.0
	if fast >= slow {
		return Signals{}, fmt.Errorf("ma_cross 要求快线周期小于慢线周期（当前 fast=%d, slow=%d）", fast, slow)
	}

	close := series.Close()
	fastMA := indicators.SMA(close, fast)
	slowMA := indicators.SMA(close, slow)
	sig := make([]float64, len(close))
	for i := range close {
		switch {
		case model.IsNaN(fastMA[i]) || model.IsNaN(slowMA[i]) || slowMA[i] == 0:
			sig[i] = math.NaN() // warm-up: hold the previous state
		default:
			gap := (fastMA[i] - slowMA[i]) / slowMA[i]
			switch {
			case gap > minGap:
				sig[i] = 1
			case gap < -minGap:
				sig[i] = 0
			default:
				sig[i] = math.NaN() // dead zone: hold
			}
		}
	}
	return Signals{Signal: forwardFill(sig, 0)}, nil
}

// --------------------------------------------------------------------------
// rsi_reversion

// RSIReversion buys oversold RSI and exits when momentum recovers.
type RSIReversion struct{}

func (RSIReversion) Name() string { return "rsi_reversion" }

func (s RSIReversion) Describe() string {
	return "rsi_reversion"
}

func (RSIReversion) Generate(series model.Series, cfg config.Config) (Signals, error) {
	period := cfg.IntParam("period", 14)
	lower := cfg.Param("lower", 30)
	exitLevel := cfg.Param("exit_level", 55)
	atrPeriod := cfg.IntParam("atr_period", 14)
	stopMult := cfg.Param("atr_stop_mult", 2.5)

	close := series.Close()
	rsi := indicators.RSI(close, period)
	atr := indicators.ATR(series.High(), series.Low(), close, atrPeriod)

	sig := make([]float64, len(close))
	for i := range close {
		switch {
		case rsi[i] < lower:
			sig[i] = 1
		case rsi[i] > exitLevel:
			sig[i] = 0
		default:
			sig[i] = math.NaN()
		}
	}
	sig = forwardFill(sig, 0)

	stop := make([]float64, len(close))
	for i := range close {
		stop[i] = math.NaN()
		if sig[i] > 0 {
			stop[i] = close[i] - stopMult*atr[i]
		}
	}
	return Signals{Signal: sig, Stop: stop}, nil
}

// --------------------------------------------------------------------------
// breakout

// Breakout trades Donchian breakouts with an ATR trailing stop.
type Breakout struct{}

func (Breakout) Name() string { return "breakout" }

func (s Breakout) Describe() string {
	return "breakout"
}

func (Breakout) Generate(series model.Series, cfg config.Config) (Signals, error) {
	lookback := cfg.IntParam("lookback", 20)
	atrPeriod := cfg.IntParam("atr_period", 14)
	stopMult := cfg.Param("atr_stop_mult", 2.0)
	exitLookback := cfg.IntParam("exit_lookback", lookback/2)
	if exitLookback < 2 {
		exitLookback = 2
	}

	high, low := series.High(), series.Low()
	close := series.Close()
	upper := indicators.ShiftRight(indicators.RollingMax(high, lookback), 1)
	lower := indicators.ShiftRight(indicators.RollingMin(low, exitLookback), 1)
	atr := indicators.ATR(high, low, close, atrPeriod)

	sig := make([]float64, len(close))
	for i := range close {
		switch {
		case model.IsNaN(upper[i]) || model.IsNaN(lower[i]):
			sig[i] = math.NaN()
		case close[i] > upper[i]:
			sig[i] = 1
		case close[i] < lower[i]:
			sig[i] = 0
		default:
			sig[i] = math.NaN()
		}
	}
	sig = forwardFill(sig, 0)

	trailing := make([]float64, len(close))
	runMax := indicators.RollingMax(close, lookback)
	for i := range close {
		trailing[i] = math.NaN()
		if sig[i] > 0 && !model.IsNaN(runMax[i]) {
			trailing[i] = runMax[i] - stopMult*atr[i]
		}
	}
	return Signals{Signal: sig, Stop: trailing}, nil
}

// forwardFill replaces NaN with the last known value, then with `initial`.
func forwardFill(values []float64, initial float64) []float64 {
	out := make([]float64, len(values))
	last := initial
	for i, v := range values {
		if model.IsNaN(v) {
			out[i] = last
			continue
		}
		last = v
		out[i] = v
	}
	return out
}
