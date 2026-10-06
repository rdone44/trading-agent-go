package report

import (
	"fmt"
	"strings"
)

// The engine emits stable English codes for sides, exit reasons and risk
// events. The report page is Chinese, so these map the codes to labels while
// falling back to the raw value so nothing is ever hidden.

var sideLabels = map[string]string{
	"buy":  "买入",
	"sell": "卖出",
}

var reasonLabels = map[string]string{
	"entry_long":      "开多",
	"entry_short":     "开空",
	"signal_exit":     "信号离场",
	"stop_loss":       "止损",
	"take_profit":     "止盈",
	"end of backtest": "回测结束平仓",
}

var riskTypeLabels = map[string]string{
	"halt":             "熔断停止交易",
	"daily_loss_limit": "单日亏损超限",
}

func sideLabel(side any) string {
	key := strings.ToLower(strings.TrimSpace(toString(side)))
	if label, ok := sideLabels[key]; ok {
		return label
	}
	return key
}

// reasonLabel handles the fixed codes plus the parameterised ones the engine
// builds at runtime, such as "risk halt: max drawdown breached (-25.0%)".
func reasonLabel(reason any) string {
	text := toString(reason)
	if rest, ok := strings.CutPrefix(text, "risk halt: "); ok {
		return "风控熔断：" + reasonLabel(rest)
	}
	if label, ok := reasonLabels[text]; ok {
		return label
	}
	switch {
	case strings.HasPrefix(text, "max drawdown breached"):
		return "触发最大回撤限制"
	case strings.HasPrefix(text, "daily loss "):
		return "单日亏损超限，暂停开仓"
	}
	return text
}

func riskTypeLabel(kind any) string {
	key := strings.TrimSpace(toString(kind))
	if label, ok := riskTypeLabels[key]; ok {
		return label
	}
	return key
}

// toString accepts the string-ish types the templates pass in, including
// named string types such as broker.Side.
func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
