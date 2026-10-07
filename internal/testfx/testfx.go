// Package testfx builds deterministic offline bars for unit tests.
//
// It is a test-only data source: the project ships a single real data source
// (Binance), and these helpers let the engine, report and webui tests run
// without touching the network. Bars are reproducible for a given
// (symbol, days, seed) so a change to any indicator, risk rule or
// accounting path shows up as a different trade count or return.
package testfx

import (
	"math"
	"math/rand"
	"time"

	"github.com/huijun/trading-agent-go/internal/model"
)

// Bars returns `days` daily bars ending on the day before `end`. The series
// is a geometric random walk with a mild drift, so strategies have signal to
// trade. Seeding with an FNV-1a hash of the symbol keeps runs reproducible.
func Bars(symbol string, days int, seed int64, end time.Time) model.Series {
	if days < 1 {
		days = 1
	}
	dates := TradingDays(days, end)
	rng := rand.New(rand.NewSource(seed ^ int64(fnvHash(symbol))))

	closes := make([]float64, days)
	cum := 0.0
	const annualVol, annualDrift = 0.25, 0.06
	dt := 1.0 / 252.0
	for i := 0; i < days; i++ {
		shock := rng.NormFloat64()
		cum += (annualDrift-0.5*annualVol*annualVol)*dt + annualVol*math.Sqrt(dt)*shock
		closes[i] = 100.0 * math.Exp(cum)
	}

	bars := make([]model.Bar, days)
	for i, close := range closes {
		open := 100.0
		if i > 0 {
			open = closes[i-1] * (1 + rng.NormFloat64()*0.002)
		}
		intraday := math.Abs(rng.NormFloat64() * annualVol * math.Sqrt(dt) * 0.7)
		bars[i] = model.Bar{
			Time:   dates[i],
			Open:   open,
			High:   math.Max(open, close) * (1 + intraday),
			Low:    math.Min(open, close) * (1 - intraday),
			Close:  close,
			Volume: 200_000 + float64(rng.Intn(2_800_000)),
		}
	}
	return model.Series{Symbol: symbol, Bars: bars, Source: "testfx"}
}

// TradingDays returns `days` business days ending the day before `end`,
// oldest first.
func TradingDays(days int, end time.Time) []time.Time {
	out := make([]time.Time, 0, days)
	day := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	for len(out) < days {
		day = day.AddDate(0, 0, -1)
		if weekday := day.Weekday(); weekday != time.Saturday && weekday != time.Sunday {
			out = append(out, day)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// fnvHash is FNV-1a: stable across processes, unlike a runtime-salted hash.
func fnvHash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
