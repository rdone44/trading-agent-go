package strategy

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// ParamSpec describes one tunable strategy parameter so a UI can render a
// labelled input with a sensible default and step size, and a tuning loop can
// keep the model's proposals inside a sane range.
type ParamSpec struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Default float64 `json:"default"`
	Step    float64 `json:"step"`
	// Min/Max bound the legal range; 0 means "no bound on that side". The
	// tuner clamps model proposals into [Min, Max] so a wild suggestion can
	// never produce a degenerate strategy (period=0, negative lookback).
	Min float64 `json:"min,omitempty"`
	Max float64 `json:"max,omitempty"`
	// Integer marks parameters that only make sense whole (bar counts, RSI
	// levels): the tuner rounds them to the nearest integer.
	Integer bool   `json:"-"`
	Hint    string `json:"hint,omitempty"`
}

// Spec is the user-facing description of a strategy.
type Spec struct {
	Name        string      `json:"name"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Params      []ParamSpec `json:"params"`
}

// Specs returns the built-in strategies with their parameters, in a stable
// order suitable for a menu.
func Specs() []Spec {
	return []Spec{
		{
			Name:        "ma_cross",
			Title:       "双均线交叉",
			Description: "快线上穿慢线时持有多头，下穿后回到空仓。",
			Params: []ParamSpec{
				{Key: "fast", Label: "快线周期", Default: 10, Step: 1, Min: 2, Max: 200, Integer: true, Hint: "根K线"},
				{Key: "slow", Label: "慢线周期", Default: 30, Step: 1, Min: 5, Max: 500, Integer: true, Hint: "根K线"},
				{Key: "min_gap_pct", Label: "死区阈值", Default: 0, Step: 0.1, Min: 0, Max: 5, Hint: "% 差值"},
			},
		},
		{
			Name:        "rsi_reversion",
			Title:       "RSI 均值回归",
			Description: "RSI 超卖时买入，动量恢复后离场，下方用 ATR 止损保护。",
			Params: []ParamSpec{
				{Key: "period", Label: "RSI 周期", Default: 14, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "lower", Label: "买入阈值", Default: 30, Step: 1, Min: 5, Max: 50, Integer: true, Hint: "低于该 RSI"},
				{Key: "exit_level", Label: "离场阈值", Default: 55, Step: 1, Min: 50, Max: 95, Integer: true, Hint: "高于该 RSI"},
				{Key: "atr_period", Label: "ATR 周期", Default: 14, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "atr_stop_mult", Label: "ATR 止损", Default: 2.5, Step: 0.1, Min: 0.5, Max: 10, Hint: "倍 ATR"},
			},
		},
		{
			Name:        "breakout",
			Title:       "唐奇安通道突破",
			Description: "突破新高买入，跌破通道下沿或触发 ATR 移动止损时离场。",
			Params: []ParamSpec{
				{Key: "lookback", Label: "突破回看周期", Default: 20, Step: 1, Min: 2, Max: 200, Integer: true, Hint: "根K线"},
				{Key: "exit_lookback", Label: "离场回看周期", Default: 10, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "atr_period", Label: "ATR 周期", Default: 14, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "atr_stop_mult", Label: "ATR 止损", Default: 2.0, Step: 0.1, Min: 0.5, Max: 10, Hint: "倍 ATR"},
			},
		},
		{
			// The LLM strategy's tunable knobs. The indicator periods feed its
			// prompt; llm_step/llm_window control how often and with how much
			// context the model is asked.
			Name:        "llm",
			Title:       "LLM 目标仓位",
			Description: "模型读取紧凑的K线快照并给出目标仓位，配合可选止损/目标位。",
			Params: []ParamSpec{
				{Key: "fast", Label: "快线周期", Default: 10, Step: 1, Min: 2, Max: 200, Integer: true, Hint: "根K线"},
				{Key: "slow", Label: "慢线周期", Default: 30, Step: 1, Min: 5, Max: 500, Integer: true, Hint: "根K线"},
				{Key: "period", Label: "RSI 周期", Default: 14, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "atr_period", Label: "ATR 周期", Default: 14, Step: 1, Min: 2, Max: 100, Integer: true, Hint: "根K线"},
				{Key: "llm_step", Label: "模型调用步长", Default: 1, Step: 1, Min: 1, Max: 50, Integer: true, Hint: "每 k 根K线问一次模型"},
				{Key: "llm_window", Label: "模型上下文窗口", Default: 30, Step: 1, Min: 5, Max: 200, Integer: true, Hint: "送入模型的收盘价根数"},
			},
		},
	}
}

// SpecFor looks up one strategy by name.
func SpecFor(name string) (Spec, bool) {
	for _, spec := range Specs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return Spec{}, false
}

// Defaults returns a parameter map seeded from the spec: every documented
// parameter at its default, then user-supplied values layered on top.
func (s Spec) Defaults(overrides map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(s.Params))
	for _, p := range s.Params {
		out[p.Key] = p.Default
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ClampParams keeps a proposed parameter set inside the strategy's legal
// ranges: values are clamped into [Min, Max], integer parameters are rounded
// to the nearest whole number, and every value is snapped to the parameter's
// step grid. Unknown keys are passed through untouched (the strategy decides
// whether it understands them).
func (s Spec) ClampParams(params map[string]float64) map[string]float64 {
	bounds := make(map[string]ParamSpec, len(s.Params))
	for _, p := range s.Params {
		bounds[p.Key] = p
	}
	for key, v := range params {
		spec, ok := bounds[key]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if spec.Integer {
			v = math.Round(v)
		}
		if spec.Step > 0 {
			v = math.Round(v/spec.Step) * spec.Step
			// Snap-to-step leaves a binary floating-point tail (7.3 ->
			// 7.300000000000001); round to the step's own precision so the
			// value prints and compares cleanly.
			v = roundToStep(v, spec.Step)
		}
		if spec.Min > 0 && v < spec.Min {
			v = spec.Min
		}
		if spec.Max > 0 && v > spec.Max {
			v = spec.Max
		}
		params[key] = v
	}
	return params
}

// roundToStep cleans a binary floating-point tail off a value that has been
// snapped to a step grid, to the step's own decimal precision. A step of 0.1
// therefore returns 7.3 (not 7.300000000000001); a step of 1 returns the
// value unchanged.
func roundToStep(v, step float64) float64 {
	digits := decimalDigits(step)
	scale := math.Pow(10, float64(digits))
	return math.Round(v*scale) / scale
}

// decimalDigits counts the number of decimal places in a step value (0.1 -> 1,
// 0.01 -> 2, 1 -> 0). It reads the shortest round-trip spelling, which is
// what a user actually wrote into the spec.
func decimalDigits(step float64) int {
	s := strconv.FormatFloat(step, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		return 0
	}
	frac := s[strings.Index(s, ".")+1:]
	return len(frac)
}

// SortKeys returns the parameter keys in deterministic (sorted) order, for
// stable rendering of parameter sets.
func SortKeys(params map[string]float64) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
