// RunPrompt is the L2 iteration loop: the AI iterates on its own decision
// framework (the llm strategy's trading persona) instead of on numbers.
//
// One round: the model rewrites the persona given the objective and the last
// backtest's metrics; the loop backtests the new persona on the same series
// and keeps it as the new current state whenever it does not regress the
// best-so-far, exactly like Run keeps the best param set. Cross-validation
// works the same way: disjoint validation windows plus a final holdout judge
// the winning persona, and a persona that only memorised the training window
// is rejected and the baseline is restored.
//
// The loop is bounded (Rounds + stall guard) and degrades like Run: without
// an API key the baseline still runs and the loop reports the model as
// disabled.
package tune

import (
	"fmt"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/engine"
	"github.com/rdone44/trading-agent-go/internal/llm"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// PromptOptions configures one prompt-tuning run.
type PromptOptions struct {
	// Objective is the metric to maximize (same vocabulary as Options).
	Objective string
	// Rounds is how many persona-rewrite rounds to run after the baseline.
	Rounds int
	// Stall early-stops after this many consecutive non-improving rounds;
	// 0 disables early stop.
	Stall int
	// StartPersona, when non-empty, overrides the persona the loop starts
	// from (normally cfg.LLM.Prompt or the built-in persona).
	StartPersona string
	// CVFolds enables disjoint validation windows plus a final holdout,
	// judged the same way Run judges parameter winners.
	CVFolds int
}

// PromptRoundResult is one round of the loop (round 0 is the baseline).
type PromptRoundResult struct {
	Index          int    `json:"index"`
	ObjectiveValue float64 `json:"objective_value"`
	// Persona is the full text evaluated in this round.
	Persona   string `json:"persona,omitempty"`
	Rationale string `json:"rationale,omitempty"`
	Improved  bool   `json:"improved"`
	Note      string `json:"note,omitempty"`
}

// PromptReport is the full outcome of a prompt-tuning run.
type PromptReport struct {
	Objective    string `json:"objective"`
	Symbol       string `json:"symbol"`
	LLMEnabled   bool   `json:"llm_enabled"`
	EarlyStopped bool   `json:"early_stopped"`
	Rounds       []PromptRoundResult `json:"rounds"`
	Baseline     float64            `json:"baseline"`
	BestValue    float64            `json:"best_value"`
	// BestPrompt is the persona that won; callers persist it into
	// cfg.LLM.Prompt (or the per-user vault) to adopt the result.
	BestPrompt   string               `json:"best_prompt"`
	Validation   *PromptValidationReport `json:"validation,omitempty"`
}

// PromptValidationReport reuses the window layout of parameter validation.
type PromptValidationReport struct {
	Accepted bool           `json:"accepted"`
	Windows  []WindowResult `json:"windows"`
}

// RunPrompt executes the prompt-tuning loop over the shared series and
// returns what happened. It is a pure loop: it never writes files, never
// mutates cfg, and reports via PromptReport. The strategy must be "llm" —
// the persona is the llm strategy's system-prompt persona, nothing else
// reads it.
func RunPrompt(cfg config.Config, series model.Series, opts PromptOptions) (PromptReport, error) {
	if cfg.Strategy.Name != "llm" {
		return PromptReport{}, fmt.Errorf("提示词迭代只适用于 llm 策略（当前: %q）", cfg.Strategy.Name)
	}
	if opts.Objective == "" {
		opts.Objective = "sharpe"
	}
	if opts.Rounds < 0 {
		opts.Rounds = 0
	}

	training, windows, err := splitCV(series, opts.CVFolds, cfg.Backtest.WarmupBars)
	if err != nil {
		return PromptReport{}, err
	}
	original := series
	series = training

	baselinePersona := opts.StartPersona
	if baselinePersona == "" {
		baselinePersona = cfg.LLM.Prompt
	}
	if baselinePersona == "" {
		baselinePersona = strategy.DefaultLLMPersona
	}

	// runOnce backtests the llm strategy with the given persona on the
	// shared series. Only cfg.LLM.Prompt changes between rounds.
	runOnce := func(persona string) (engine.Result, float64, bool) {
		clone := cfg
		clone.LLM.Prompt = persona
		strat, err := strategy.New("llm", clone)
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

	latest, baseVal, baseValid := runOnce(baselinePersona)
	if !baseValid {
		return PromptReport{}, fmt.Errorf("目标指标 %q 在基线结果里不可用（无交易或指标为 null）", opts.Objective)
	}
	bestPersona := baselinePersona
	bestVal := baseVal
	rounds := []PromptRoundResult{{Index: 0, ObjectiveValue: baseVal, Persona: baselinePersona}}

	client := llm.New(cfg.LLM)
	earlyStopped := false
	stallCount := 0
	currentPersona := baselinePersona
	for i := 1; i <= opts.Rounds; i++ {
		if !client.Enabled() {
			rounds = append(rounds, PromptRoundResult{Index: i, Persona: currentPersona, Note: "skipped: 未设置 LLM_API_KEY，无法生成新提示词"})
			break
		}
		proposal, err := llm.ProposePrompt(cfg.LLM, cfg.Agent.Symbol, cfg.Agent.Timeframe,
			opts.Objective, currentPersona, MetricsSummary(latest.Metrics))
		if err != nil {
			rounds = append(rounds, PromptRoundResult{Index: i, Persona: currentPersona, Note: "提议失败，沿用当前提示词 — " + err.Error()})
			continue
		}
		res, val, valid := runOnce(proposal.Persona)
		if !valid {
			// No trades under the new persona: revert to the best-known
			// persona; the last valid result is what the next round
			// reasons from.
			stallCount++
			rounds = append(rounds, PromptRoundResult{Index: i, Persona: currentPersona, Rationale: proposal.Rationale, Note: "该提示词无交易，已还原"})
		} else {
			// Adopt the rewrite as the new current state, whatever its
			// score, so the loop reasons from what it actually evaluated.
			currentPersona, latest = proposal.Persona, res
			if val > bestVal {
				bestPersona, bestVal = currentPersona, val
				stallCount = 0
				rounds = append(rounds, PromptRoundResult{Index: i, ObjectiveValue: val, Persona: currentPersona, Rationale: proposal.Rationale, Improved: true})
			} else {
				stallCount++
				rounds = append(rounds, PromptRoundResult{Index: i, ObjectiveValue: val, Persona: currentPersona, Rationale: proposal.Rationale})
			}
		}
		if opts.Stall > 0 && stallCount >= opts.Stall {
			earlyStopped = true
			rounds = append(rounds, PromptRoundResult{Index: -1, Note: fmt.Sprintf("早停: 连续 %d 轮没有改进 best", opts.Stall)})
			break
		}
	}

	var validation *PromptValidationReport
	if opts.CVFolds > 0 {
		validation = validatePromptWinner(cfg, original, windows, baselinePersona, bestPersona, opts.Objective)
		if !validation.Accepted {
			bestPersona, bestVal = baselinePersona, baseVal
			for i := range rounds {
				rounds[i].Improved = false
			}
			rounds = append(rounds, PromptRoundResult{Index: -1, Note: "cross-validation rejected winner; restored baseline"})
		}
	}
	return PromptReport{
		Validation:   validation,
		Objective:    opts.Objective,
		Symbol:       cfg.Agent.Symbol,
		LLMEnabled:   client.Enabled(),
		EarlyStopped: earlyStopped,
		Rounds:       rounds,
		Baseline:     baseVal,
		BestValue:    bestVal,
		BestPrompt:   bestPersona,
	}, nil
}

// validatePromptWinner reuses the parameter-validation machinery's window
// layout and decision rule, evaluating personas instead of param maps.
func validatePromptWinner(cfg config.Config, series model.Series, windows []WindowResult, baseline, winner string, objective string) *PromptValidationReport {
	report := &PromptValidationReport{Windows: append([]WindowResult(nil), windows...)}
	evaluate := func(persona string, window model.Series) (float64, bool) {
		clone := cfg
		clone.LLM.Prompt = persona
		strat, err := strategy.New(clone.Strategy.Name, clone)
		if err != nil {
			return 0, false
		}
		result, err := engine.New(clone, strat).RunBacktest(window)
		if err != nil {
			return 0, false
		}
		value, ok := ObjectiveValue(result.Metrics, objective)
		return value, ok && model.Valid(value)
	}
	for i := range report.Windows {
		w := &report.Windows[i]
		window := series
		window.Bars = series.Bars[w.Start:w.End]
		base, baseOK := evaluate(baseline, window)
		candidate, candidateOK := evaluate(winner, window)
		w.Valid = baseOK && candidateOK
		if baseOK {
			w.Baseline = base
		}
		if candidateOK {
			w.Winner = candidate
		}
		w.Passed = w.Valid && candidate >= base
	}
	report.Accepted = acceptsValidation(report.Windows)
	return report
}
