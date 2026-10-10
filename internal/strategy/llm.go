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
	"time"

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

// resolver returns the model call, whether the strategy is usable at all (a
// usable strategy can also be a stub injected for tests), and the model name
// to report on the wire. The built-in client carries its own name; an
// injected stub does not, so the console then shows the configured name.
func (s LLM) resolver(cfg config.Config) (func(string, string) (string, error), bool, string) {
	if s.ask != nil {
		return s.ask, s.enabled, ""
	}
	client := llm.New(cfg.LLM)
	return client.Complete, client.Enabled(), client.Model()
}

// Generate walks the series with a causal window and, on the bars it chooses
// to call the model on, asks for a target position. Answers are
// forward-filled so the engine sees a complete per-bar signal.
func (s LLM) Generate(series model.Series, cfg config.Config) (Signals, error) {
	ask, enabled, _ := s.resolver(cfg)

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
		pos, sp, tp, _, _, err := s.decideAt(ask, series, cfg, ind, i, window)
		if err != nil {
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
// ok is false when the model is missing, unreachable or answering
// unparseable text. The live loop then holds the current position instead of
// acting on a default: an outage must never be read as "go flat" and
// liquidate a healthy position. This matches Generate, which forward-fills
// the previous target on a failed call.
func (s LLM) LastDecision(series model.Series, cfg config.Config) (signal, stop, target float64, reason string, ok bool) {
	ask, enabled, modelName := s.resolver(cfg)
	if modelName == "" {
		modelName = cfg.LLM.Model
	}
	if !enabled {
		// Classify the failure for the operator: a missing credential is a
		// config gap to fix in settings, a timeout is a budget to raise, an
		// HTTP 401 is a wrong key — the ai_fault state machine and the
		// console key off the category prefix, not the raw text.
		return 0, math.NaN(), math.NaN(), classifyDecisionError(fmt.Errorf("未配置模型密钥（设置 → Binance 与 AI 服务 填写 AI Token）")), false
	}
	if series.Len() == 0 {
		return 0, math.NaN(), math.NaN(), classifyDecisionError(fmt.Errorf("行情数据为空，无法请求模型")), false
	}
	ind := buildIndicators(series, cfg)
	window := cfg.IntParam("llm_window", 30)
	if window < 5 {
		window = 5
	}
	d, ok := s.decideAtDetailed(ask, modelName, series, cfg, ind, series.Len()-1, window)
	if !ok {
		return 0, math.NaN(), math.NaN(), d.Reason, false
	}
	return d.Signal, d.Stop, d.Target, d.Reason, true
}

// LastDecisionDetailed answers the live loop with the structured Decision
// (strategy.StructuredLiveDecision). On failure, ok=false and Decision.Reason
// carries the classified cause so the console log can say what to fix.
func (s LLM) LastDecisionDetailed(series model.Series, cfg config.Config) (Decision, bool) {
	ask, enabled, modelName := s.resolver(cfg)
	if modelName == "" {
		modelName = cfg.LLM.Model
	}
	d := Decision{Model: modelName}
	if !enabled {
		d.Reason = classifyDecisionError(fmt.Errorf("未配置模型密钥（设置 → Binance 与 AI 服务 填写 AI Token）"))
		return d, false
	}
	if series.Len() == 0 {
		d.Reason = classifyDecisionError(fmt.Errorf("行情数据为空，无法请求模型"))
		return d, false
	}
	ind := buildIndicators(series, cfg)
	window := cfg.IntParam("llm_window", 30)
	if window < 5 {
		window = 5
	}
	return s.decideAtDetailed(ask, modelName, series, cfg, ind, series.Len()-1, window)
}

// decideAtDetailed wraps decideAt with the audit fields: it times the model
// call and stamps the model name and the model's own confidence on the
// answer, so a successful cycle records not just WHAT the AI decided but
// WHICH model decided it, how long it took and how sure it was — the fields
// the console's AI strip and the ai_fault state machine rely on.
func (s LLM) decideAtDetailed(ask func(string, string) (string, error), modelName string, series model.Series, cfg config.Config, ind llmIndicators, i, window int) (Decision, bool) {
	start := time.Now()
	d := Decision{Model: modelName}
	pos, stop, target, reason, confidence, err := s.decideAt(ask, series, cfg, ind, i, window)
	d.Signal, d.Stop, d.Target, d.Reason, d.Confidence = pos, stop, target, reason, confidence
	d.Latency = time.Since(start)
	if err != nil {
		d.Signal, d.Stop, d.Target = 0, math.NaN(), math.NaN()
		d.Reason = classifyDecisionError(err)
		return d, false
	}
	return d, true
}

// decisionErrorCategories are the prefixes classifyDecisionError puts on a
// failure reason. They are a stable contract: the session's ai_fault state
// machine detects a classified failure by these prefixes (a plain "AI 不可用"
// text is no longer the whole story — the operator must see the kind), and
// the console renders one Chinese label per category.
var decisionErrorCategories = []string{
	"no_key",      // the credential is missing: a settings gap
	"timeout",     // the model did not answer within the budget
	"network",     // connection reset, refused, DNS or proxy trouble
	"http",        // the endpoint answered with an HTTP error code
	"unparseable", // the model answered, but not with a usable decision
}

// isClassifiedFailure reports whether a strategy failure reason carries a
// decisionErrorCategories prefix, i.e. the strategy reported a machine-readable
// cause instead of a bare "AI 不可用".
func isClassifiedFailure(reason string) bool {
	for _, c := range decisionErrorCategories {
		if strings.HasPrefix(reason, c+":") {
			return true
		}
	}
	return false
}

// decisionFailureCategory extracts the prefix from a classified failure
// reason; it returns "unknown" when the reason is not classified.
func decisionFailureCategory(reason string) string {
	for _, c := range decisionErrorCategories {
		if strings.HasPrefix(reason, c+":") {
			return c
		}
	}
	return "unknown"
}

// classifyDecisionError turns a raw failure into a "<category>: <detail>"
// string the operator can act on. The raw error text is kept after the
// colon so nothing is lost; the category is what the state machine and the
// UI key on.
func classifyDecisionError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	category := "unknown"
	switch {
	case strings.Contains(text, "未配置模型密钥") || strings.Contains(text, "api key"):
		category = "no_key"
	case strings.Contains(text, "deadline exceeded"), strings.Contains(text, "timeout exceeded"),
		strings.Contains(text, "tls handshake timeout"), strings.Contains(text, "i/o timeout"):
		category = "timeout"
	case strings.Contains(text, "unparseable"), strings.Contains(text, "无法解析"):
		category = "unparseable"
	case strings.Contains(text, "http 4"), strings.Contains(text, "http 5"):
		category = "http"
	case strings.Contains(text, "refused"), strings.Contains(text, "reset"), strings.Contains(text, "closed"),
		strings.Contains(text, "no such host"), strings.Contains(text, "dial"), strings.Contains(text, "broken pipe"),
		strings.Contains(text, "tls"), strings.Contains(text, "eof"):
		category = "network"
	}
	return category + ": " + err.Error()
}

// decideAt asks the model for one target-position decision on bar i and
// returns the snapped position plus any stop/target it supplied. A non-nil
// error means the model could not be called or its answer did not parse, and
// its text says which — the caller holds the previous state and surfaces the
// message in the console log. reason is the model's own short explanation,
// trimmed to one line for the console. confidence is the model's
// self-assessed 0..1 certainty (0 when it did not say one).
func (s LLM) decideAt(ask func(string, string) (string, error), series model.Series, cfg config.Config, ind llmIndicators, i, window int) (pos, stop, target float64, reason string, confidence float64, err error) {
	user := llmUserPrompt(series.Symbol, i, window, series.Close(), ind)
	answer, callErr := ask(llmSystemPrompt(cfg), user)
	if callErr != nil {
		return 0, 0, 0, "", 0, callErr
	}
	pos, stop, target, reason, confidence, ok := parseLLMAnswer(answer)
	if !ok {
		// Include a short excerpt of what the model actually said: "答案无法
		// 解析" alone gives the operator nothing to correct.
		return 0, 0, 0, "", 0, fmt.Errorf("模型回答无法解析为 JSON 目标仓位: %s", llm.Truncate(answer, 160))
	}
	return pos, stop, target, reason, confidence, nil
}

// parseLLMAnswer extracts {position, stop, target, reason, confidence} from a
// model reply. The reply is expected to contain a JSON object, possibly
// inside markdown fences or around prose; anything that does not parse
// returns ok=false and the caller holds the last state. confidence is
// optional in the contract: models that omit it report 0.
func parseLLMAnswer(answer string) (position, stop, target float64, reason string, confidence float64, ok bool) {
	var payload struct {
		Position   json.Number `json:"position"`
		Stop       json.Number `json:"stop"`
		Target     json.Number `json:"target"`
		Reason     string      `json:"reason"`
		Confidence json.Number `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(answer)), &payload); err != nil {
		return 0, 0, 0, "", 0, false
	}
	pos, err := payload.Position.Float64()
	if err != nil {
		return 0, 0, 0, "", 0, false
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
	// Confidence is a self-assessed 0..1; a missing or non-finite value
	// means "the model did not say", and an out-of-range one is clamped
	// rather than trusted.
	conf, confErr := payload.Confidence.Float64()
	if confErr != nil || math.IsNaN(conf) || math.IsInf(conf, 0) {
		conf = 0
	}
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	// The model writes the reason in its own words; trim it to one short line
	// so it fits the status strip without crowding the controls.
	reason = strings.TrimSpace(payload.Reason)
	if idx := strings.IndexAny(reason, "\r\n"); idx >= 0 {
		reason = reason[:idx]
	}
	if len([]rune(reason)) > 80 {
		reason = string([]rune(reason)[:80]) + "…"
	}
	return pos, sp, tp, reason, conf, true
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

// llmSystemPrompt frames the model's job and the answer contract. The
// persona (how the model should think) is the first block: by default it is
// the built-in conservative analyst, but cfg.LLM.Prompt can override it —
// that is the lever the prompt-tune loop iterates. The answer contract (JSON
// shape, risk profile) is always appended by code, so a tuned persona can
// never break the parse contract: a reply without the JSON object is simply
// unusable and the strategy holds its last state.
func llmSystemPrompt(cfg config.Config) string {
	if persona := strings.TrimSpace(cfg.LLM.Prompt); persona != "" {
		return persona + "\n" + answerContract(cfg)
	}
	return builtinLLMPrompt(cfg)
}

// DefaultLLMPersona is the built-in persona, exposed so the prompt-tune loop
// can seed its first round with the text the strategy would actually use.
const DefaultLLMPersona = "You are a crypto technical analyst. You are given a compact snapshot of a price series and must decide the desired position on the next bar. Be conservative; when unsure, position=0."

func builtinLLMPrompt(cfg config.Config) string {
	return fmt.Sprintf(
		"You are a crypto technical analyst. You are given a compact snapshot of a "+
			"price series (%s, %s) and must decide the desired position on the next bar.\n"+
			"Answer ONLY with a JSON object: {\"position\": -1|0|1, \"stop\": <price|null>, \"target\": <price|null>, \"reason\": \"<short>\", \"confidence\": <0..1|null>}.\n"+
			"position: 1 = long, 0 = flat, -1 = short. stop/target are in the same units as price; use null when none.\n"+
			"Be conservative; when unsure, position=0.\n"+
			"confidence: your self-assessed certainty in [0,1]; report 0 when genuinely unsure.\n"+
			"Risk profile: max risk per trade %.2f%%, default stop loss %.2f%%, default take profit %.2f%%.",
		cfg.Agent.Symbol, cfg.Agent.Timeframe,
		cfg.Risk.MaxRiskPerTradePct*100, stopLossPct(cfg), takeProfitPct(cfg),
	)
}

// answerContract is the code-enforced tail every llm strategy prompt ends
// with, tuned persona or not.
func answerContract(cfg config.Config) string {
	return "Answer ONLY with a JSON object: {\"position\": -1|0|1, \"stop\": <price|null>, \"target\": <price|null>, \"reason\": \"<short>\", \"confidence\": <0..1|null>}.\n" +
		"position: 1 = long, 0 = flat, -1 = short. stop/target are in the same units as price; use null when none.\n" +
		"confidence: your self-assessed certainty in [0,1]; report null when unsure.\n" +
		fmt.Sprintf("Risk profile: max risk per trade %.2f%%, default stop loss %.2f%%, default take profit %.2f%%.",
			cfg.Risk.MaxRiskPerTradePct*100, stopLossPct(cfg), takeProfitPct(cfg))
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
