// Package indicators implements the moving averages and oscillators used by
// the strategies. Every function returns a slice aligned with the input and
// uses NaN for the warm-up region, so callers can distinguish "not enough data
// yet" from a real value.
package indicators

import (
	"math"

	"github.com/huijun/trading-agent-go/internal/model"
)

// SMA is a simple moving average; NaN until `period` values are available.
func SMA(values []float64, period int) []float64 {
	out := filled(len(values))
	if period <= 0 || len(values) < period {
		return out
	}
	sum := 0.0
	for i, v := range values {
		sum += v
		if i >= period {
			sum -= values[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// EMA is an exponential moving average with alpha = 1/period, seeded with the
// first observation (Wilder's smoothing).
func EMA(values []float64, period int) []float64 {
	out := filled(len(values))
	if period <= 0 || len(values) == 0 {
		return out
	}
	alpha := 1.0 / float64(period)
	started := false
	prev := 0.0
	for i, v := range values {
		if model.IsNaN(v) {
			continue
		}
		if !started {
			prev = v
			started = true
		} else {
			prev = (1-alpha)*prev + alpha*v
		}
		out[i] = prev
	}
	return out
}

// RSI is Wilder's relative strength index. A pure uptrend reads 100, a pure
// downtrend 0, and a flat series 50.
func RSI(values []float64, period int) []float64 {
	out := filled(len(values))
	if period <= 0 || len(values) < 2 {
		for i := range out {
			out[i] = 50.0
		}
		return out
	}

	gains := filled(len(values))
	losses := filled(len(values))
	for i := 1; i < len(values); i++ {
		delta := values[i] - values[i-1]
		if delta > 0 {
			gains[i] = delta
			losses[i] = 0
		} else {
			gains[i] = 0
			losses[i] = -delta
		}
	}

	avgGain := EMA(gains[1:], period)
	avgLoss := EMA(losses[1:], period)
	for i := range out {
		out[i] = 50.0 // neutral reading while the averages are still warming up
	}
	for i := range avgGain {
		switch {
		case model.IsNaN(avgGain[i]) || model.IsNaN(avgLoss[i]):
			out[i+1] = 50.0
		case avgLoss[i] == 0 && avgGain[i] > 0:
			out[i+1] = 100.0
		case avgLoss[i] == 0 && avgGain[i] == 0:
			out[i+1] = 50.0
		default:
			rs := avgGain[i] / avgLoss[i]
			out[i+1] = 100.0 - 100.0/(1.0+rs)
		}
	}
	return out
}

// ATR is the average true range with Wilder's smoothing.
func ATR(high, low, close []float64, period int) []float64 {
	n := len(close)
	if len(high) < n || len(low) < n {
		n = min(len(high), len(low))
	}
	trueRange := filled(n)
	for i := 0; i < n; i++ {
		if i == 0 {
			trueRange[i] = high[i] - low[i]
			continue
		}
		prevClose := close[i-1]
		trueRange[i] = math.Max(high[i]-low[i],
			math.Max(math.Abs(high[i]-prevClose), math.Abs(low[i]-prevClose)))
	}
	return EMA(trueRange, period)
}

// RollingMax is a trailing maximum over `period` values (NaN until full).
func RollingMax(values []float64, period int) []float64 {
	out := filled(len(values))
	if period <= 0 {
		return out
	}
	for i := period - 1; i < len(values); i++ {
		best := values[i-period+1]
		for j := i - period + 2; j <= i; j++ {
			if values[j] > best {
				best = values[j]
			}
		}
		out[i] = best
	}
	return out
}

// RollingMin is a trailing minimum over `period` values (NaN until full).
func RollingMin(values []float64, period int) []float64 {
	out := filled(len(values))
	if period <= 0 {
		return out
	}
	for i := period - 1; i < len(values); i++ {
		best := values[i-period+1]
		for j := i - period + 2; j <= i; j++ {
			if values[j] < best {
				best = values[j]
			}
		}
		out[i] = best
	}
	return out
}

// ShiftRight moves values forward by `n` bars, leaving NaN at the front.
// shiftRight(x, 1)[i] == x[i-1], which is how a signal from bar t-1 reaches bar t.
func ShiftRight(values []float64, n int) []float64 {
	out := filled(len(values))
	for i := n; i < len(values); i++ {
		out[i] = values[i-n]
	}
	return out
}

func filled(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}
