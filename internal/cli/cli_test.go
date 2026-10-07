package cli

import (
	"strings"
	"testing"

	"github.com/huijun/trading-agent-go/internal/metrics"
)

func ptrf(v float64) *float64 { return &v }

func TestObjectiveValue(t *testing.T) {
	m := metrics.Metrics{
		Sharpe: ptrf(1.5), Sortino: ptrf(2.0), TotalReturnPct: ptrf(42.0),
		ProfitFactor: ptrf(1.3), WinRatePct: ptrf(61.0),
	}
	cases := map[string]float64{
		"sharpe": 1.5, "sortino": 2.0, "total_return": 42.0,
		"profit_factor": 1.3, "win_rate": 61.0,
	}
	for name, want := range cases {
		got, ok := objectiveValue(m, name)
		if !ok {
			t.Fatalf("objectiveValue(%q) ok=false, want %v", name, want)
		}
		if got != want {
			t.Fatalf("objectiveValue(%q) = %v, want %v", name, got, want)
		}
	}
	if _, ok := objectiveValue(m, "nonsense"); ok {
		t.Fatal("objectiveValue(unknown) must report ok=false")
	}
}

func TestObjectiveValueNilMetric(t *testing.T) {
	// A run with no trades leaves the risk metrics null; the loop must treat
	// that as "not usable for this objective", not as a zero.
	m := metrics.Metrics{} // every pointer field nil
	if _, ok := objectiveValue(m, "sharpe"); ok {
		t.Fatal("objectiveValue with a null metric must be ok=false")
	}
}

func TestFormatParamsIsDeterministic(t *testing.T) {
	// Map order is random in Go, so a stable render has to sort by key.
	params := map[string]float64{"slow": 30, "fast": 10, "period": 14}
	got := formatParams(params)
	want := "fast=10 period=14 slow=30"
	if got != want {
		t.Fatalf("formatParams = %q, want %q (sorted by key)", got, want)
	}
	if got2 := formatParams(map[string]float64{}); got2 != "" {
		t.Fatalf("formatParams(empty) = %q, want empty string", got2)
	}
}

func TestMetricsSummaryRendersNulls(t *testing.T) {
	s := metricsSummary(metrics.Metrics{})
	for _, token := range []string{"total=n/a", "sharpe=n/a", "trades=0"} {
		if !strings.Contains(s, token) {
			t.Fatalf("metricsSummary(all-null) missing %q:\n%s", token, s)
		}
	}
}

func TestMetricsSummaryRendersValues(t *testing.T) {
	m := metrics.Metrics{
		TotalReturnPct: ptrf(12.5), AnnualReturnPct: ptrf(8.3), Sharpe: ptrf(1.2),
		NumTrades: 40,
	}
	s := metricsSummary(m)
	for _, token := range []string{"total=12.50%", "sharpe=1.20", "trades=40"} {
		if !strings.Contains(s, token) {
			t.Fatalf("metricsSummary(values) missing %q:\n%s", token, s)
		}
	}
}

func TestPlainMetricLocal(t *testing.T) {
	if got := plainMetricLocal(nil, 2); got != "n/a" {
		t.Fatalf("plainMetricLocal(nil) = %q, want n/a", got)
	}
	if got := plainMetricLocal(ptrf(1.23456), 2); got != "1.23" {
		t.Fatalf("plainMetricLocal(1.23456,2) = %q, want 1.23", got)
	}
}
