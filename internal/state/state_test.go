package state

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/huijun/trading-agent-go/internal/broker"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/risk"
)

func saveAndReload(t *testing.T, s State) State {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	if err := Save(path, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, ok, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("Load reports no state for a file we just wrote")
	}
	return back
}

func TestSaveLoadRoundtripWithOpen(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 2, 3, 0, time.UTC)
	ot := &engine.OpenTrade{
		EntryTime: now, EntryPrice: 50000, Quantity: 0.4,
		Side: broker.Buy, EntryFee: 2,
	}
	orig := State{
		Symbol: "BTCUSDT", Strategy: "ma_cross",
		Initial: 100000, Cash: 70000, Peak: 110000,
		Open: &Open{
			EntryTime: now, EntryPrice: 50000, Quantity: 0.4,
			Side: "buy", EntryFee: 2, Stop: 47000, TakeProfit: 55000,
		},
		Risk: risk.RiskState{
			Day: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), DayStartEquity: 109000,
		},
		Executed: true,
	}
	back := saveAndReload(t, orig)

	if back.Symbol != orig.Symbol || back.Strategy != orig.Strategy {
		t.Fatalf("scalar fields did not survive: %+v", back)
	}
	if math.Abs(back.Cash-orig.Cash) > 1e-9 || math.Abs(back.Peak-orig.Peak) > 1e-9 {
		t.Fatalf("cash/peak did not survive: got %v/%v want %v/%v", back.Cash, back.Peak, orig.Cash, orig.Peak)
	}
	if back.Open == nil {
		t.Fatal("open position lost in roundtrip")
	}
	if math.Abs(back.Open.Stop-47000) > 1e-9 || math.Abs(back.Open.TakeProfit-55000) > 1e-9 {
		t.Fatalf("levels did not survive: stop=%v target=%v", back.Open.Stop, back.Open.TakeProfit)
	}
	if !back.Executed {
		t.Fatal("executed flag lost in roundtrip")
	}

	// ToEngine must hand back NaN levels only when they were unset.
	cash, peak, open, stop, target, _ := back.ToEngine()
	if cash != orig.Cash || peak != orig.Peak {
		t.Fatalf("ToEngine cash/peak = %v/%v", cash, peak)
	}
	if open == nil || open.Quantity != 0.4 || open.Side != broker.Buy {
		t.Fatalf("ToEngine open position wrong: %+v", open)
	}
	if math.Abs(stop-47000) > 1e-9 || math.Abs(target-55000) > 1e-9 {
		t.Fatalf("ToEngine levels = %v/%v", stop, target)
	}
	_ = ot
}

func TestSaveLoadRoundtripFlat(t *testing.T) {
	orig := State{Symbol: "ETHUSDT", Cash: 1000, Peak: 1000, Executed: false}
	back := saveAndReload(t, orig)
	if back.Open != nil {
		t.Fatalf("flat session must stay flat: %+v", back.Open)
	}
	_, _, open, stop, target, _ := back.ToEngine()
	if open != nil {
		t.Fatal("ToEngine must return a nil open trade for a flat session")
	}
	if !math.IsNaN(stop) || !math.IsNaN(target) {
		t.Fatalf("flat session levels must be NaN, got %v/%v", stop, target)
	}
}

func TestLoadMissingFileIsNotError(t *testing.T) {
	_, ok, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing state file must not be an error: %v", err)
	}
	if ok {
		t.Fatal("missing state file must report ok=false")
	}
}

func TestSaveDoesNotLeaveTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	for i := 0; i < 3; i++ {
		if err := Save(path, State{Symbol: "BTCUSDT"}); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "s.json.tmp" {
			t.Fatal("temp file left behind after save")
		}
	}
}

func TestFromEngineNaNLevelsStoredAsZero(t *testing.T) {
	s := FromEngine("BTCUSDT", "ma_cross", 100000, 80000, 100000,
		&engine.OpenTrade{Quantity: 1, Side: broker.Buy}, math.NaN(), math.NaN(),
		risk.RiskState{}, false)
	if s.Open == nil {
		t.Fatal("open trade must be serialized")
	}
	if s.Open.Stop != 0 || s.Open.TakeProfit != 0 {
		t.Fatalf("NaN levels must be stored as 0, got %v/%v", s.Open.Stop, s.Open.TakeProfit)
	}
	if _, _, _, stop, target, _ := s.ToEngine(); !math.IsNaN(stop) || !math.IsNaN(target) {
		t.Fatalf("ToEngine must restore NaN levels, got %v/%v", stop, target)
	}
}
