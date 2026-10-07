// Package tune is the LLM parameter-tuning loop. It is a single reusable
// implementation shared by the CLI (runTune) and the web dashboard (/api/tune):
// run a baseline backtest, then ask the model for improved parameter sets over
// several rounds, always comparing on one objective.
//
// The loop is pure logic: Run reports what happened via a Report, and callers
// render it (CLI prints to stdout, the web UI returns JSON). It degrades
// gracefully when the model is unavailable — the baseline still runs and the
// round loop reports that it was skipped.
package tune

import (
	"fmt"
	"strings"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/llm"
	"github.com/huijun/trading-agent-go/internal/metrics"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/strategy"
)

// Options configures one tuning run.
type Options struct {
	// Objective is the metric to maximize: sharpe, sortino, total_return,
	// profit_factor or win_rate.
	Objective string
	// Rounds is how many LLM proposal rounds to run after the baseline.
	Rounds int
	// Stall early-stops after this many consecutive non-improving rounds; 0
	// disables early stop.
	Stall int
	// NoClamp disables clamping model proposals into the strategy's legal
	// parameter ranges.
	NoClamp bool
	// Seed, when non-nil, is the starting parameter map (e.g. the values the
	// user picked in the web form). It layers over the strategy's documented
	// defaults.
	Seed map[string]float64
}

// RoundResult is one round of the loop (round 0 is the baseline).
type RoundResult struct {
	Index          int                `json:"index"`
	ObjectiveValue float64            `json:"objective_value"`
	Params         map[string]float64 `json:"params"`
	Rationale      string             `json:"rationale,omitempty"`
	// Improved is true when this round set a new best.
	Improved bool `json:"improved"`
	// Note carries a short human-readable status for rounds that did not
	// backtest normally (skipped, reverted, proposal failed).
	Note string `json:"note,omitempty"`
}

// Report is the full outcome of a tuning run.
type Report struct {
	Objective  string `json:"objective"`
	Strategy   string `json:"strategy"`
	Symbol     string `json:"symbol"`
	LLMEnabled bool   `json:"llm_enabled"`
	// EarlyStopped is true when the stall guard cut the loop short.
	EarlyStopped bool               `json:"early_stopped"`
	Rounds       []RoundResult      `json:"rounds"`
	Baseline     float64            `json:"baseline"`
	BestValue    float64            `json:"best_value"`
	BestParams   map[string]float64 `json:"best_params"`
}

// ObjectiveValue extracts the numeric value of a named objective from metrics;
// the second return is false when the metric is undefined for this run.
func ObjectiveValue(m metrics.Metrics, obj string) (float64, bool) {
	var p *float64
	switch obj {
	case "sharpe":
		p = m.Sharpe
	case "sortino":
		p = m.Sortino
	case "total_return":
		p = m.TotalReturnPct
	case "profit_factor":
		p = m.ProfitFactor
	case "win_rate":
		p = m.WinRatePct
	default:
		return 0, false
	}
	if p == nil {
		return 0, false
	}
	return *p, true
}

// MetricsSummary renders the headline metrics into a compact string the model
// can reason about when proposing the next parameter set.
func MetricsSummary(m metrics.Metrics) string {
	return fmt.Sprintf(
		"total=%s%% annual=%s%% sharpe=%s sortino=%s maxDD=%s%% trades=%d winrate=%s%% PF=%s",
		plainMetric(m.TotalReturnPct, 2), plainMetric(m.AnnualReturnPct, 2),
		plainMetric(m.Sharpe, 2), plainMetric(m.Sortino, 2),
		plainMetric(m.MaxDrawdownPct, 2), m.NumTrades,
		plainMetric(m.WinRatePct, 1), plainMetric(m.ProfitFactor, 2))
}

// FormatParams renders a parameter map deterministically (sorted by key).
func FormatParams(params map[string]float64) string {
	parts := make([]string, 0, len(params))
	for _, k := range strategy.SortKeys(params) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	return strings.Join(parts, " ")
}

// plainMetric renders a pointer metric the way the CLI does ("n/a" when null).
func plainMetric(v *float64, digits int) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.*f", digits, *v)
}

// Run executes the tuning loop over the shared series and returns what
// happened. It never writes files or prints; callers render the Report.
func Run(cfg config.Config, series model.Series, opts Options) (Report, error) {
	if opts.Objective == "" {
		opts.Objective = "sharpe"
	}
	if opts.Rounds < 0 {
		opts.Rounds = 0
	}

	spec, hasSpec := strategy.SpecFor(cfg.Strategy.Name)
	clamp := func(p map[string]float64) map[string]float64 {
		if hasSpec && !opts.NoClamp {
			return spec.ClampParams(p)
		}
		return p
	}

	// Seed the parameter map with the strategy's documented defaults, layer
	// any user-supplied seed on top, and clamp so a hand-edited value that
	// sits outside the strategy's legal range cannot poison the baseline.
	start := map[string]float64{}
	if hasSpec {
		start = spec.Defaults(cfg.Strategy.Params)
	} else {
		for k, v := range cfg.Strategy.Params {
			start[k] = v
		}
	}
	if opts.Seed != nil {
		for k, v := range opts.Seed {
			start[k] = v
		}
	}
	params := clamp(start)

	// runOnce backtests the given params against the shared series.
	runOnce := func(p map[string]float64) (engine.Result, float64, bool) {
		clone := cfg
		clone.Strategy.Params = map[string]float64{}
		for k, v := range p {
			clone.Strategy.Params[k] = v
		}
		strat, err := strategy.New(cfg.Strategy.Name, clone)
		if err != nil {
			return engine.Result{}, 0, false
		}
		res, err := engine.New(clone, strat).RunBacktest(series)
		if err != nil {
			return engine.Result{}, 0, false
		}
		value, ok := ObjectiveValue(res.Metrics, opts.Objective)
		return res, value, ok
	}

	// Baseline round.
	latest, baseVal, baseValid := runOnce(params)
	if !baseValid {
		return Report{}, fmt.Errorf("目标指标 %q 在基线结果里不可用（无交易或指标为 null）", opts.Objective)
	}
	bestParams := copyParams(params)
	bestVal := baseVal
	rounds := []RoundResult{{Index: 0, ObjectiveValue: baseVal, Params: copyParams(params)}}

	client := llm.New(cfg.LLM)
	earlyStopped := false
	stallCount := 0
	for i := 1; i <= opts.Rounds; i++ {
		if !client.Enabled() {
			rounds = append(rounds, RoundResult{Index: i, Note: "skipped: 未设置 LLM_API_KEY，无法生成新参数"})
			break
		}
		proposal, err := llm.Propose(cfg.LLM, opts.Objective, cfg.Strategy.Name, params, MetricsSummary(latest.Metrics))
		if err != nil {
			rounds = append(rounds, RoundResult{Index: i, Params: copyParams(params), Note: "提议失败，沿用当前参数 — " + err.Error()})
			continue
		}
		proposed := clamp(proposal.Params)
		res, val, valid := runOnce(proposed)
		if !valid {
			// No trades under the proposal: revert to the best-known params;
			// the last valid result is what the next round reasons from.
			params = copyParams(bestParams)
			stallCount++
			rounds = append(rounds, RoundResult{Index: i, Params: copyParams(params), Rationale: proposal.Rationale, Note: "该参数组合无交易，已还原"})
		} else {
			// Adopt the proposal as the new current state, whatever its score.
			params, latest = proposed, res
			if val > bestVal {
				bestParams, bestVal = copyParams(params), val
				stallCount = 0
				rounds = append(rounds, RoundResult{Index: i, ObjectiveValue: val, Params: copyParams(params), Rationale: proposal.Rationale, Improved: true})
			} else {
				stallCount++
				rounds = append(rounds, RoundResult{Index: i, ObjectiveValue: val, Params: copyParams(params), Rationale: proposal.Rationale})
			}
		}
		if opts.Stall > 0 && stallCount >= opts.Stall {
			earlyStopped = true
			rounds = append(rounds, RoundResult{Index: -1, Note: fmt.Sprintf("早停: 连续 %d 轮没有改进 best", opts.Stall)})
			break
		}
	}

	return Report{
		Objective:    opts.Objective,
		Strategy:     cfg.Strategy.Name,
		Symbol:       cfg.Agent.Symbol,
		LLMEnabled:   client.Enabled(),
		EarlyStopped: earlyStopped,
		Rounds:       rounds,
		Baseline:     baseVal,
		BestValue:    bestVal,
		BestParams:   bestParams,
	}, nil
}

// copyParams returns a shallow copy of a parameter map so the loop never
// mutates a caller's map in place.
func copyParams(p map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
