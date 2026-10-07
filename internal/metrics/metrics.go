// Package metrics computes performance statistics for an equity curve.
package metrics

import (
	"math"

	"github.com/rdone44/trading-agent-go/internal/portfolio"
)

// Metrics is the JSON shape written to metrics.json: values that can be
// undefined are pointers so they serialize as null instead of 0.
type Metrics struct {
	InitialCash         float64  `json:"initial_cash"`
	FinalEquity         *float64 `json:"final_equity"`
	TotalReturnPct      *float64 `json:"total_return_pct"`
	AnnualReturnPct     *float64 `json:"annual_return_pct"`
	AnnualVolatilityPct *float64 `json:"annual_volatility_pct"`
	Sharpe              *float64 `json:"sharpe"`
	Sortino             *float64 `json:"sortino"`
	MaxDrawdownPct      *float64 `json:"max_drawdown_pct"`
	Calmar              *float64 `json:"calmar"`
	ExposurePct         *float64 `json:"exposure_pct"`
	NumTrades           int      `json:"num_trades"`
	WinRatePct          *float64 `json:"win_rate_pct"`
	ProfitFactor        *float64 `json:"profit_factor"`
	AvgWin              *float64 `json:"avg_win"`
	AvgLoss             *float64 `json:"avg_loss"`
	Expectancy          *float64 `json:"expectancy"`
	TotalFees           float64  `json:"total_fees"`
}

// Compute derives the statistics from the equity curve and closed trades.
//
// It is generic over the trade type so the engine package can pass its own
// Trade slice without an import cycle or a conversion loop.
func Compute[T interface {
	PnLValue() float64
	FeeValue() float64
}](curve []portfolio.EquityPoint, trades []T, initialCash float64, barsPerYear int) Metrics {
	m := Metrics{InitialCash: round2(initialCash)}
	if len(curve) == 0 {
		return m
	}
	if barsPerYear <= 0 {
		barsPerYear = 252
	}

	values := make([]float64, len(curve))
	for i, p := range curve {
		values[i] = p.Equity
	}
	finalEquity := values[len(values)-1]
	totalReturn := finalEquity/initialCash - 1

	periods := len(values) - 1
	if periods < 1 {
		periods = 1
	}
	years := float64(periods) / float64(barsPerYear)
	annualReturn := 0.0
	if years > 0 && finalEquity > 0 {
		annualReturn = math.Pow(finalEquity/initialCash, 1/years) - 1
	}

	returns := make([]float64, 0, periods)
	for i := 1; i < len(values); i++ {
		if values[i-1] != 0 {
			returns = append(returns, values[i]/values[i-1]-1)
		}
	}

	annualVol := 0.0
	sharpe, sortino := 0.0, 0.0
	if len(returns) > 1 {
		mean, std := meanStd(returns)
		annualVol = std * math.Sqrt(float64(barsPerYear))
		if std > 0 {
			sharpe = mean / std * math.Sqrt(float64(barsPerYear))
		}
		downside := make([]float64, 0, len(returns))
		for _, r := range returns {
			if r < 0 {
				downside = append(downside, r)
			}
		}
		if len(downside) > 1 {
			_, downStd := meanStd(downside)
			if downStd > 0 {
				sortino = mean / downStd * math.Sqrt(float64(barsPerYear))
			}
		}
	}

	peak := values[0]
	maxDD := 0.0
	for _, v := range values {
		if v > peak {
			peak = v
		}
		if peak != 0 {
			dd := v/peak - 1
			if dd < maxDD {
				maxDD = dd
			}
		}
	}
	calmar := 0.0
	if maxDD < 0 {
		calmar = annualReturn / math.Abs(maxDD)
	}

	exposed := 0
	for _, p := range curve {
		if math.Abs(p.MarketValue) > 1e-9 {
			exposed++
		}
	}
	exposure := float64(exposed) / float64(len(curve))

	m.FinalEquity = f(round2(finalEquity))
	m.TotalReturnPct = f(round6(totalReturn * 100))
	m.AnnualReturnPct = f(round6(annualReturn * 100))
	m.AnnualVolatilityPct = f(round6(annualVol * 100))
	m.Sharpe = f(round6(sharpe))
	m.Sortino = f(round6(sortino))
	m.MaxDrawdownPct = f(round6(maxDD * 100))
	m.Calmar = f(round6(calmar))
	m.ExposurePct = f(round6(exposure * 100))

	if len(trades) > 0 {
		wins, losses := []float64{}, []float64{}
		grossProfit, grossLoss := 0.0, 0.0
		totalFees := 0.0
		for _, t := range trades {
			pnl := t.PnLValue()
			totalFees += t.FeeValue()
			switch {
			case pnl > 0:
				wins = append(wins, pnl)
				grossProfit += pnl
			case pnl < 0:
				losses = append(losses, pnl)
				grossLoss += math.Abs(pnl)
			}
		}
		m.NumTrades = len(trades)
		m.WinRatePct = f(round6(float64(len(wins)) / float64(len(trades)) * 100))
		if grossLoss > 0 {
			m.ProfitFactor = f(round6(grossProfit / grossLoss))
		}
		m.AvgWin = f(round6(mean(wins)))
		m.AvgLoss = f(round6(mean(losses)))
		allPnL := make([]float64, len(trades))
		for i, t := range trades {
			allPnL[i] = t.PnLValue()
		}
		m.Expectancy = f(round6(mean(allPnL)))
		m.TotalFees = round2(totalFees)
	}
	return m
}

func meanStd(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	mean := mean(values)
	if len(values) < 2 {
		return mean, 0
	}
	sum := 0.0
	for _, v := range values {
		sum += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(sum / float64(len(values)-1))
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func f(v float64) *float64 { return &v }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
