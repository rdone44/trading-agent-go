// The LLM second-opinion gate and post-mortem reviewer. Both reuse the same
// OpenAI-compatible client. The gate is fail-open: any error or a missing key
// lets the entry through, because a down model must never silently block a
// stop-loss-adjacent trade. Only an explicit model refusal blocks.
//
// This package deliberately does NOT import engine: the engine only
// references an engine.Veto function value, and the live runner adapts it to
// llm.VetoDecision. Keeping the import one-way avoids an engine<->llm cycle
// (engine imports strategy, strategy imports llm).
package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/huijun/trading-agent-go/internal/config"
)

// VetoRequest is the plain, engine-free description of one proposed entry that
// the gate reviews. The live runner fills it from an engine.VetoContext.
type VetoRequest struct {
	Side        string
	Symbol      string
	Quantity    float64
	EntryPrice  float64
	StopPrice   float64
	TargetPrice float64
	Equity      float64
	Cash        float64
	Leverage    int
	Reason      string
}

// VetoDecision consults the model about one entry. It is fail-open: a disabled
// client, a transport error or an unparseable answer all mean "let it through"
// (blocked=false). Only an explicit model refusal returns blocked=true.
func VetoDecision(cfg config.LLM, req VetoRequest) (blocked bool, reason string) {
	client := New(cfg)
	if !client.Enabled() {
		return false, ""
	}
	answer, err := client.Complete(vetoSystemPrompt(), vetoUserPrompt(req))
	if err != nil {
		return false, ""
	}
	var verdict struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSON(answer)), &verdict); err != nil {
		return false, ""
	}
	if !verdict.Approve {
		return true, strings.TrimSpace(verdict.Reason)
	}
	return false, ""
}

// Review turns a finished run into a short, plain-language post-mortem. The
// caller supplies a compact facts blob (symbol, window, key metrics, a few of
// the largest trades); this formats the prompt and returns the model text.
// When no key is set it returns an error so the caller can fall back to a
// "LLM review unavailable" note.
func Review(cfg config.LLM, facts string) (string, error) {
	client := New(cfg)
	if !client.Enabled() {
		return "", fmt.Errorf("LLM 复盘不可用（没有 API key）")
	}
	return client.Complete(
		"You are a trading post-mortem writer. Given the factual summary of a "+
			"finished run, write a concise Chinese review: what happened, whether "+
			"the strategy's edge was confirmed, and 2-3 concrete risks or changes "+
			"to watch. Be specific to the numbers given; do not invent metrics.",
		facts,
	)
}

func vetoSystemPrompt() string {
	return "You are a risk officer. You are shown a proposed new trade and the " +
		"account's state. Approve it unless the risk is clearly unacceptable. " +
		"Answer ONLY with JSON: {\"approve\": true|false, \"reason\": \"<short>\"}."
}

func vetoUserPrompt(req VetoRequest) string {
	return fmt.Sprintf(
		"Proposed trade: side=%s symbol=%s quantity=%.6f entry=%.2f stop=%.2f target=%.2f\n"+
			"Account: equity=%.2f cash=%.2f leverage=%d\nReason given by strategy: %s\n"+
			"Approve?",
		req.Side, req.Symbol, req.Quantity, req.EntryPrice, req.StopPrice, req.TargetPrice,
		req.Equity, req.Cash, req.Leverage, req.Reason,
	)
}
