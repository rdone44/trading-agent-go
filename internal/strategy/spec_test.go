package strategy

import (
	"math"
	"testing"
)

func TestSpecForListsAllStrategies(t *testing.T) {
	for _, name := range Available() {
		spec, ok := SpecFor(name)
		if !ok {
			t.Fatalf("SpecFor(%q) not found", name)
		}
		if len(spec.Params) == 0 {
			t.Fatalf("SpecFor(%q) has no parameters", name)
		}
	}
	if _, ok := SpecFor("does_not_exist"); ok {
		t.Fatal("SpecFor(unknown) must report not-found")
	}
}

func TestLLMSpecIsRegistered(t *testing.T) {
	// The tuner can only propose params for a strategy that has a spec; the
	// LLM strategy must be one of them now.
	spec, ok := SpecFor("llm")
	if !ok {
		t.Fatal("llm strategy has no spec, so tune cannot propose params for it")
	}
	for _, key := range []string{"llm_step", "llm_window", "fast", "slow", "period", "atr_period"} {
		found := false
		for _, p := range spec.Params {
			if p.Key == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("llm spec missing parameter %q", key)
		}
	}
}

func TestDefaultsSeedsThenOverrides(t *testing.T) {
	spec, _ := SpecFor("rsi_reversion")
	params := spec.Defaults(nil)
	if params["period"] != 14 {
		t.Fatalf("Defaults period = %v, want 14", params["period"])
	}
	// User override wins over the documented default.
	over := spec.Defaults(map[string]float64{"period": 21})
	if over["period"] != 21 {
		t.Fatalf("overridden period = %v, want 21", over["period"])
	}
	// Untouched keys keep their default.
	if over["lower"] != 30 {
		t.Fatalf("untouched lower = %v, want 30", over["lower"])
	}
}

func TestClampParamsBoundsToLegalRange(t *testing.T) {
	spec, _ := SpecFor("rsi_reversion")
	// period's legal range is [2,100] integer; lower is [5,50]; atr_stop_mult
	// is [0.5,10] on a 0.1 step.
	got := spec.ClampParams(map[string]float64{
		"period":        500,  // above max
		"lower":         2,    // below min
		"atr_stop_mult": 7.33, // snaps to 7.3 on the 0.1 step
		"fast":          99,   // not a param of rsi_reversion: pass through
	})
	if got["period"] != 100 {
		t.Fatalf("period 500 -> %v, want clamped to 100", got["period"])
	}
	if got["lower"] != 5 {
		t.Fatalf("lower 2 -> %v, want clamped to 5", got["lower"])
	}
	if got["atr_stop_mult"] != 7.3 {
		t.Fatalf("atr_stop_mult 7.33 -> %v, want snapped to 7.3", got["atr_stop_mult"])
	}
	if got["fast"] != 99 {
		t.Fatalf("unknown key fast 99 -> %v, want passed through", got["fast"])
	}
}

func TestClampParamsRoundsIntegerParams(t *testing.T) {
	spec, _ := SpecFor("rsi_reversion")
	got := spec.ClampParams(map[string]float64{"period": 14.4})
	if got["period"] != 14 {
		t.Fatalf("integer period 14.4 -> %v, want rounded to 14", got["period"])
	}
}

func TestClampParamsIgnoresNaNAndInf(t *testing.T) {
	spec, _ := SpecFor("rsi_reversion")
	in := map[string]float64{"period": math.NaN(), "lower": math.Inf(1)}
	got := spec.ClampParams(in)
	if !math.IsNaN(got["period"]) || !math.IsInf(got["lower"], 1) {
		t.Fatalf("NaN/Inf values must pass through, got %v / %v", got["period"], got["lower"])
	}
}

func TestSortKeysIsDeterministic(t *testing.T) {
	keys := SortKeys(map[string]float64{"z": 1, "a": 2, "m": 3})
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
		t.Fatalf("SortKeys = %v, want [a m z]", keys)
	}
	if got := SortKeys(map[string]float64{}); len(got) != 0 {
		t.Fatalf("SortKeys(empty) = %v, want empty", got)
	}
}
