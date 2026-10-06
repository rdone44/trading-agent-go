// Package cli implements the command line interface.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/marketdata"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/report"
	"github.com/huijun/trading-agent-go/internal/strategy"
	"github.com/huijun/trading-agent-go/internal/webui"
	"gopkg.in/yaml.v3"
)

const usage = `trading-agent - a small trading agent (Go)

Usage:
  trading-agent backtest [flags]   run a backtest and write a report
  trading-agent scan [flags]       backtest several symbols and rank them
  trading-agent live [flags]       paper-trade the latest bars (no real orders)
  trading-agent web [flags]        serve the interactive dashboard
  trading-agent strategies         list the built-in strategies
  trading-agent version            print the version

Examples:
  trading-agent backtest --symbol AAPL --days 730 --seed 42
  trading-agent backtest --provider yahoo --symbol MSFT --days 900
  trading-agent backtest --provider binance --symbol BTCUSDT --days 730
  trading-agent backtest --strategy breakout --symbol AAPL
  trading-agent scan --symbols AAPL,MSFT,NVDA,SPY
  trading-agent web --addr :8080

Run "trading-agent backtest -h" for the full flag list.
`

// Run dispatches a command and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	switch args[0] {
	case "backtest":
		return runBacktest(args[1:])
	case "scan":
		return runScan(args[1:])
	case "live":
		return runLive(args[1:])
	case "web":
		return runWeb(args[1:])
	case "strategies":
		fmt.Println("built-in strategies:")
		for _, name := range strategy.Available() {
			fmt.Printf("  %s\n", name)
		}
		return 0
	case "version", "-v", "--version":
		fmt.Println("trading-agent 0.1.0 (go)")
		return 0
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// flags holds every CLI option. Unset options stay at their zero value and are
// ignored, so values from the config file survive.
type flags struct {
	configPath *string
	symbol     *string
	days       *int
	provider   *string
	dataPath   *string
	strategy   *string
	cash       *float64
	seed       *int64
	output     *string
	warmup     *int
	jsonOut    *bool
	symbols    *string
	iterations *int
	poll       *int
	saveConfig *string
	addr       *string
	noOpen     *bool
}

func bind(fs *flag.FlagSet, f *flags) {
	f.configPath = fs.String("config", "", "YAML config file (default: ./config.yaml when present)")
	f.symbol = fs.String("symbol", "", "ticker, e.g. AAPL")
	f.days = fs.Int("days", 0, "calendar days of history")
	f.provider = fs.String("provider", "", "synthetic | yahoo | binance | csv")
	f.dataPath = fs.String("data", "", "CSV file of bars (sets provider=csv)")
	f.strategy = fs.String("strategy", "", "ma_cross | rsi_reversion | breakout")
	f.cash = fs.Float64("cash", 0, "initial cash")
	f.seed = fs.Int64("seed", 0, "synthetic data seed")
	f.output = fs.String("output", "", "report directory")
	f.warmup = fs.Int("warmup", 0, "bars to skip before entries")
	f.jsonOut = fs.Bool("json", false, "print metrics as JSON only")
	f.symbols = fs.String("symbols", "", "comma-separated tickers (scan)")
	f.iterations = fs.Int("iterations", 1, "update cycles (live)")
	f.poll = fs.Int("poll", 0, "seconds between cycles (live)")
	f.saveConfig = fs.String("save-config", "", "write the effective config to this path")
	f.addr = fs.String("addr", ":8080", "dashboard listen address")
	f.noOpen = fs.Bool("no-open", false, "do not open a browser automatically")
}

func (f flags) overrides() config.Overrides {
	o := config.Overrides{}
	if set(f.symbol) {
		o.Symbol = f.symbol
	}
	if set(f.days) {
		o.HistoryDays = f.days
	}
	if set(f.provider) {
		o.Provider = f.provider
	}
	if set(f.dataPath) {
		o.CSVPath = f.dataPath
		// Supplying a data file implies the csv provider unless one was named.
		if !set(f.provider) {
			csv := "csv"
			o.Provider = &csv
		}
	}
	if set(f.strategy) {
		o.Strategy = f.strategy
	}
	if set(f.cash) {
		o.InitialCash = f.cash
	}
	if set(f.seed) {
		o.Seed = f.seed
	}
	if set(f.output) {
		o.OutputDir = f.output
	}
	if set(f.warmup) {
		o.WarmupBars = f.warmup
	}
	return o
}

// set reports whether the flag was given a non-zero value.
func set[T comparable](value *T) bool {
	if value == nil {
		return false
	}
	var zero T
	return *value != zero
}

func loadConfig(f flags) (config.Config, error) {
	path := ""
	if set(f.configPath) {
		path = *f.configPath
	} else if _, err := os.Stat("config.yaml"); err == nil {
		path = "config.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	cfg.Apply(f.overrides())
	return cfg, nil
}

func runBacktest(args []string) int {
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	var f flags
	bind(fs, &f)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(f)
	if err != nil {
		return fail(err)
	}
	result, err := execute(cfg)
	if err != nil {
		return fail(err)
	}

	if set(f.jsonOut) && *f.jsonOut {
		encoded, err := json.MarshalIndent(result.Metrics, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(encoded))
		return 0
	}

	printSummary(result)
	paths, err := report.Write(result, cfg.Backtest.OutputDir, "")
	if err != nil {
		return fail(err)
	}
	if set(f.saveConfig) {
		if err := saveConfig(cfg, *f.saveConfig); err != nil {
			return fail(err)
		}
		fmt.Printf("\neffective config -> %s\n", *f.saveConfig)
	}
	fmt.Printf(`
reports written
  HTML report : %s
  Markdown    : %s
  Metrics JSON: %s
  Trades CSV  : %s
`, paths.HTML, paths.Markdown, paths.MetricsJSON, paths.TradesCSV)
	return 0
}

func runScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	var f flags
	bind(fs, &f)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	base, err := loadConfig(f)
	if err != nil {
		return fail(err)
	}
	symbols := "AAPL,MSFT,NVDA,SPY"
	if set(f.symbols) {
		symbols = *f.symbols
	}

	rows := []scanRow{}
	for _, symbol := range strings.Split(symbols, ",") {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" {
			continue
		}
		cfg := base
		cfg.Agent.Symbol = symbol
		result, err := execute(cfg)
		rows = append(rows, scanRow{symbol: symbol, result: result, err: err})
	}

	// Insertion sort: rank by total return, best first, failures last.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			if !lessRow(rows[j], rows[j-1]) {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}

	fmt.Printf("%-8s %12s %14s %10s %8s %8s %10s\n",
		"symbol", "total", "annual", "max DD", "sharpe", "trades", "win rate")
	fmt.Println(strings.Repeat("-", 78))
	for _, r := range rows {
		if r.err != nil {
			fmt.Printf("%-8s %s\n", r.symbol, r.err)
			continue
		}
		m := r.result.Metrics
		fmt.Printf("%-8s %11s%% %13s%% %9s%% %8s %8d %9s%%\n",
			r.symbol, plain(m.TotalReturnPct, 2), plain(m.AnnualReturnPct, 2),
			plain(m.MaxDrawdownPct, 2), plain(m.Sharpe, 2), m.NumTrades,
			plain(m.WinRatePct, 1))
	}
	return 0
}

// runWeb serves the interactive dashboard.
func runWeb(args []string) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	var f flags
	bind(fs, &f)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(f)
	if err != nil {
		return fail(err)
	}
	addr := ":8080"
	if set(f.addr) {
		addr = *f.addr
	}
	server := webui.New(cfg)

	// Open the browser once the listener is up, unless told not to.
	if !(set(f.noOpen) && *f.noOpen) {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openBrowser("http://" + displayAddr(addr))
		}()
	}
	fmt.Printf("trading-agent dashboard\n  reports: %s\n  stop with Ctrl+C\n\n", cfg.Backtest.OutputDir)
	if err := server.Listen(addr); err != nil {
		return fail(err)
	}
	return 0
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// openBrowser launches the default browser. Failure is not fatal: the URL is
// printed anyway, and a headless machine simply has nothing to open.
func openBrowser(url string) {
	var command string
	var args []string
	switch runtime.GOOS {
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		command, args = "open", []string{url}
	default:
		command, args = "xdg-open", []string{url}
	}
	_ = exec.Command(command, args...).Start()
}

func runLive(args []string) int {
	fs := flag.NewFlagSet("live", flag.ContinueOnError)
	var f flags
	bind(fs, &f)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(f)
	if err != nil {
		return fail(err)
	}
	if !cfg.Live.PaperTrading {
		return fail(fmt.Errorf("live.paper_trading is false; refusing to run without a broker adapter"))
	}
	if set(f.days) {
		cfg.Live.LookbackDays = *f.days
	}

	iterations := 1
	if set(f.iterations) {
		iterations = *f.iterations
	}
	poll := cfg.Live.PollSeconds
	if set(f.poll) {
		poll = *f.poll
	}

	var result engine.Result
	for step := 0; step < max(iterations, 1); step++ {
		cfg.Agent.HistoryDays = cfg.Live.LookbackDays
		result, err = execute(cfg)
		if err != nil {
			return fail(err)
		}
		openPositions := 0
		if len(result.Equity) > 0 {
			openPositions = result.Equity[len(result.Equity)-1].OpenPositions
		}
		fmt.Printf("[live] %s  equity=%s  positions=%d  orders=%d\n",
			result.End.Format("2006-01-02"),
			plain(result.Metrics.FinalEquity, 2), openPositions, len(result.Orders))
		if step < iterations-1 {
			time.Sleep(time.Duration(poll) * time.Second)
		}
	}
	printSummary(result)
	return 0
}

// execute loads data and runs the agent end to end.
func execute(cfg config.Config) (engine.Result, error) {
	series, err := loadSeries(cfg)
	if err != nil {
		return engine.Result{}, err
	}
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		return engine.Result{}, err
	}
	agent := engine.New(cfg, strat)
	return agent.RunBacktest(series)
}

func loadSeries(cfg config.Config) (model.Series, error) {
	symbol := strings.ToUpper(cfg.Agent.Symbol)
	switch strings.ToLower(cfg.Data.Provider) {
	case "synthetic", "":
		return marketdata.Synthetic(symbol, cfg.Agent.HistoryDays, cfg.Data.Synthetic, time.Now().UTC(), nil)
	case "yahoo":
		return marketdata.Yahoo(symbol, cfg.Agent.HistoryDays, time.Now().UTC())
	case "binance":
		return marketdata.Binance(symbol, cfg.Agent.HistoryDays, time.Now().UTC())
	case "csv":
		if cfg.Data.CSVPath == "" {
			return model.Series{}, fmt.Errorf("provider csv requires --data <file.csv>")
		}
		return marketdata.FromCSV(cfg.Data.CSVPath, symbol)
	default:
		return model.Series{}, fmt.Errorf("unknown data provider %q (use synthetic, yahoo, binance or csv)", cfg.Data.Provider)
	}
}

func printSummary(result engine.Result) {
	m := result.Metrics
	fmt.Printf("\n%s - %s\n", result.Symbol, result.Strategy)
	fmt.Printf("  period              %s .. %s (%d bars, %s)\n",
		result.Start.Format("2006-01-02"), result.End.Format("2006-01-02"), result.Bars, result.DataSource)
	fmt.Printf("  final equity        %s\n", plain(m.FinalEquity, 2))
	fmt.Printf("  total return        %s%%\n", plain(m.TotalReturnPct, 2))
	fmt.Printf("  annual return       %s%%\n", plain(m.AnnualReturnPct, 2))
	fmt.Printf("  annual volatility   %s%%\n", plain(m.AnnualVolatilityPct, 2))
	fmt.Printf("  sharpe / sortino    %s / %s\n", plain(m.Sharpe, 2), plain(m.Sortino, 2))
	fmt.Printf("  max drawdown        %s%%\n", plain(m.MaxDrawdownPct, 2))
	fmt.Printf("  exposure            %s%%\n", plain(m.ExposurePct, 2))
	fmt.Printf("  trades              %d (win rate %s%%, PF %s)\n",
		m.NumTrades, plain(m.WinRatePct, 1), plain(m.ProfitFactor, 2))
	fmt.Printf("  total fees          %.2f\n", m.TotalFees)
}

// scanRow is one symbol's result in the ranking table.
type scanRow struct {
	symbol string
	result engine.Result
	err    error
}

// lessRow orders rows best-first: successful runs by total return, failures last.
func lessRow(a, b scanRow) bool {
	switch {
	case a.err != nil:
		return false
	case b.err != nil:
		return true
	default:
		return ret(a.result) > ret(b.result)
	}
}

func ret(r engine.Result) float64 {
	if r.Metrics.TotalReturnPct == nil {
		return 0
	}
	return *r.Metrics.TotalReturnPct
}

func plain(v *float64, digits int) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.*f", digits, *v)
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

// saveConfig writes the effective configuration as YAML so a run can be
// reproduced later.
func saveConfig(cfg config.Config, path string) error {
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}
