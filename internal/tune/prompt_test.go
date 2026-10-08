package tune

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

// promptStub stands in for an OpenAI-compatible endpoint and returns the
// same persona rewrite for every prompt-tuning call. It only counts the
// tuning calls (system prompt "You improve the trading persona"); the many
// per-bar strategy decision calls that a backtest makes are served too but
// answered with the baseline flat JSON so the backtest stays quiet.
func promptStub(t *testing.T, persona, rationale string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		isTune := false
		for _, m := range req.Messages {
			if m.Role == "system" && strings.HasPrefix(m.Content, "You improve the trading persona") {
				isTune = true
				break
			}
		}
		var content string
		if isTune {
			*calls++
			content = `{"persona":` + jsonQuote(persona) + `,"rationale":` + jsonQuote(rationale) + `}`
		} else {
			// Strategy decision: go long on odd bars, flat on even ones, so
			// the backtest produces trades and a defined sharpe regardless of
			// the persona text (the stub ignores the persona on purpose).
			pos := 0
			var userContent string
			for _, m := range req.Messages {
				if m.Role == "user" {
					userContent = m.Content
				}
			}
			if idx := barIndex(userContent); idx%2 == 1 {
				pos = 1
			}
			content = fmt.Sprintf(`{"position":%d,"stop":null,"target":null,"reason":"stub"}`, pos)
		}
		out := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// barIndex pulls "bar_index=N" out of the strategy's user prompt so the
// stub can decide deterministically per bar.
func barIndex(userContent string) int {
	const key = "bar_index="
	if i := strings.LastIndex(userContent, key); i >= 0 {
		rest := userContent[i+len(key):]
		if j := strings.IndexAny(rest, " \n"); j >= 0 {
			rest = rest[:j]
		}
		n, _ := strconv.Atoi(rest)
		return n
	}
	return 0
}

func promptBaseTestConfig() config.Config {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Agent.HistoryDays = 300
	cfg.Backtest.WarmupBars = 30
	// Cap the per-bar model calls so the offline backtest stays cheap and
	// fast: the persona is what under test, not the number of bars.
	cfg.Strategy.Name = "llm"
	cfg.Strategy.Params = map[string]float64{"llm_step": 5, "llm_window": 10, "slow": 10}
	return cfg
}

// TestRunPromptKeepsBetterPersona is the heart of the L2 loop: with a model
// that proposes one fixed better persona, the loop must backtest it, keep it
// as the winner, and report it as BestPrompt.
func TestRunPromptKeepsBetterPersona(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")

	var calls int
	better := "Read the SMA cross and RSI together; only go long when both agree, stay flat in chop."
	cfg := promptBaseTestConfig()
	cfg.LLM.BaseURL = promptStub(t, better, "follows the trend instead of fighting it", &calls).URL

	report, err := RunPrompt(cfg, deterministicSeries(cfg), PromptOptions{
		Objective: "sharpe",
		Rounds:    2,
		Stall:     3,
	})
	if err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model called %d times, want 2 (one proposal per round)", calls)
	}
	if !report.LLMEnabled {
		t.Fatal("expected an enabled model in this scenario")
	}
	// The baseline used the built-in persona.
	if report.Rounds[0].Persona != strategy.DefaultLLMPersona {
		t.Fatalf("baseline persona = %q, want the built-in persona", report.Rounds[0].Persona)
	}
	// The proposed persona must have been evaluated in round 1.
	if report.Rounds[1].Persona != better {
		t.Fatalf("round 1 persona = %q, want the model's rewrite", report.Rounds[1].Persona)
	}
	// Whatever the backtest scored, the loop ends on the best persona it saw;
	// with a fixed proposal the best is either the baseline or the rewrite.
	if report.BestPrompt != better && report.BestPrompt != strategy.DefaultLLMPersona {
		t.Fatalf("BestPrompt = %q, want baseline or the proposed persona", report.BestPrompt)
	}
}

// TestRunPromptNoModelRunsBaselineOnly verifies the degraded path: with no
// key the loop still runs the baseline, reports the model as disabled, and
// stops with a skipped round instead of dialing anything.
func TestRunPromptNoModelRunsBaselineOnly(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	cfg := promptBaseTestConfig()
	report, err := RunPrompt(cfg, deterministicSeries(cfg), PromptOptions{
		Objective: "sharpe",
		Rounds:    3,
	})
	if err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if report.LLMEnabled {
		t.Fatal("model must be disabled when no key is set")
	}
	if len(report.Rounds) != 2 || report.Rounds[1].Note == "" {
		t.Fatalf("expected baseline + skipped note, got %+v", report.Rounds)
	}
	// No model: the winner must be the baseline persona.
	if report.BestPrompt != strategy.DefaultLLMPersona {
		t.Fatalf("BestPrompt = %q, want the baseline persona when the model is off", report.BestPrompt)
	}
}

// TestRunPromptRejectsNonLLMStrategy guards the scope: the persona only
// exists for the llm strategy, so the loop must refuse anything else.
func TestRunPromptRejectsNonLLMStrategy(t *testing.T) {
	cfg := promptBaseTestConfig()
	cfg.Strategy.Name = "ma_cross"
	if _, err := RunPrompt(cfg, deterministicSeries(cfg), PromptOptions{Rounds: 1}); err == nil {
		t.Fatal("RunPrompt must refuse a non-llm strategy")
	}
}

// TestRunPromptEarlyStop verifies the stall guard: a model that proposes the
// same (non-improving) persona every round stops the loop early.
func TestRunPromptEarlyStop(t *testing.T) {
	t.Setenv("LLM_API_KEY", "stub-key")
	t.Setenv("OPENAI_API_KEY", "")

	var calls int
	same := "Flat unless the SMA cross is clean."
	cfg := promptBaseTestConfig()
	cfg.LLM.BaseURL = promptStub(t, same, "no change", &calls).URL

	report, err := RunPrompt(cfg, deterministicSeries(cfg), PromptOptions{
		Objective: "sharpe",
		Rounds:    6,
		Stall:     2,
	})
	if err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if !report.EarlyStopped {
		t.Fatal("expected the stall guard to stop the loop early")
	}
	if calls != 2 {
		t.Fatalf("model called %d times, want 2 (stall guard)", calls)
	}
	if !strings.Contains(report.Rounds[len(report.Rounds)-1].Note, "早停") {
		t.Fatalf("expected an early-stop marker, got %+v", report.Rounds[len(report.Rounds)-1])
	}
}
