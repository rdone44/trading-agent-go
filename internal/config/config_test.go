package config

import "testing"

// The only data source is Binance and crypto trades every day, so bars are
// annualized over 365 periods unless an explicit override is set.
func TestBarsPerYearDefaultsToCrypto(t *testing.T) {
	cases := []struct {
		provider string
		override int
		want     int
	}{
		{"binance", 0, 365},
		{"", 0, 365},
		{"binance", 500, 500},
		{"", 500, 500},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Data.Provider = tc.provider
		cfg.Data.BarsPerYear = tc.override
		if got := cfg.BarsPerYear(); got != tc.want {
			t.Errorf("provider %q override %d: BarsPerYear() = %d, want %d",
				tc.provider, tc.override, got, tc.want)
		}
	}
}

func TestDefaultBarsPerYear(t *testing.T) {
	if got := Default().BarsPerYear(); got != 365 {
		t.Fatalf("default BarsPerYear() = %d, want 365", got)
	}
}
