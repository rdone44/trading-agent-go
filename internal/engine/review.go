// ReviewFacts renders a finished run into a compact, model-ready fact blob:
// the key metrics plus a handful of the largest winners and losers. It is the
// input to llm.Review; keeping it here (not in the llm package) avoids an
// engine<->llm import cycle, since the engine already knows the metric shapes.
package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ReviewFacts summarizes a run for a post-mortem prompt. topN bounds the
// number of example trades named (per side); the overall metrics are always in.
func ReviewFacts(result Result, topN int) string {
	m := result.Metrics
	if topN < 1 {
		topN = 3
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s strategy=%s window=%s..%s bars=%d trades=%d\n",
		result.Symbol, result.Strategy,
		result.Start.Format("2006-01-02"), result.End.Format("2006-01-02"),
		result.Bars, m.NumTrades)
	fmt.Fprintf(&b, "Metrics: total=%s%% annual=%s%% sharpe=%s sortino=%s "+
		"maxDD=%s%% winrate=%s%% PF=%s fees=%.2f\n",
		plainMetric(m.TotalReturnPct, 2), plainMetric(m.AnnualReturnPct, 2),
		plainMetric(m.Sharpe, 2), plainMetric(m.Sortino, 2),
		plainMetric(m.MaxDrawdownPct, 2), plainMetric(m.WinRatePct, 1),
		plainMetric(m.ProfitFactor, 2), m.TotalFees)

	if len(result.Trades) == 0 {
		b.WriteString("\nNo closed trades in the window.\n")
		return b.String()
	}

	// Sort by absolute PnL so the extremes lead the review.
	trades := append([]Trade{}, result.Trades...)
	sort.SliceStable(trades, func(i, j int) bool {
		return math.Abs(trades[i].PnL) > math.Abs(trades[j].PnL)
	})

	b.WriteString("\nLargest trades by |pnl|:\n")
	for i, t := range trades {
		if i >= topN {
			break
		}
		fmt.Fprintf(&b, "  %s %.2f -> %.2f  pnl=%.2f  reason=%s\n",
			t.Side, t.EntryPrice, t.ExitPrice, t.PnL, t.Reason)
	}
	return b.String()
}

// plainMetric renders a pointer metric the way the CLI does ("n/a" when null).
func plainMetric(v *float64, digits int) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.*f", digits, *v)
}
