# trading-agent (Go)

A small, readable trading agent in Go: load market data, run a strategy, size
every trade through a risk manager, execute on a paper or live broker, and
write a report you can open in the browser.

```
data -> strategy -> risk -> broker -> portfolio -> metrics -> report
```

Two execution venues: **Binance spot** and **Binance USDT-margined perpetual
futures** (leverage, long and short). The live loop is paper by default and
only places real orders with an explicit `--execute` and a typed `yes`.

**This is research/education code, not investment advice.** Futures and
leverage can lose more than the margin posted; treat a live run accordingly.

## Features

- Three built-in strategies: `ma_cross`, `rsi_reversion`, `breakout`
- Risk layer: position sizing (leverage-aware), ATR/% stops, take-profit, max drawdown kill switch, daily loss limit
- Data source: Binance spot **and** USDT-perp futures (public REST API, daily bars)
- Two brokers: paper (simulated fills) and live Binance spot / futures, behind one `Broker` interface
- Realistic frictions: commission, slippage, lot rounding, minimum notional
- Look-ahead safe: signals decided on the close, executed on the next open
- Live loop: state persistence, restart reconciliation, idempotent client order ids
- AI features: an LLM strategy, an entry veto, a post-mortem reviewer and a parameter-tuning loop, all behind one OpenAI-compatible client
- Reports: HTML (with an inline SVG equity curve), Markdown, CSV and JSON
- Only one dependency (`yaml.v3`); the Binance clients and the LLM client use `net/http`

## Quickstart

Requires Go 1.23+.

```powershell
cd E:\agent\trading-agent-go

go test ./...                                  # unit + integration tests
go build -o bin/trading-agent.exe ./cmd/trading-agent

.\bin\trading-agent.exe web                    # dashboard at localhost:8080
.\bin\trading-agent.exe backtest --symbol BTCUSDT --days 730   # Binance data
```

## Dashboard

```powershell
.\bin\trading-agent.exe web                    # opens http://localhost:8080
.\bin\trading-agent.exe web --addr :9000       # another port
.\bin\trading-agent.exe web --no-open          # do not launch a browser
```

The dashboard is a single embedded page (Chinese UI) - no CDN, no JavaScript
framework, no build step - so it works offline and ships inside the binary:

- **Config form** for instrument, data source, strategy parameters, risk limits and costs
- **Charts** drawn as inline SVG: equity curve and underwater (drawdown) plot
- **Blotter** tabs for closed trades, every fill, breached risk limits and the raw metric dump
- **Run history** that lists saved runs and reopens any of them from disk

Every run started from the UI is written to `reports/<run-name>/` exactly like a
CLI run, including `report.html`, so results stay reproducible and shareable.
The HTML and Markdown reports are Chinese too.

The HTTP API behind it is small enough to script against:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/config` | effective config plus strategy metadata |
| `POST` | `/api/backtest` | run a backtest, persist it, return metrics and curves |
| `GET` | `/api/runs` | list saved runs, newest first |
| `GET` | `/api/run?name=...` | reopen a saved run's curves, trades and orders |
| `GET` | `/runs/<name>/report.html` | the static report of a saved run |

```powershell
curl.exe -X POST http://localhost:8080/api/backtest `
  -H "Content-Type: application/json" `
  -d '{\"symbol\":\"BTCUSDT\",\"strategy\":\"breakout\",\"days\":900}'
```

## Commands

```powershell
# dashboard
.\bin\trading-agent.exe web

# backtest on Binance spot klines (BTCUSDT, ETHUSDT, ...)
.\bin\trading-agent.exe backtest --symbol BTCUSDT --days 730

# compare strategies on the same data
.\bin\trading-agent.exe backtest --strategy rsi_reversion --symbol ETHUSDT
.\bin\trading-agent.exe backtest --strategy breakout --symbol BTCUSDT

# scan many tickers and rank them
.\bin\trading-agent.exe scan --symbols BTCUSDT,ETHUSDT,SOLUSDT

# paper-trade loop (simulated, no orders leave your machine)
.\bin\trading-agent.exe live --symbol BTCUSDT --iterations 3 --poll 10

# continuous trading loop (paper by default; real orders only with --execute)
.\bin\trading-agent.exe trade --symbol BTCUSDT --strategy rsi_reversion

# perpetual venue with leverage; shorts allowed. --execute + BINANCE_API_KEY /
# BINANCE_SECRET_KEY + a typed "yes" (or --yes) to place real orders.
.\bin\trading-agent.exe trade --symbol BTCUSDT --futures --leverage 5
.\bin\trading-agent.exe trade --symbol BTCUSDT --futures --leverage 5 --execute

# machine-readable output, and a saved config for reproducibility
.\bin\trading-agent.exe backtest --json
.\bin\trading-agent.exe backtest --save-config runs\btc.yaml
```

The `trade` command is the continuous loop: one decision per poll (protective
exits, mark-to-market, then act on the last closed signal), persisted to a state
file so a restart resumes the open position, peak equity and risk-manager state
instead of re-entering. `--cycles N` runs a bounded number of passes then
exits; `--state PATH` picks the persistence file.

## Configuration

`config.yaml` holds every setting; flags override it. Run with `-h` for the full
flag list.

```yaml
strategy:
  name: ma_cross
  params:
    fast: 10
    slow: 30

risk:
  initial_cash: 100000
  max_position_pct: 0.95
  max_risk_per_trade_pct: 0.02
  stop_loss_pct: 0.06
  take_profit_pct: 0.18
  max_drawdown_pct: 0.25
  max_daily_loss_pct: 0.05
  allow_short: false       # enable to trade the strategy's short signals
  leverage: 1             # >1 only applies on a futures venue

live:
  poll_seconds: 60
  lookback_days: 400
  paper_trading: true
  state_file: trade-state.json
  futures: false          # true = USDT-margined perpetuals (fapi.binance.com)
  margin_mode: ISOLATED   # or CROSS
```

Sizing is leverage-aware: the notional a position may control is the smallest
of `equity × max_position_pct × leverage`, the fixed-fractional risk cap
(`equity × max_risk_per_trade_pct` divided by the stop distance), and the
margin the balance can post (`cash × leverage`). With `leverage: 1` the formula
collapses to the historical spot behaviour. The per-trade risk *dollars* do
not grow with leverage — only the notional does.

## Project layout

```
cmd/trading-agent/     main entry point
internal/
  model/               Bar and Series types
  indicators/          SMA, EMA, RSI, ATR, rolling max/min, shift
  strategy/            ma_cross, rsi_reversion, breakout, llm
  risk/                sizing, stops, kill switches
  broker/              paper broker + live Binance spot & futures clients
  portfolio/           cash, positions, realized PnL, equity curve
  engine/              the event loop (backtest + live step)
  metrics/             Sharpe, Sortino, Calmar, drawdown, win rate, profit factor
  marketdata/          Binance spot and futures klines clients
  state/               JSON session persistence (atomic write)
  live/                the live session runner: broker wiring, reconciliation
  report/              CSV / JSON / Markdown / HTML writers
  webui/               HTTP server, JSON API and the embedded dashboard
  cli/                 command line interface
  llm/                 OpenAI-compatible client: strategy, veto, review, tune
  testfx/              deterministic offline bars for the unit tests only
```

## Adding your own strategy

Implement the `strategy.Strategy` interface and register it in the `New`
switch:

```go
type MyStrategy struct{}

func (MyStrategy) Name() string     { return "my_strategy" }
func (MyStrategy) Describe() string { return "my_strategy" }

func (MyStrategy) Generate(series model.Series, cfg config.Config) (strategy.Signals, error) {
	close := series.Close()
	mean := indicators.SMA(close, 50)
	signal := make([]float64, len(close))
	for i := range close {
		if model.IsNaN(mean[i]) {
			signal[i] = 0
		} else if close[i] > mean[i] {
			signal[i] = 1
		} else {
			signal[i] = 0
		}
	}
	return strategy.Signals{Signal: signal}, nil
}
```

A signal is a *target position*: `1` long, `0` flat, `-1` short. The engine
handles sizing, orders and stops; `Signals.Normalize` fills in any level you
leave unset.

## Notes on realism

Backtests here are honest but simplified: daily bars, market orders at the next
open, fixed bps costs, no partial fills, no borrow costs for shorts, and stops
assumed to fill at the stop price (real gaps can be much worse). Treat results
as a sanity check on an idea, not a forecast.

Annualized figures use 365 bars a year (Binance), since crypto trades 24/7.
`config.BarsPerYear()` reads that default; set `data.bars_per_year` to override
it. Because the market runs every day, a "daily loss limit" and a fixed stop
are tested against continuous price action - and overnight gaps through a stop
are the normal failure mode, exactly as in live crypto.

## Offline tests

The unit tests never touch the network: `internal/testfx` builds deterministic
daily bars for a given `(symbol, days, seed)`, and the engine/report/webui
suites feed it in. A change to any indicator, risk rule or accounting path
shows up as a different trade count or return, so the suites stay fast and
reproducible while the CLI and dashboard keep talking to real Binance.

## Going live

The `trade` command is the live loop, already wired to real Binance:

- **Venue.** `--futures` switches the runner to USDT-margined perpetuals
  (`fapi.binance.com`): signed positions, so the strategy's short signals are
  tradable, and leverage via `--leverage` / `risk.leverage`. Without it the
  loop trades spot. `risk.allow_short` keeps the short side opt-in even on a
  futures venue.
- **Paper by default.** No `--execute` means fills are simulated locally
  (`DryRun`), so you can run the loop with real market data and zero financial
  risk. `--execute` places real orders and reads the API key/secret from the
  `BINANCE_API_KEY` / `BINANCE_SECRET_KEY` environment variables only.
- **Safe by default.** `--execute` requires a typed `yes` on stdin unless
  `--yes` is given. On startup the runner reconciles the local book against
  the exchange (balances for spot, the signed position for futures) and
  refuses to continue if the two disagree. Client order ids are idempotent.
- **Restart-safe.** Each cycle persists the open position, equity
  high-water mark and risk-manager state to `--state`. A restart resumes
  rather than re-entering.

Before real money: run the paper loop against a broker testnet or demo
account for a meaningful period, size `max_risk_per_trade_pct` for the
leverage you actually intend to use, and confirm the venue's rules for your
account (leverage caps, isolation, taxes, regional availability).

## AI features

Four model-driven features sit on top of the same agent. They all share one
`llm` client — a thin OpenAI-compatible REST wrapper on `net/http`, so the
project still has a single non-stdlib dependency — and they all degrade
gracefully when the model is unavailable. The API key is read from the
environment only (`LLM_API_KEY`, falling back to `OPENAI_API_KEY`); it is never
stored in the config file, so a config can never leak a secret. Any
OpenAI-compatible endpoint works: set `llm.base_url` and `llm.model`.

| Feature | Where | Flag / config | Without a key |
| --- | --- | --- | --- |
| LLM strategy | `--strategy llm` | `llm.*` | backtest still runs; the strategy emits an all-flat (hold) curve |
| LLM entry veto | `trade --veto` | `live.veto_enabled` | fail-open: entries pass, protective stops are never blocked |
| LLM post-mortem | `backtest --review`, `trade --review` | `llm.*` | the report notes "LLM review unavailable" |
| LLM tuning loop | `tune` | `llm.*` | runs the baseline, skips proposal rounds, saves the baseline |

The veto is **fail-open on purpose**: a disabled client, a transport error or an
unparseable answer all let the trade through, so a down model can never
silently block a stop-loss or risk-halt flatten. Only an explicit model
refusal (`approve: false`) blocks, and it blocks a *new entry* — protective
exits and the drawdown kill switch are never gated.

The `tune` loop is the proposer: each round the model suggests a new parameter
set for the strategy, the loop backtests it on the same series and keeps the
winner against the objective (`--objective sharpe|sortino|total_return|
profit_factor|win_rate`). `--save-best PATH` writes the winning parameters into
a reproducible config.

```powershell
# strategy that lets the model choose target positions
.\bin\trading-agent.exe backtest --strategy llm --symbol BTCUSDT

# gate new live entries with a model second opinion (fail-open)
.\bin\trading-agent.exe trade --symbol BTCUSDT --strategy rsi_reversion --veto

# post-mortem review appended to the run's report
.\bin\trading-agent.exe backtest --symbol BTCUSDT --review

# tuning loop: 4 proposal rounds, keep the best Sharpe, save it
.\bin\trading-agent.exe tune --strategy rsi_reversion --objective sharpe --rounds 4 --save-best runs\best.yaml
```

## License

MIT
