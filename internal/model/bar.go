// Package model holds the shared market-data types.
package model

import (
	"math"
	"time"
)

// Bar is one OHLCV candle.
type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Series is an ordered, immutable set of bars for one symbol.
type Series struct {
	Symbol string
	Bars   []Bar
	Source string
}

func (s Series) Len() int { return len(s.Bars) }

// Open/High/Low/Close return a column as a plain slice, which keeps the
// indicator functions free of any struct plumbing.
func (s Series) Open() []float64  { return column(s.Bars, func(b Bar) float64 { return b.Open }) }
func (s Series) High() []float64  { return column(s.Bars, func(b Bar) float64 { return b.High }) }
func (s Series) Low() []float64   { return column(s.Bars, func(b Bar) float64 { return b.Low }) }
func (s Series) Close() []float64 { return column(s.Bars, func(b Bar) float64 { return b.Close }) }

func column(bars []Bar, pick func(Bar) float64) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = pick(b)
	}
	return out
}

// NaN is the "no value" marker used by every indicator.
func NaN() float64 { return math.NaN() }

// IsNaN reports whether v is NaN (a missing value).
func IsNaN(v float64) bool { return math.IsNaN(v) }

// Valid reports whether v is a usable number (not NaN and not Inf).
func Valid(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// NaNOr returns v, or def when v is missing.
func NaNOr(v, def float64) float64 {
	if Valid(v) {
		return v
	}
	return def
}
