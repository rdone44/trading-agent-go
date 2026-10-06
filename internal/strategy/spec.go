package strategy

// ParamSpec describes one tunable strategy parameter so a UI can render a
// labelled input with a sensible default and step size.
type ParamSpec struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Default float64 `json:"default"`
	Step    float64 `json:"step"`
	Hint    string  `json:"hint,omitempty"`
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
				{Key: "fast", Label: "快线周期", Default: 10, Step: 1, Hint: "根K线"},
				{Key: "slow", Label: "慢线周期", Default: 30, Step: 1, Hint: "根K线"},
				{Key: "min_gap_pct", Label: "死区阈值", Default: 0, Step: 0.1, Hint: "% 差值"},
			},
		},
		{
			Name:        "rsi_reversion",
			Title:       "RSI 均值回归",
			Description: "RSI 超卖时买入，动量恢复后离场，下方用 ATR 止损保护。",
			Params: []ParamSpec{
				{Key: "period", Label: "RSI 周期", Default: 14, Step: 1, Hint: "根K线"},
				{Key: "lower", Label: "买入阈值", Default: 30, Step: 1, Hint: "低于该 RSI"},
				{Key: "exit_level", Label: "离场阈值", Default: 55, Step: 1, Hint: "高于该 RSI"},
				{Key: "atr_period", Label: "ATR 周期", Default: 14, Step: 1, Hint: "根K线"},
				{Key: "atr_stop_mult", Label: "ATR 止损", Default: 2.5, Step: 0.1, Hint: "倍 ATR"},
			},
		},
		{
			Name:        "breakout",
			Title:       "唐奇安通道突破",
			Description: "突破新高买入，跌破通道下沿或触发 ATR 移动止损时离场。",
			Params: []ParamSpec{
				{Key: "lookback", Label: "突破回看周期", Default: 20, Step: 1, Hint: "根K线"},
				{Key: "exit_lookback", Label: "离场回看周期", Default: 10, Step: 1, Hint: "根K线"},
				{Key: "atr_period", Label: "ATR 周期", Default: 14, Step: 1, Hint: "根K线"},
				{Key: "atr_stop_mult", Label: "ATR 止损", Default: 2.0, Step: 0.1, Hint: "倍 ATR"},
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
