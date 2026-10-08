// The prompt-tuning proposer: given the current trading persona of the llm
// strategy, its objective and the last backtest metrics, the model proposes a
// better persona. This is the step that lets the AI iterate on its own
// decision framework instead of on numbers: the persona is data, it is judged
// by the backtest, and a worse one simply never becomes the best.
//
// The proposer stays engine-free (it returns a plain text persona) so the
// tuning loop can live in the tune package without an import cycle.
package llm

import (
	"fmt"
	"strings"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// MaxPromptRunes caps the length of a persona the proposer may return. The
// per-bar prompt already carries the price snapshot and the answer contract;
// a persona longer than this just bills tokens and dilutes the instructions.
const MaxPromptRunes = 8000

// PromptProposal is one round's answer from the prompt-tuning model.
type PromptProposal struct {
	Persona   string
	Rationale string
}

// ProposePrompt asks the model for a better trading persona. Like Propose it
// is fail-loud: a disabled model or a non-JSON / empty / oversized persona
// returns an error so the loop skips the round instead of adopting junk.
func ProposePrompt(cfg config.LLM, symbol, timeframe, objective, currentPersona, currentMetrics string) (PromptProposal, error) {
	client := New(cfg)
	if !client.Enabled() {
		return PromptProposal{}, fmt.Errorf("LLM 迭代提示词需要 API key（设置 LLM_API_KEY 或 OPENAI_API_KEY）")
	}

	user := "Symbol: " + symbol + "\n" +
		"Timeframe: " + timeframe + "\n" +
		"Objective: maximize " + objective + "\n" +
		"Current trading persona (the text given to the model as its system prompt):\n" +
		"---\n" + currentPersona + "\n---\n" +
		"Last backtest metrics: " + currentMetrics + "\n" +
		"Rewrite the trading persona so a model following it would improve " + objective + " on this market. " +
		"Keep it 2-6 sentences of concrete trading judgment (what to read in the indicators, when to be aggressive, when to stay flat). " +
		"Do not describe the JSON answer format and do not add rules the model cannot act on. " +
		"Answer ONLY with JSON: {\"persona\": \"<new persona text>\", \"rationale\": \"<short>\"}"

	var payload struct {
		Persona   string `json:"persona"`
		Rationale string `json:"rationale"`
	}
	if err := client.CompleteJSON(promptTuneSystemPrompt(), user, &payload); err != nil {
		return PromptProposal{}, err
	}
	persona := strings.TrimSpace(payload.Persona)
	if persona == "" {
		return PromptProposal{}, fmt.Errorf("模型返回的 persona 为空")
	}
	if runes := []rune(persona); len(runes) > MaxPromptRunes {
		return PromptProposal{}, fmt.Errorf("persona 过长（%d 字符，上限 %d）", len(runes), MaxPromptRunes)
	}
	return PromptProposal{Persona: persona, Rationale: payload.Rationale}, nil
}

func promptTuneSystemPrompt() string {
	return "You improve the trading persona of an LLM trading strategy. The " +
		"persona is the only part of the prompt you may change; the JSON answer " +
		"contract is enforced by code and must not be restated in the persona. " +
		"Prefer small, explainable changes over rewriting from scratch, and say " +
		"what specifically should change and why. Do not write prose outside the " +
		"JSON object."
}
