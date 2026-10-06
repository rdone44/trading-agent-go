// Package config defines the typed configuration and the CLI override rules.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pointer fields distinguish "absent" from "zero", so a CLI flag can override
// a value and YAML can leave one unset.
type Config struct {
	Agent     Agent     `yaml:"agent"`
	Data      Data      `yaml:"data"`
	Strategy  Strategy  `yaml:"strategy"`
	Risk      Risk      `yaml:"risk"`
	Execution Execution `yaml:"execution"`
	Backtest  Backtest  `yaml:"backtest"`
	Live      Live      `yaml:"live"`
}

type Agent struct {
	Name        string `yaml:"name"`
	Symbol      string `yaml:"symbol"`
	Timeframe   string `yaml:"timeframe"`
	HistoryDays int    `yaml:"history_days"`
}

type Data struct {
	Provider string `yaml:"provider"`
	CSVPath  string `yaml:"csv_path"`
	// BarsPerYear is the annualization factor used for Sharpe, volatility and
	// the annual return. Zero means "infer from the provider".
	BarsPerYear int       `yaml:"bars_per_year"`
	Synthetic   Synthetic `yaml:"synthetic"`
}

type Synthetic struct {
	Seed             int64   `yaml:"seed"`
	StartPrice       float64 `yaml:"start_price"`
	AnnualDrift      float64 `yaml:"annual_drift"`
	AnnualVol        float64 `yaml:"annual_vol"`
	RegimeSwitchProb float64 `yaml:"regime_switch_prob"`
	BarsPerYear      int     `yaml:"bars_per_year"`
}

type Strategy struct {
	Name   string             `yaml:"name"`
	Params map[string]float64 `yaml:"params"`
}

type Risk struct {
	InitialCash        float64  `yaml:"initial_cash"`
	MaxPositionPct     float64  `yaml:"max_position_pct"`
	MaxRiskPerTradePct float64  `yaml:"max_risk_per_trade_pct"`
	StopLossPct        *float64 `yaml:"stop_loss_pct"`
	TakeProfitPct      *float64 `yaml:"take_profit_pct"`
	MaxDrawdownPct     *float64 `yaml:"max_drawdown_pct"`
	MaxDailyLossPct    *float64 `yaml:"max_daily_loss_pct"`
	AllowShort         bool     `yaml:"allow_short"`
	MaxOpenPositions   int      `yaml:"max_open_positions"`
}

type Execution struct {
	CommissionBps    float64 `yaml:"commission_bps"`
	SlippageBps      float64 `yaml:"slippage_bps"`
	MinTradeNotional float64 `yaml:"min_trade_notional"`
	LiquidateAtEnd   bool    `yaml:"liquidate_at_end"`
	LotSize          float64 `yaml:"lot_size"`
}

type Backtest struct {
	WarmupBars int    `yaml:"warmup_bars"`
	OutputDir  string `yaml:"output_dir"`
}

type Live struct {
	PollSeconds  int  `yaml:"poll_seconds"`
	LookbackDays int  `yaml:"lookback_days"`
	PaperTrading bool `yaml:"paper_trading"`
}

// Default returns the built-in settings used when no config file is present.
func Default() Config {
	return Config{
		Agent: Agent{Name: "trading-agent", Symbol: "AAPL", Timeframe: "1d", HistoryDays: 730},
		Data: Data{
			Provider: "synthetic",
			Synthetic: Synthetic{
				Seed: 42, StartPrice: 100, AnnualDrift: 0.08,
				AnnualVol: 0.28, RegimeSwitchProb: 0.01, BarsPerYear: 252,
			},
		},
		Strategy: Strategy{Name: "ma_cross", Params: map[string]float64{}},
		Risk: Risk{
			InitialCash: 100_000, MaxPositionPct: 0.95, MaxRiskPerTradePct: 0.02,
			StopLossPct: f(0.06), TakeProfitPct: f(0.18),
			MaxDrawdownPct: f(0.25), MaxDailyLossPct: f(0.05),
			AllowShort: false, MaxOpenPositions: 1,
		},
		Execution: Execution{
			CommissionBps: 1, SlippageBps: 5, MinTradeNotional: 100,
			LiquidateAtEnd: true, LotSize: 0.0001,
		},
		Backtest: Backtest{WarmupBars: 60, OutputDir: "reports"},
		Live:     Live{PollSeconds: 60, LookbackDays: 400, PaperTrading: true},
	}
}

// Load reads a YAML file and overlays it on the defaults. A missing path is an
// error; an empty path returns the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Strategy.Params == nil {
		cfg.Strategy.Params = map[string]float64{}
	}
	if cfg.Agent.Symbol == "" {
		cfg.Agent.Symbol = "AAPL"
	}
	if cfg.Data.Provider == "" {
		cfg.Data.Provider = "synthetic"
	}
	return cfg, nil
}

// Overrides carries optional CLI values; nil means "not provided".
type Overrides struct {
	Symbol      *string
	HistoryDays *int
	Provider    *string
	CSVPath     *string
	Strategy    *string
	InitialCash *float64
	Seed        *int64
	OutputDir   *string
	WarmupBars  *int
}

// Apply mutates the config with the provided overrides.
func (c *Config) Apply(o Overrides) {
	if o.Symbol != nil {
		c.Agent.Symbol = *o.Symbol
	}
	if o.HistoryDays != nil {
		c.Agent.HistoryDays = *o.HistoryDays
	}
	if o.Provider != nil {
		c.Data.Provider = *o.Provider
	}
	if o.CSVPath != nil {
		c.Data.CSVPath = *o.CSVPath
	}
	if o.Strategy != nil {
		c.Strategy.Name = *o.Strategy
	}
	if o.InitialCash != nil {
		c.Risk.InitialCash = *o.InitialCash
	}
	if o.Seed != nil {
		c.Data.Synthetic.Seed = *o.Seed
	}
	if o.OutputDir != nil {
		c.Backtest.OutputDir = *o.OutputDir
	}
	if o.WarmupBars != nil {
		c.Backtest.WarmupBars = *o.WarmupBars
	}
}

// Param reads a strategy parameter with a default.
func (c Config) Param(key string, def float64) float64 {
	if v, ok := c.Strategy.Params[key]; ok {
		return v
	}
	return def
}

// BarsPerYear is the annualization factor for the active data source.
//
// Stocks trade about 252 days a year; crypto trades every day, so Binance bars
// are annualized over 365 periods. Getting this wrong silently distorts the
// Sharpe ratio and the annualized return, so it is derived from the provider
// rather than left to the caller.
func (c Config) BarsPerYear() int {
	if c.Data.BarsPerYear > 0 {
		return c.Data.BarsPerYear
	}
	if strings.EqualFold(strings.TrimSpace(c.Data.Provider), "binance") {
		return 365
	}
	if c.Data.Synthetic.BarsPerYear > 0 {
		return c.Data.Synthetic.BarsPerYear
	}
	return 252
}

// IntParam is Param for integer parameters.
func (c Config) IntParam(key string, def int) int {
	return int(c.Param(key, float64(def)))
}

func f(v float64) *float64 { return &v }
