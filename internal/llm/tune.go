// The parameter-tuning proposer: given an objective, the strategy name, the
// current parameter set and its metrics, the model proposes a new parameter
// set. The caller runs a backtest with the proposal and feeds the result back
// on the next round. This package stays engine-free (it returns a plain
// params map) so the tuning loop can live in the CLI without an import cycle.
package llm

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// Proposal is one round's answer from the tuning model.
type Proposal struct {
	Params    map[string]float64
	Rationale string
}

// Propose asks the model for a new parameter set. It is fail-loud (returns an
// error when the model is disabled or the answer does not parse) so the
// tuning loop can skip a bad round rather than silently keep old params.
func Propose(cfg config.LLM, objective, strat string, current map[string]float64, currentMetrics string) (Proposal, error) {
	client := New(cfg)
	if !client.Enabled() {
		return Proposal{}, fmt.Errorf("LLM 调参需要 API key（设置 LLM_API_KEY 或 OPENAI_API_KEY）")
	}

	currentJSON, _ := json.Marshal(current)
	user := fmt.Sprintf(
		"Strategy: %s\nObjective: maximize %s\n"+
			"Current params: %s\nCurrent metrics: %s\n"+
			"Propose a NEW set of params likely to improve %s. Keep every value "+
			"a finite number within a sensible range for this strategy. "+
			"Answer ONLY with JSON: {\"params\": {\"<name>\": <number>, ...}, \"rationale\": \"<short>\"}",
		strat, objective, string(currentJSON), currentMetrics, objective)

	var payload struct {
		Params    map[string]json.Number `json:"params"`
		Rationale string                 `json:"rationale"`
	}
	if err := client.CompleteJSON(tuneSystemPrompt(), user, &payload); err != nil {
		return Proposal{}, err
	}
	params := make(map[string]float64, len(payload.Params))
	for k, v := range payload.Params {
		f, err := v.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return Proposal{}, fmt.Errorf("参数 %q 不是有限数值: %v", k, v.String())
		}
		params[k] = f
	}
	return Proposal{Params: params, Rationale: payload.Rationale}, nil
}

func tuneSystemPrompt() string {
	return "You are optimizing a trading strategy. Propose concrete numeric " +
		"parameter values; do not add prose outside the JSON object. Prefer " +
		"modest, explainable changes over wild swings."
}
