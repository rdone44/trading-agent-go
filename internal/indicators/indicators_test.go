package indicators

import (
	"math"
	"testing"
)

func TestSMAWarmupAndValues(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	got := SMA(values, 3)
	for i := 0; i < 2; i++ {
		if !math.IsNaN(got[i]) {
			t.Fatalf("index %d should be NaN during warm-up, got %v", i, got[i])
		}
	}
	want := []float64{2, 3, 4}
	for i, w := range want {
		if got[i+2] != w {
			t.Fatalf("SMA[%d] = %v, want %v", i+2, got[i+2], w)
		}
	}
}

func TestEMAStartsAtFirstValue(t *testing.T) {
	values := []float64{10, 10, 10, 10}
	got := EMA(values, 3)
	for i, v := range got {
		if math.Abs(v-10) > 1e-12 {
			t.Fatalf("EMA[%d] = %v, want 10", i, v)
		}
	}
}

func TestRSIExtremes(t *testing.T) {
	up := make([]float64, 40)
	down := make([]float64, 40)
	flat := make([]float64, 40)
	for i := range up {
		up[i] = float64(100 + i)
		down[i] = float64(200 - i)
		flat[i] = 50
	}
	if got := RSI(up, 14)[39]; math.Abs(got-100) > 1e-6 {
		t.Fatalf("uptrend RSI = %v, want 100", got)
	}
	if got := RSI(down, 14)[39]; math.Abs(got-0) > 1e-6 {
		t.Fatalf("downtrend RSI = %v, want 0", got)
	}
	if got := RSI(flat, 14)[39]; math.Abs(got-50) > 1e-6 {
		t.Fatalf("flat RSI = %v, want 50", got)
	}
}

func TestATRIsPositiveAndFinite(t *testing.T) {
	high := []float64{11, 12, 13, 14, 15}
	low := []float64{9, 10, 11, 12, 13}
	close := []float64{10, 11, 12, 13, 14}
	got := ATR(high, low, close, 3)
	if !(got[4] > 0) || math.IsNaN(got[4]) {
		t.Fatalf("ATR[4] = %v, want a positive finite value", got[4])
	}
}

func TestRollingMaxMinAndShift(t *testing.T) {
	values := []float64{3, 1, 4, 1, 5, 9, 2, 6}
	if got := RollingMax(values, 3)[2]; got != 4 {
		t.Fatalf("RollingMax[2] = %v, want 4", got)
	}
	if got := RollingMin(values, 3)[2]; got != 1 {
		t.Fatalf("RollingMin[2] = %v, want 1", got)
	}
	shifted := ShiftRight(values, 2)
	if !math.IsNaN(shifted[0]) || shifted[2] != 3 || shifted[3] != 1 {
		t.Fatalf("ShiftRight mismatch: %v", shifted)
	}
}
