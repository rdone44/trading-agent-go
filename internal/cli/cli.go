// Package cli implements the command line interface.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huijun/trading-agent-go/internal/config"
	"github.com/huijun/trading-agent-go/internal/engine"
	"github.com/huijun/trading-agent-go/internal/live"
	"github.com/huijun/trading-agent-go/internal/marketdata"
	"github.com/huijun/trading-agent-go/internal/model"
	"github.com/huijun/trading-agent-go/internal/report"
	"github.com/huijun/trading-agent-go/internal/strategy"
	"github.com/huijun/trading-agent-go/internal/webui"
	"gopkg.in/yaml.v3"
)

const usage = `trading-agent - a small trading agent (Go)

Data source: Binance spot (public REST API), daily bars.

Usage:
  trading-agent backtest [flags]   run a backtest and write a report
  trading-agent scan [flags]       backtest several symbols and rank them
  trading-agent live [flags]       paper-trade the latest bars (no real orders)
  trading-agent trade [flags]      run the live trading loop (paper by default;
                                   --execute places real orders)
  trading-agent web [flags]        serve the interactive dashboard
  trading-agent strategies         list the built-in strategies
  trading-agent version            print the version

Examples:
  trading-agent backtest --symbol BTCUSDT --days 730
  trading-agent backtest --strategy breakout --symbol ETHUSDT --days 730
  trading-agent scan --symbols BTCUSDT,ETHUSDT,SOLUSDT,XRPUSDT
  trading-agent trade --symbol BTCUSDT --strategy rsi_reversion
  trading-agent trade --execute   # real orders: needs BINANCE_API_KEY /
                                  # BINANCE_SECRET_KEY and a typed "yes"
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
	case "trade":
		return runTrade(args[1:])
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
	strategy   *string
	cash       *float64
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
	f.symbol = fs.String("symbol", "", "Binance ticker, e.g. BTCUSDT")
	f.days = fs.Int("days", 0, "calendar days of history")
	f.strategy = fs.String("strategy", "", "ma_cross | rsi_reversion | breakout")
	f.cash = fs.Float64("cash", 0, "initial cash")
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
	if set(f.strategy) {
		o.Strategy = f.strategy
	}
	if set(f.cash) {
		o.InitialCash = f.cash
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
	symbols := "BTCUSDT,ETHUSDT,SOLUSDT"
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

func runTrade(args []string) int {
	fs := flag.NewFlagSet("trade", flag.ContinueOnError)
	var f flags
	bind(fs, &f)

	// trade-specific flags; the shared ones are bound first.
	type tradeFlags struct {
		execute   *bool
		stateFile *string
		yes       *bool
		cycles    *int
		futures   *bool
		leverage  *int
	}
	var tf tradeFlags
	tf.execute = fs.Bool("execute", false, "place real orders (default: paper, no orders)")
	tf.stateFile = fs.String("state", "", "state file for restart persistence (default ./trade-state.json when present)")
	tf.yes = fs.Bool("yes", false, "skip the confirmation prompt when --execute is used")
	tf.cycles = fs.Int("cycles", 0, "run exactly N cycles then exit (0 = run until stopped)")
	tf.futures = fs.Bool("futures", false, "trade USDT-margined perpetuals (leverage + shorts)")
	tf.leverage = fs.Int("leverage", 0, "leverage multiplier for the futures venue (default from config, 1 = spot-like)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(f)
	if err != nil {
		return fail(err)
	}
	if set(f.days) {
		cfg.Live.LookbackDays = *f.days
	}
	// Venue and leverage come from flags, defaulting to the config file.
	if set(tf.futures) {
		cfg.Live.Futures = *tf.futures
	}
	if set(tf.leverage) {
		if *tf.leverage < 1 {
			return fail(fmt.Errorf("--leverage 必须 >= 1"))
		}
		cfg.Risk.Leverage = *tf.leverage
		// Leverage only exists on a futures venue: a multiplier > 1 must
		// point the run at perpetuals, otherwise a spot account would be
		// asked to post margin it cannot. The short side is left to
		// Risk.AllowShort (long-only futures is a valid, safer default).
		if cfg.Risk.Leverage > 1 {
			cfg.Live.Futures = true
		}
	}

	execute := set(tf.execute) && *tf.execute
	strat, err := strategy.New(cfg.Strategy.Name, cfg)
	if err != nil {
		return fail(err)
	}

	statePath := ""
	if set(tf.stateFile) {
		statePath = *tf.stateFile
	} else if cfg.Live.StateFile != "" {
		statePath = cfg.Live.StateFile
	} else {
		statePath = "trade-state.json"
	}

	runner, err := live.New(cfg, strat, execute, statePath)
	if err != nil {
		return fail(err)
	}

	if execute {
		if !(set(tf.yes) && *tf.yes) {
			fmt.Printf("⚠  将以真实订单交易 %s（策略 %s，初始资金 %.0f）。\n",
				runner.Agent().Symbol, strat.Describe(), cfg.Risk.InitialCash)
			if !confirmExecute() {
				fmt.Println("已取消。真实订单需要交互输入 yes，或在非交互环境使用 --yes。")
				return 0
			}
		}
	}

	if err := runner.Init(); err != nil {
		return fail(err)
	}

	mode := "paper"
	if execute {
		mode = "EXECUTE"
	}
	fmt.Printf("trade loop started: %s %s [mode=%s] state=%s\n",
		cfg.Agent.Symbol, strat.Describe(), mode, statePath)
	if execute {
		fmt.Printf("  交易所余额校验通过；每周期最多下一单。按 Ctrl+C 安全停止并保存状态。\n\n")
	}

	// Poll for as long as Ctrl+C is not pressed.
	poll := cfg.Live.PollSeconds
	if set(f.poll) {
		poll = *f.poll
	}
	if poll <= 0 {
		poll = 60
	}

	ticker := time.NewTicker(time.Duration(poll) * time.Second)
	defer ticker.Stop()

	// done is closed exactly once — by either a signal handler or the cycle
	// limit — to avoid a double-close panic.
	done := make(chan struct{})
	var closeOnce sync.Once
	stop := func() { closeOnce.Do(func() { close(done) }) }

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range signals {
			fmt.Printf("\nstopping and saving state…\n")
			stop()
			return
		}
	}()

	// Run an initial cycle immediately, then one per tick until stopped.
	// With --cycles N the loop exits after N cycles instead of running
	// until stopped, which makes the poll loop testable and usable for a
	// quick single-pass run.
	maxCycles := 0
	if set(tf.cycles) {
		maxCycles = *tf.cycles
	}

	cycleNo := 0
	for {
		cycleNo++
		if err := runCycle(runner, time.Now().UTC()); err != nil {
			fmt.Fprintf(os.Stderr, "cycle error: %v\n", err)
		}
		if maxCycles > 0 && cycleNo >= maxCycles {
			break
		}
		select {
		case <-ticker.C:
		case <-done:
			// Interrupted by a signal or by the --cycles limit.
		}
	}

	if err := runner.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 保存状态失败: %v\n", err)
	} else if statePath != "" {
		fmt.Printf("state saved -> %s\n", statePath)
	}
	return 0
}

// runCycle fetches the latest price, runs one agent step, and persists.
func runCycle(runner *live.Runner, now time.Time) error {
	res, err := runner.Cycle(now)
	if err != nil {
		return err
	}
	open := "flat"
	if res.Open != nil {
		open = fmt.Sprintf("%s %.6f @ %.2f", res.Open.Side, res.Open.Quantity, res.Open.EntryPrice)
	}
	fmt.Printf("[%s] %-22s equity=%12.2f  cash=%12.2f  open=%s\n",
		now.Format("2006-01-02 15:04:05"), res.Action, res.Equity, res.Cash, open)
	return nil
}

// confirmExecute asks for a typed "yes" on stdin before real orders run. A
// non-interactive session (no TTY) or any other answer declines.
func confirmExecute() bool {
	var answer string
	fmt.Print("输入 yes 确认真实订单 > ")
	if _, err := fmt.Scanln(&answer); err != nil || answer != "yes" {
		return false
	}
	return true
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
	return marketdata.Binance(symbol, cfg.Agent.HistoryDays, time.Now().UTC())
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
