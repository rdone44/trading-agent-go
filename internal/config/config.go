// Package config defines the typed configuration and the CLI override rules.
package config

import (
	"fmt"
	"os"

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
	LLM       LLM       `yaml:"llm"`
}

type Agent struct {
	Name        string `yaml:"name"`
	Symbol      string `yaml:"symbol"`
	Timeframe   string `yaml:"timeframe"`
	HistoryDays int    `yaml:"history_days"`
}

type Data struct {
	Provider string `yaml:"provider"`
	// BarsPerYear is the annualization factor used for Sharpe, volatility and
	// the annual return. Zero means "infer from the provider" (Binance = 365).
	BarsPerYear int `yaml:"bars_per_year"`
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
	// Leverage is the margin multiplier for a position: notional = margin *
	// Leverage, so the same risk budget controls more notional. 1 means an
	// unleveraged perpetual position, which is still a perpetual: the position
	// can be shorted and the exchange, not the wallet, holds the inventory.
	Leverage int `yaml:"leverage"`
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
	PollSeconds  int    `yaml:"poll_seconds"`
	LookbackDays int    `yaml:"lookback_days"`
	PaperTrading bool   `yaml:"paper_trading"`
	StateFile    string `yaml:"state_file"`
	MarginMode   string `yaml:"margin_mode"` // "ISOLATED" (default) or "CROSS"
	// ExchangeAPIKey / ExchangeSecretKey carry the per-user Binance
	// credentials the webui injects from the credential vault. They are
	// yaml:"-" on purpose: secrets live in the vault (0600, hashed users),
	// never in a YAML config file. When empty, the runner falls back to the
	// BINANCE_API_KEY / BINANCE_SECRET_KEY environment variables.
	ExchangeAPIKey    string `yaml:"-"`
	ExchangeSecretKey string `yaml:"-"`
	// NoEnvKeys disables that environment fallback. The webui sets it for
	// accounts-mode sessions: the exchange credentials must come from the
	// logged-in user's own vault, never from the deployer's BINANCE_API_KEY /
	// BINANCE_SECRET_KEY. Without it, any registered user who stored no keys
	// could start a live session that traded the deployer's account. The CLI
	// and desktop builds (no vault) leave it false and keep the historical
	// environment behaviour. yaml:"-" so a config file cannot toggle it.
	NoEnvKeys bool `yaml:"-"`
}

// LLM configures the OpenAI-compatible model used by the AI features
// (the llm strategy, the entry veto, the post-mortem review and the tune
// loop). The API key is deliberately NOT part of this block: it is read
// from the LLM_API_KEY (or OPENAI_API_KEY) environment variable at call
// time, so a config file can never leak a secret.
type LLM struct {
	BaseURL string `yaml:"base_url"` // default https://api.openai.com/v1
	Model   string `yaml:"model"`    // default gpt-4o-mini
	// TimeoutSec bounds one chat completion. The default is generous because
	// a trading prompt carries the indicator snapshot and asks for a JSON
	// answer, and a reasoning model can take well over a minute to produce
	// it: a 30s budget turned healthy calls into "context deadline exceeded"
	// on the first real model. The live loop polls on its own interval and
	// keeps the current position when a call times out, so a slow answer
	// delays a decision rather than corrupting one.
	TimeoutSec  int     `yaml:"timeout_sec"` // default 180
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
	// VetoEnabled gates new live entries with an LLM second opinion.
	// Fail-open: a model error or a missing key lets the entry through.
	VetoEnabled bool `yaml:"veto_enabled"`
	// VetoCacheSec memoizes a veto verdict for this many seconds, so repeated
	// proposals for the same position do not bill the model again. The hard
	// risk limits (stop distance, size, kill switch) are still re-checked every
	// cycle; only the soft model opinion is cached. 0 disables caching.
	VetoCacheSec int `yaml:"veto_cache_sec"`
	// APIKey is the model credential the webui injects from the per-user
	// credential vault. yaml:"-" so a config file can never carry it; when
	// empty, llm.New falls back to the LLM_API_KEY / OPENAI_API_KEY
	// environment variables.
	APIKey string `yaml:"-"`
	// Prompt is the trading persona the llm strategy prepends to its
	// answer-contract block. It is DATA, not code: the prompt-tune loop
	// iterates this text against backtest metrics, and the winning text is
	// saved back here. Empty keeps the built-in conservative persona, which
	// preserves byte-identical behaviour for every existing config file.
	Prompt string `yaml:"prompt,omitempty"`
	// NoEnvKey disables the environment-variable key fallback in llm.New.
	// The webui sets this for accounts-mode requests: the model must use only
	// the credential the user stored in their own vault. Without it, a user
	// who points BaseURL at their own endpoint and leaves the key empty would
	// leak the deployer's LLM_API_KEY to the user-controlled endpoint. yaml:"-"
	// so it can never be toggled from a config file.
	NoEnvKey bool `yaml:"-"`
}

// Default returns the built-in settings used when no config file is present.
func Default() Config {
	return Config{
		Agent:    Agent{Name: "trading-agent", Symbol: "BTCUSDT", Timeframe: "1d", HistoryDays: 730},
		Data:     Data{Provider: "binance"},
		Strategy: Strategy{Name: "ma_cross", Params: map[string]float64{}},
		Risk: Risk{
			InitialCash: 100_000, MaxPositionPct: 0.95, MaxRiskPerTradePct: 0.02,
			StopLossPct: f(0.06), TakeProfitPct: f(0.18),
			MaxDrawdownPct: f(0.25), MaxDailyLossPct: f(0.05),
			AllowShort: false, MaxOpenPositions: 1, Leverage: 1,
		},
		Execution: Execution{
			CommissionBps: 1, SlippageBps: 5, MinTradeNotional: 100,
			LiquidateAtEnd: true, LotSize: 0.0001,
		},
		Backtest: Backtest{WarmupBars: 60, OutputDir: "reports"},
		Live:     Live{PollSeconds: 60, LookbackDays: 400, PaperTrading: true},
		LLM: LLM{
			BaseURL:      "https://api.openai.com/v1",
			Model:        "gpt-4o-mini",
			TimeoutSec:   180,
			MaxTokens:    1024,
			VetoCacheSec: 900,
		},
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
		cfg.Agent.Symbol = "BTCUSDT"
	}
	if cfg.Data.Provider == "" {
		cfg.Data.Provider = "binance"
	}
	// A Leverage below 1 is nonsensical (that would be a discount), clamp to 1.
	// Leverage does NOT force AllowShort: long-only perpetuals is a valid,
	// safer default. To use the short side the user sets Risk.AllowShort
	// explicitly.
	if cfg.Risk.Leverage < 1 {
		cfg.Risk.Leverage = 1
	}
	return cfg, nil
}

// Overrides carries optional CLI values; nil means "not provided".
type Overrides struct {
	Symbol      *string
	HistoryDays *int
	Strategy    *string
	InitialCash *float64
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
	if o.Strategy != nil {
		c.Strategy.Name = *o.Strategy
	}
	if o.InitialCash != nil {
		c.Risk.InitialCash = *o.InitialCash
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
// The only data source is Binance and crypto trades every day, so bars are
// annualized over 365 periods. Getting this wrong silently distorts the Sharpe
// ratio and the annualized return; an explicit data.bars_per_year override is
// honoured when set.
func (c Config) BarsPerYear() int {
	if c.Data.BarsPerYear > 0 {
		return c.Data.BarsPerYear
	}
	return 365
}

// IntParam is Param for integer parameters.
func (c Config) IntParam(key string, def int) int {
	return int(c.Param(key, float64(def)))
}

// LLMAPIKey returns the API key the LLM features should use. It is read only
// from the environment so a secret can never be committed in a config file:
// LLM_API_KEY wins, falling back to OPENAI_API_KEY. An empty result means
// the AI features are unavailable (they degrade to their non-LLM behaviour).
func LLMAPIKey() string {
	if k := os.Getenv("LLM_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("OPENAI_API_KEY")
}

func f(v float64) *float64 { return &v }
