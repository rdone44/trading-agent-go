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
			cfg := config.Default()
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

// The desktop edition's local switch is a second way to open the gate, but
// only when the environment variable is absent: an operator who explicitly
// exported TA_ALLOW_LIVE=0 must not be overridden by a checkbox.
func TestDesktopGateOpensOnlyWhenEnvironmentIsSilent(t *testing.T) {
	t.Cleanup(func() { live.DesktopGate = nil })
	cfg := config.Default()
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state.json")

	// Desktop switch off, environment unset: still refused.
	t.Setenv("TA_ALLOW_LIVE", "")
	if err := os.Unsetenv("TA_ALLOW_LIVE"); err != nil {
		t.Fatal(err)
	}
	live.DesktopGate = func() bool { return false }
	if runner, err := live.New(cfg, strat, true, state); runner != nil || err == nil {
		t.Fatalf("closed desktop gate allowed a runner: runner=%v err=%v", runner, err)
	}

	// Desktop switch on: allowed, and the runner really is in execute mode.
	live.DesktopGate = func() bool { return true }
	runner, err := live.New(cfg, strat, true, state)
	if err != nil || runner == nil || !runner.IsExecuted() {
		t.Fatalf("open desktop gate refused the runner: runner=%v err=%v", runner, err)
	}

	// An explicit TA_ALLOW_LIVE=0 wins over the switch.
	t.Setenv("TA_ALLOW_LIVE", "0")
	if runner, err := live.New(cfg, strat, true, state); runner != nil || err == nil {
		t.Fatalf("explicit environment opt-out was ignored: runner=%v err=%v", runner, err)
	}

	// The server edition never wires the hook; the variable stays the gate.
	live.DesktopGate = nil
	t.Setenv("TA_ALLOW_LIVE", "")
	if err := os.Unsetenv("TA_ALLOW_LIVE"); err != nil {
		t.Fatal(err)
	}
	if runner, err := live.New(cfg, strat, true, state); runner != nil || err == nil {
		t.Fatalf("server edition built a runner without the variable: runner=%v err=%v", runner, err)
	}
}
