// Package marketdata loads price bars from an offline generator or a provider.
package marketdata

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/model"
)

// TradingCalendar returns `days` business days ending at `end`.
func TradingCalendar(days int, end time.Time) []time.Time {
	if days < 1 {
		days = 1
	}
	out := make([]time.Time, 0, days)
	day := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	for len(out) < days {
		if weekday := day.Weekday(); weekday != time.Saturday && weekday != time.Sunday {
			out = append(out, day)
		}
		day = day.AddDate(0, 0, -1)
	}
	// The loop walks backwards, so reverse into chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Synthetic generates reproducible OHLCV bars with a two-state volatility
// regime. Deterministic for a given (seed, symbol, days), so strategy changes
// are actually comparable.
func Synthetic(symbol string, days int, cfg config.Synthetic, end time.Time, seedOverride *int64) (model.Series, error) {
	if days < 1 {
		return model.Series{}, fmt.Errorf("历史天数必须大于等于 1，当前为 %d", days)
	}
	seed := cfg.Seed
	if seedOverride != nil {
		seed = *seedOverride
	}
	if cfg.StartPrice <= 0 {
		cfg.StartPrice = 100
	}
	if cfg.BarsPerYear <= 0 {
		cfg.BarsPerYear = 252
	}

	dates := TradingCalendar(days, end)
	n := len(dates)
	// Seeding with the symbol hash (instead of rand.Seed) keeps runs reproducible.
	rng := rand.New(rand.NewSource(seed ^ int64(hashString(symbol))))

	vol := make([]float64, n)
	drift := make([]float64, n)
	regime := 0
	for i := 0; i < n; i++ {
		if i > 0 && rng.Float64() < cfg.RegimeSwitchProb {
			regime = 1 - regime
		}
		if regime == 0 {
			vol[i] = cfg.AnnualVol
			drift[i] = cfg.AnnualDrift
		} else {
			vol[i] = cfg.AnnualVol * 1.8
			drift[i] = -cfg.AnnualDrift * 0.9
		}
	}

	dt := 1.0 / float64(cfg.BarsPerYear)
	close := make([]float64, n)
	cum := 0.0
	for i := 0; i < n; i++ {
		shock := rng.NormFloat64()
		cum += (drift[i]-0.5*vol[i]*vol[i])*dt + vol[i]*math.Sqrt(dt)*shock
		close[i] = cfg.StartPrice * math.Exp(cum)
	}

	bars := make([]model.Bar, n)
	for i := 0; i < n; i++ {
		open := cfg.StartPrice
		if i > 0 {
			open = close[i-1] * (1 + rng.NormFloat64()*0.002)
		}
		intraday := math.Abs(rng.NormFloat64() * vol[i] * math.Sqrt(dt) * 0.7)
		bars[i] = model.Bar{
			Time:   dates[i],
			Open:   open,
			High:   math.Max(open, close[i]) * (1 + intraday),
			Low:    math.Min(open, close[i]) * (1 - intraday),
			Close:  close[i],
			Volume: 200_000 + float64(rng.Intn(2_800_000)),
		}
	}
	return model.Series{Symbol: symbol, Bars: bars, Source: "synthetic"}, nil
}

// hashString is FNV-1a: stable across processes and runs, unlike a language
// runtime's salted hash.
func hashString(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
