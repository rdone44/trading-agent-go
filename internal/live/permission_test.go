package live_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/live"
	"github.com/rdone44/trading-agent-go/internal/strategy"
)

func TestRunnerRequiresExactProcessOptIn(t *testing.T) {
	// Constructor only: never Init or Cycle a real runner, never contact Binance.
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")
	for _, value := range []string{"unset", "", "0", "true", "yes", "01", " 1", "1 ", "1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TA_ALLOW_LIVE", value)
			if value == "unset" {
				if err := os.Unsetenv("TA_ALLOW_LIVE"); err != nil {
					t.Fatal(err)
				}
			}
			for _, futures := range []bool{false, true} {
				cfg := config.Default()
				cfg.Live.Futures = futures
				strat, err := strategy.New(cfg.Strategy.Name, cfg)
				if err != nil {
					t.Fatal(err)
				}
				runner, err := live.New(cfg, strat, true, filepath.Join(t.TempDir(), "state.json"))
				if value == "1" {
					if err != nil || runner == nil || !runner.IsExecuted() {
						t.Fatalf("explicit opt-in did not allow construction: runner=%v err=%v", runner, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "TA_ALLOW_LIVE=1") || runner != nil {
					t.Fatalf("unauthorized runner: runner=%v err=%v", runner, err)
				}
				paper, err := live.New(cfg, strat, false, "")
				if err != nil || paper == nil || paper.IsExecuted() {
					t.Fatalf("paper behavior changed: runner=%v err=%v", paper, err)
				}
			}
		})
	}
}

func TestProcessOptInDoesNotBypassStateRequirement(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "1")
	cfg := config.Default()
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runner, err := live.New(cfg, strat, true, ""); runner != nil || err == nil || !strings.Contains(err.Error(), "持久化状态文件") {
		t.Fatalf("state guard bypassed: runner=%v err=%v", runner, err)
	}
}
