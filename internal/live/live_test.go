package live_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/live"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/state"
	"github.com/rdone44/trading-agent-go/internal/strategy"
	"github.com/rdone44/trading-agent-go/internal/testfx"
)

// offlineRunner builds a paper runner whose market data comes from testfx
// instead of Binance, so the loop can be exercised without the network.
func offlineRunner(t *testing.T, statePath string) *live.Runner {
	t.Helper()
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Agent.HistoryDays = 300
	cfg.Backtest.WarmupBars = 30

	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatalf("build strategy: %v", err)
	}
	runner, err := live.New(cfg, strat, false, statePath)
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	runner.SeriesLoader = func(symbol string, days int, end time.Time) (model.Series, error) {
		return testfx.Bars(symbol, days, 42, end), nil
	}
	runner.PriceLoader = func(symbol string) (float64, time.Time, error) {
		series := testfx.Bars(symbol, 5, 42, time.Now().UTC())
		last := series.Bars[len(series.Bars)-1]
		return last.Close, last.Time, nil
	}
	return runner
}

// A paper runner must never claim to place real orders, and the venue
// accessors must mirror the config so the console can label the session.
func TestPaperRunnerReportsItsVenue(t *testing.T) {
	runner := offlineRunner(t, "")
	if runner.IsExecuted() {
		t.Fatal("a paper runner must report executed=false")
	}
	if got := runner.Leverage(); got != 1 {
		t.Fatalf("leverage = %d, want 1 for the default config", got)
	}
}

// One cycle has to move the book: it fetches bars and a price, decides, and
// returns a result the caller can display. Without this the console would show
// an idle session forever.
func TestCycleProducesAResultAndTrades(t *testing.T) {
	runner := offlineRunner(t, "")
	if err := runner.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	result, err := runner.Cycle(time.Now().UTC())
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if result.Equity <= 0 {
		t.Fatalf("equity = %v, want a positive mark-to-market value", result.Equity)
	}
	if result.Action == "" {
		t.Fatal("a completed cycle must describe what it did")
	}
	// The deterministic fixture series is long enough for the default
	// strategy to find a signal, so the first cycle opens a position.
	if !runner.Agent().Book.Position("TEST").IsOpen() {
		t.Fatalf("expected an open position after one cycle, action = %q", result.Action)
	}
}

// A cycle must survive a failing data source: a transient network error has to
// surface as an error while leaving the book untouched, because the session
// keeps polling and must not corrupt state on a bad tick.
func TestCycleSurfacesDataErrorsWithoutTrading(t *testing.T) {
	runner := offlineRunner(t, "")
	runner.PriceLoader = func(string) (float64, time.Time, error) {
		return 0, time.Time{}, os.ErrDeadlineExceeded
	}
	if err := runner.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if _, err := runner.Cycle(time.Now().UTC()); err == nil {
		t.Fatal("a failing price loader must surface as an error")
	}
	if runner.Agent().Book.Position("TEST").IsOpen() {
		t.Fatal("a cycle that failed to price must not open a position")
	}
}

// Save then Init is the restart path: the second runner must resume the open
// position and peak equity instead of starting flat and re-entering.
func TestSaveAndInitRoundTripResumesPosition(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "nested", "trade-state.json")

	first := offlineRunner(t, statePath)
	if err := first.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := first.Cycle(time.Now().UTC()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	before := first.Agent().Book.Position("TEST")
	if !before.IsOpen() {
		t.Fatal("precondition: the first runner should have opened a position")
	}

	// Save is also called by Cycle; call it directly to prove it is idempotent.
	if err := first.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file was not written: %v", err)
	}

	second := offlineRunner(t, statePath)
	if err := second.Init(); err != nil {
		t.Fatalf("Init on the restored runner: %v", err)
	}
	after := second.Agent().Book.Position("TEST")
	if !after.IsOpen() {
		t.Fatal("the restored runner lost the open position")
	}
	if after.Quantity != before.Quantity {
		t.Fatalf("restored quantity = %v, want %v", after.Quantity, before.Quantity)
	}
	if second.Agent().PeakEquity != first.Agent().PeakEquity {
		t.Fatalf("restored peak equity = %v, want %v",
			second.Agent().PeakEquity, first.Agent().PeakEquity)
	}
}

// A corrupt state file must fail loudly rather than silently starting flat,
// which would double up on a position the user already holds.
func TestInitRejectsCorruptState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "trade-state.json")
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}

	runner := offlineRunner(t, statePath)
	if err := runner.Init(); err == nil {
		t.Fatal("Init must reject a corrupt state file")
	}
}

// An empty state path means "do not persist", which the CLI relies on for
// throwaway runs; Save must be a no-op rather than writing to the CWD.
func TestSaveWithoutStatePathIsANoOp(t *testing.T) {
	dir := t.TempDir()
	// Run from an empty directory so the assertion cannot trip over a state
	// file left behind by another test or a manual CLI run.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	runner := offlineRunner(t, "")
	if err := runner.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := runner.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Save with an empty state path wrote %d file(s)", len(entries))
	}
}

// The runner must not claim executed=true for a leveraged paper session:
// DryRun still simulates fills locally, whatever the multiplier is.
func TestFuturesPaperRunnerIsNotExecuted(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Symbol = "TEST"
	cfg.Risk.Leverage = 5

	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		t.Fatalf("build strategy: %v", err)
	}
	runner, err := live.New(cfg, strat, false, "")
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	if runner.IsExecuted() {
		t.Fatal("futures paper trading must not report executed=true")
	}
	if got := runner.Leverage(); got != 5 {
		t.Fatalf("leverage = %d, want 5", got)
	}
}

// state.FromEngine/ToEngine is the contract the restart path depends on; pin
// the field mapping here so a change to either side fails in one obvious place.
func TestSavedStateCarriesThePosition(t *testing.T) {
	runner := offlineRunner(t, "")
	if err := runner.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := runner.Cycle(time.Now().UTC()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	pos := runner.Agent().Book.Position("TEST")
	if !pos.IsOpen() {
		t.Fatal("precondition: expected an open position")
	}

	saved := state.FromEngine(
		"TEST", runner.Agent().Strategy.Describe(),
		runner.Agent().Book.InitialCash, runner.Agent().Book.Cash,
		runner.Agent().PeakEquity, runner.Agent().OpenTrade(),
		pos.StopPrice, pos.TakeProfitPrice, runner.Agent().Risk.Snapshot(), false,
	)
	cash, peak, open, stop, target, _ := saved.ToEngine()
	if cash != runner.Agent().Book.Cash {
		t.Fatalf("cash = %v, want %v", cash, runner.Agent().Book.Cash)
	}
	if peak != runner.Agent().PeakEquity {
		t.Fatalf("peak = %v, want %v", peak, runner.Agent().PeakEquity)
	}
	if open == nil {
		t.Fatal("the saved state must carry the open trade")
	}
	if open.Quantity != pos.Quantity {
		t.Fatalf("open quantity = %v, want %v", open.Quantity, pos.Quantity)
	}
	if stop != pos.StopPrice || target != pos.TakeProfitPrice {
		t.Fatalf("stop/target = %v/%v, want %v/%v",
			stop, target, pos.StopPrice, pos.TakeProfitPrice)
	}
}
