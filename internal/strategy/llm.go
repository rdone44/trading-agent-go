// The LLM strategy: an OpenAI-compatible model reads a compact, causal
// feature snapshot of the most recent bars and answers with a target
// position (long / flat / short) plus optional protective levels. It is a
// first-class strategy — the same interface as the indicator ones — so it
// plugs straight into backtests and the live loop.
//
// Two knobs matter when backtesting long histories:
//
//   - llm_step   ask the model every k-th bar and hold the answer between
//     calls (default 1; raise it to cap the number of model calls)
//   - llm_window how many recent closes go into a call
//
// Without an API key the strategy degrades to an all-flat signal rather than
// failing the run, so a backtest without a key still produces a coherent
// equity curve and can be inspected.
//
// The indicators (SMA, RSI, ATR) are cheap and are always computed over the
// whole series; only the model call is expensive. LastDecision — used by the
// live loop — asks the model for one decision on the most recent bar instead
// of regenerating a per-bar signal across every historical bar, so a poll
// bills the model once rather than O(bars) times.
package strategy

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/indicators"
	"github.com/rdone44/trading-agent-go/internal/llm"
	"github.com/rdone44/trading-agent-go/internal/model"
)

// LLM is a strategy that delegates its target-position decision to a model.
type LLM struct {
	// ask overrides the model call; nil means "build one from cfg at
	// Generate time". Tests inject a stub here to stay offline.
	ask func(system, user string) (string, error)
	// enabled is consulted only when ask is injected. The built-in client
	// checks its own key instead.
	enabled bool
}

func (LLM) Name() string { return "llm" }

func (LLM) Describe() string { return "llm" }

// llmIndicators bundles the cheap per-bar indicator series so they can be
// computed once and shared between the full-signal path (Generate) and the
// single-decision path (LastDecision).
type llmIndicators struct {
	fast, slow, atrPeriod, rsiPeriod int
	smaF, smaS, rsi, atr             []float64
}

// buildIndicators computes the indicator series and the parameters that shape
// the prompt. It is pure arithmetic (no model call), so it is cheap even on a
// long lookback window.
func buildIndicators(series model.Series, cfg config.Config) llmIndicators {
	close := series.Close()
	high, low := series.High(), series.Low()
	fast := cfg.IntParam("fast", 10)
	slow := cfg.IntParam("slow", 30)
	rsiPeriod := cfg.IntParam("period", 14)
	atrPeriod := cfg.IntParam("atr_period", 14)
	return llmIndicators{
		fast: fast, slow: slow, atrPeriod: atrPeriod, rsiPeriod: rsiPeriod,
		smaF: indicators.SMA(close, fast), smaS: indicators.SMA(close, slow),
		rsi: indicators.RSI(close, rsiPeriod), atr: indicators.ATR(high, low, close, atrPeriod),
	}
}

// resolver returns the model call and whether the strategy is usable at all
// (a usable strategy can also be a stub injected for tests).
func (s LLM) resolver(cfg config.Config) (func(string, string) (string, error), bool) {
	if s.ask != nil {
		return s.ask, s.enabled
	}
	client := llm.New(cfg.LLM)
	return client.Complete, client.Enabled()
}

// Generate walks the series with a causal window and, on the bars it chooses
// to call the model on, asks for a target position. Answers are
// forward-filled so the engine sees a complete per-bar signal.
func (s LLM) Generate(series model.Series, cfg config.Config) (Signals, error) {
	ask, enabled := s.resolver(cfg)

	n := series.Len()
	if n == 0 {
		return Signals{}, fmt.Errorf("行情数据为空")
	}
	// Without a usable model, hold flat for the whole run: a complete zero
	// signal keeps the engine's accounting sound and the backtest inspectable.
	if !enabled {
		return Signals{Signal: make([]float64, n)}, nil
	}

	ind := buildIndicators(series, cfg)

	window := cfg.IntParam("llm_window", 30)
	if window < 5 {
		window = 5
	}
	step := cfg.IntParam("llm_step", 1)
	if step < 1 {
		step = 1
	}

	sig, stop, target := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range sig {
		sig[i], stop[i], target[i] = math.NaN(), math.NaN(), math.NaN()
	}

	// warm-up: skip until the slowest indicator is defined.
	start := ind.slow
	if start >= n {
		start = n - 1
	}

	for i := start; i < n; i++ {
		if i%step != 0 {
			continue
		}
		pos, sp, tp, ok := s.decideAt(ask, series, cfg, ind, i, window)
		if !ok {
			continue // unusable answer: hold the previous state
		}
		sig[i] = pos
		if sp > 0 {
			stop[i] = sp
		}
		if tp > 0 {
			target[i] = tp
		}
	}

	return Signals{
		Signal:     forwardFill(sig, 0),
		Stop:       forwardFill(stop, math.NaN()),
		TakeProfit: forwardFill(target, math.NaN()),
	}, nil
}

// LastDecision answers the live loop's question: what position would the
// strategy take right now, given the most recent bar? It is a single model
// call — not a full per-bar regeneration — so a poll bills the model once.
//
// A model error or a missing key degrades to flat (0, NaN, NaN) rather than
// erroring: the live loop must stay alive when the model is down, exactly the
// fail-open philosophy the entry veto uses. A flat answer is always a safe
// no-op for the engine.
func (s LLM) LastDecision(series model.Series, cfg config.Config) (signal, stop, target float64) {
	ask, enabled := s.resolver(cfg)
	if !enabled || series.Len() == 0 {
		return 0, math.NaN(), math.NaN()
	}
	ind := buildIndicators(series, cfg)
	window := cfg.IntParam("llm_window", 30)
	if window < 5 {
		window = 5
	}
	pos, sp, tp, ok := s.decideAt(ask, series, cfg, ind, series.Len()-1, window)
	if !ok {
		return 0, math.NaN(), math.NaN()
	}
	return pos, sp, tp
}

// decideAt asks the model for one target-position decision on bar i and
// returns the snapped position plus any stop/target it supplied. ok is false
// when the model could not be called or its answer did not parse, in which
// case the caller holds the previous state.
func (s LLM) decideAt(ask func(string, string) (string, error), series model.Series, cfg config.Config, ind llmIndicators, i, window int) (pos, stop, target float64, ok bool) {
	user := llmUserPrompt(series.Symbol, i, window, series.Close(), ind)
	answer, err := ask(llmSystemPrompt(cfg), user)
	if err != nil {
		return 0, 0, 0, false
	}
	return parseLLMAnswer(answer)
}

// parseLLMAnswer extracts {position, stop, target} from a model reply. The
// reply is expected to contain a JSON object, possibly inside markdown fences
// or around prose; anything that does not parse returns ok=false and the
// caller holds the last state.
func parseLLMAnswer(answer string) (position, stop, target float64, ok bool) {
	var payload struct {
		Position json.Number `json:"position"`
		Stop     json.Number `json:"stop"`
		Target   json.Number `json:"target"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(answer)), &payload); err != nil {
		return 0, 0, 0, false
	}
	pos, err := payload.Position.Float64()
	if err != nil {
		return 0, 0, 0, false
	}
	// Snap to the three legal targets; out-of-range values become flat.
	switch {
	case pos >= 0.5:
		pos = 1
	case pos <= -0.5:
		pos = -1
	default:
		pos = 0
	}
	// Missing stop/target come back as the JSON null / empty number; treat a
	// non-finite value as "no level" so the engine falls back to its own.
	sp, _ := payload.Stop.Float64()
	tp, _ := payload.Target.Float64()
	if math.IsNaN(sp) || math.IsInf(sp, 0) {
		sp = 0
	}
	if math.IsNaN(tp) || math.IsInf(tp, 0) {
		tp = 0
	}
	return pos, sp, tp, true
}

// extractJSONObject trims a reply down to the first balanced object, so the
// model can answer in prose around a JSON block.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return "{}"
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

// llmSystemPrompt frames the model's job and the answer contract.
func llmSystemPrompt(cfg config.Config) string {
	return fmt.Sprintf(
		"You are a crypto technical analyst. You are given a compact snapshot of a "+
			"price series (%s, %s) and must decide the desired position on the next bar.\n"+
			"Answer ONLY with a JSON object: {\"position\": -1|0|1, \"stop\": <price|null>, \"target\": <price|null>, \"reason\": \"<short>\"}.\n"+
			"position: 1 = long, 0 = flat, -1 = short. stop/target are in the same units as price; use null when none.\n"+
			"Be conservative; when unsure, position=0.\n"+
			"Risk profile: max risk per trade %.2f%%, default stop loss %.2f%%, default take profit %.2f%%.",
		cfg.Agent.Symbol, cfg.Agent.Timeframe,
		cfg.Risk.MaxRiskPerTradePct*100, stopLossPct(cfg), takeProfitPct(cfg),
	)
}

// llmUserPrompt renders one causal snapshot as compact text: the recent close
// path plus the current indicator readings. The indicator periods are the
// ones actually configured, so the labels the model reads are honest.
func llmUserPrompt(symbol string, i, window int, close []float64, ind llmIndicators) string {
	lo := i - window + 1
	if lo < 0 {
		lo = 0
	}
	seg := make([]string, 0, i-lo+1)
	for j := lo; j <= i; j++ {
		seg = append(seg, fmtNum(close[j], 2))
	}
	return fmt.Sprintf(
		"Recent closes (oldest→newest, %d bars): %s\n"+
			"SMA%d=%s SMA%d=%s RSI%d=%s ATR%d=%s last_close=%s\n"+
			"Symbol=%s bar_index=%d",
		i-lo+1, strings.Join(seg, ","),
		ind.fast, fmtNum(ind.smaF[i], 2), ind.slow, fmtNum(ind.smaS[i], 2),
		ind.rsiPeriod, fmtNum(ind.rsi[i], 1), ind.atrPeriod, fmtNum(ind.atr[i], 2), fmtNum(close[i], 2),
		symbol, i,
	)
}

func fmtNum(v float64, digits int) string {
	if math.IsNaN(v) {
		return "na"
	}
	return strconv.FormatFloat(v, 'f', digits, 64)
}

func stopLossPct(cfg config.Config) float64 {
	if cfg.Risk.StopLossPct != nil {
		return *cfg.Risk.StopLossPct * 100
	}
	return 0
}

func takeProfitPct(cfg config.Config) float64 {
	if cfg.Risk.TakeProfitPct != nil {
		return *cfg.Risk.TakeProfitPct * 100
	}
	return 0
}
