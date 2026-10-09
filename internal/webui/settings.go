package webui

import (
	"fmt"
	"math"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

func settingsView(cfg config.Config, interval int, execute bool) *StartSessionRequest {
	params := map[string]float64{}
	for k, v := range cfg.Strategy.Params {
		params[k] = v
	}
	return &StartSessionRequest{BacktestRequest: BacktestRequest{
		Symbol: cfg.Agent.Symbol, Strategy: cfg.Strategy.Name, Params: params,
		Days: cfg.Live.LookbackDays, InitialCash: cfg.Risk.InitialCash,
		Risk: &RiskOverrides{MaxPositionPct: &cfg.Risk.MaxPositionPct, MaxRiskPerTradePct: &cfg.Risk.MaxRiskPerTradePct,
			StopLossPct: cfg.Risk.StopLossPct, TakeProfitPct: cfg.Risk.TakeProfitPct, MaxDrawdownPct: cfg.Risk.MaxDrawdownPct,
			MaxDailyLossPct: cfg.Risk.MaxDailyLossPct, AllowShort: &cfg.Risk.AllowShort,
			CommissionBps: &cfg.Execution.CommissionBps, SlippageBps: &cfg.Execution.SlippageBps}},
		IntervalSeconds: interval, Execute: execute, Futures: true, Leverage: cfg.Risk.Leverage}
}

func validateSessionConfig(cfg config.Config, interval int) error {
	if cfg.Agent.Symbol == "" {
		return fmt.Errorf("请填写交易对")
	}
	if !marketSymbolPattern.MatchString(cfg.Agent.Symbol) {
		return fmt.Errorf("交易对格式无效，例如 BTCUSDT")
	}
	if cfg.Live.LookbackDays < 30 || cfg.Live.LookbackDays > 5000 {
		return fmt.Errorf("回看范围为 30–5000 天")
	}
	if interval != 0 && (interval < 5 || interval > 86400) {
		return fmt.Errorf("轮询间隔为 5–86400 秒")
	}
	if cfg.Risk.InitialCash <= 0 || math.IsInf(cfg.Risk.InitialCash, 0) {
		return fmt.Errorf("纸面资金必须大于 0")
	}
	if cfg.Risk.MaxPositionPct <= 0 || cfg.Risk.MaxPositionPct > 1 || cfg.Risk.MaxRiskPerTradePct <= 0 || cfg.Risk.MaxRiskPerTradePct > 1 {
		return fmt.Errorf("仓位与单笔风险必须大于 0 且不超过 100%%")
	}
	for _, pct := range []*float64{cfg.Risk.StopLossPct, cfg.Risk.MaxDrawdownPct, cfg.Risk.MaxDailyLossPct} {
		if pct != nil && (*pct <= 0 || *pct >= 1) {
			return fmt.Errorf("止损、回撤、日亏损上限必须大于 0 且小于 100%%；留空表示不设置")
		}
	}
	if cfg.Risk.TakeProfitPct != nil && (*cfg.Risk.TakeProfitPct <= 0 || *cfg.Risk.TakeProfitPct > 5) {
		return fmt.Errorf("止盈必须在 0–500%% 范围内且大于 0")
	}
	if cfg.Execution.CommissionBps < 0 || cfg.Execution.SlippageBps < 0 {
		return fmt.Errorf("费用与滑点不能为负")
	}
	for _, spec := range strategy.Specs() {
		if spec.Name != cfg.Strategy.Name {
			continue
		}
		for _, param := range spec.Params {
			if v, ok := cfg.Strategy.Params[param.Key]; ok && (v < param.Min || v > param.Max) {
				return fmt.Errorf("策略参数 %s 超出范围", param.Label)
			}
		}
	}
	if cfg.Strategy.Name == "ma_cross" && cfg.IntParam("fast", 10) >= cfg.IntParam("slow", 30) {
		return fmt.Errorf("快线周期必须小于慢线周期")
	}
	return nil
}
