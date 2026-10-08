package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
)

// TestStatusExposesAIFields locks the AI's face on the wire: the console
// relies on ai_model (which model is deciding) and ai_reason (its own
// explanation of the latest call) showing up on the polled status, with
// omitempty so indicator strategies send neither key at all.
func TestStatusExposesAIFields(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Model = "gpt-5.4"
	cfg.Strategy.Name = "llm"

	s := NewSession()
	s.cfg = cfg
	s.cycles = 1
	s.lastReason = "RSI 超卖后金叉，维持空仓观望"
	s.lastTick = time.Now()

	st := s.Status()
	if st.AIModel != "gpt-5.4" {
		t.Fatalf("AIModel = %q, want the configured model name", st.AIModel)
	}
	if st.AIReason != "RSI 超卖后金叉，维持空仓观望" {
		t.Fatalf("AIReason = %q, want the model's latest explanation", st.AIReason)
	}

	// Both keys must be present on the JSON the console actually parses.
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if wire["ai_model"] != "gpt-5.4" || wire["ai_reason"] != "RSI 超卖后金叉，维持空仓观望" {
		t.Fatalf("wire = ai_model:%v ai_reason:%v, want both model and reason", wire["ai_model"], wire["ai_reason"])
	}

	// No model configured and no reason yet: the keys stay off the wire.
	s2 := NewSession()
	s2.cfg = config.Default()
	s2.cfg.LLM.Model = ""
	st2 := s2.Status()
	b2, err := json.Marshal(st2)
	if err != nil {
		t.Fatalf("marshal status2: %v", err)
	}
	var wire2 map[string]any
	if err := json.Unmarshal(b2, &wire2); err != nil {
		t.Fatalf("unmarshal wire2: %v", err)
	}
	if _, present := wire2["ai_model"]; present {
		t.Fatalf("ai_model must be omitted when the model is unconfigured")
	}
	if _, present := wire2["ai_reason"]; present {
		t.Fatalf("ai_reason must be omitted before the model has decided")
	}
}
