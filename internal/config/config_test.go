package config

import "testing"

// The annualization factor feeds Sharpe, volatility and the annual return, so
// it has to follow the data source: stocks trade ~252 days a year, crypto 365.
func TestBarsPerYearFollowsTheProvider(t *testing.T) {
	cases := []struct {
		provider string
		override int
		want     int
	}{
		{"synthetic", 0, 252},
		{"yahoo", 0, 252},
		{"csv", 0, 252},
		{"binance", 0, 365},
		{"BINANCE", 0, 365},
		{"binance", 500, 500},
		{"yahoo", 365, 365},
		{"", 0, 252},
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

func TestDefaultIsStocksCalendar(t *testing.T) {
	if got := Default().BarsPerYear(); got != 252 {
		t.Fatalf("default BarsPerYear() = %d, want 252", got)
	}
}
