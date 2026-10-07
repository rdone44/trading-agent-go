package live_test

import (
	"path/filepath"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/state"
)

func TestInitRefusesDifferentSymbolAndExecutionMode(t *testing.T) {
	for _, saved := range []state.State{{Symbol: "OTHER", Initial: 1000, Cash: 1000}, {Symbol: "TEST", Executed: true, Initial: 1000, Cash: 1000}, {Symbol: "TEST", Venue: "futures", Initial: 1000, Cash: 1000}} {
		path := filepath.Join(t.TempDir(), "s.json")
		if err := state.Save(path, saved); err != nil {
			t.Fatal(err)
		}
		if err := offlineRunner(t, path).Init(); err == nil {
			t.Fatalf("mismatched state accepted %+v", saved)
		}
	}
}
