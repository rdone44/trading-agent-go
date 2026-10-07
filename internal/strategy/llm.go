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
package strategy

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/indicators"
	"github.com/huijun/trading-agent-go/internal/llm"
	"github.com/huijun/trading-agent-go/internal/model"
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

// Generate walks the series with a causal window and, on the bars it chooses
// to call the model on, asks for a target position. Answers are
// forward-filled so the engine sees a complete per-bar signal.
func (s LLM) Generate(series model.Series, cfg config.Config) (Signals, error) {
	// Pick the model call and whether it is usable at all.
	var ask func(string, string) (string, error)
	enabled := true
	if s.ask != nil {
		ask, enabled = s.ask, s.enabled
	} else {
		client := llm.New(cfg.LLM)
		enabled = client.Enabled()
		ask = client.Complete
	}

	n := series.Len()
	if n == 0 {
		return Signals{}, fmt.Errorf("行情数据为空")
	}
	// Without a usable model, hold flat for the whole run: a complete zero
	// signal keeps the engine's accounting sound and the backtest inspectable.
	if !enabled {
		return Signals{Signal: make([]float64, n)}, nil
	}

	window := cfg.IntParam("llm_window", 30)
	if window < 5 {
		window = 5
	}
	step := cfg.IntParam("llm_step", 1)
	if step < 1 {
		step = 1
	}
	fast := cfg.IntParam("fast", 10)
	slow := cfg.IntParam("slow", 30)
	period := cfg.IntParam("period", 14)

	close := series.Close()
	high, low := series.High(), series.Low()
	smaF, smaS := indicators.SMA(close, fast), indicators.SMA(close, slow)
	rsi := indicators.RSI(close, period)
	atr := indicators.ATR(high, low, close, period)

	sig, stop, target := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range sig {
		sig[i], stop[i], target[i] = math.NaN(), math.NaN(), math.NaN()
	}

	for i := 0; i < n; i++ {
		if i%step != 0 || i < slow { // skip warm-up and held-back bars
			continue
		}
		user := llmUserPrompt(series.Symbol, i, window, fast, slow, close, smaF, smaS, rsi, atr)
		answer, err := ask(llmSystemPrompt(cfg), user)
		if err != nil {
			return Signals{}, fmt.Errorf("llm 策略在第 %d 根K线调用模型失败: %w", i, err)
		}
		pos, sp, tp, ok := parseLLMAnswer(answer)
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
	sp, _ := payload.Stop.Float64()
	tp, _ := payload.Target.Float64()
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
// path plus the current indicator readings.
func llmUserPrompt(symbol string, i, window, fast, slow int, close, smaF, smaS, rsi, atr []float64) string {
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
		fast, fmtNum(smaF[i], 2), slow, fmtNum(smaS[i], 2),
		14, fmtNum(rsi[i], 1), 14, fmtNum(atr[i], 2), fmtNum(close[i], 2),
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
