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

## Two editions

The dashboard ships in two builds from the same source. Pick the one that
matches where it runs; `build.ps1` produces both.

```powershell
.\build.ps1                 # both editions into dist\
.\build.ps1 -Only desktop   # Windows only
.\build.ps1 -Only server    # Linux only
```

| | Windows desktop | Linux server |
| --- | --- | --- |
| Binary | `trading-agent-desktop-windows-amd64.exe` | `trading-agent-server-linux-amd64` (also arm64) |
| Entry point | `cmd/trading-agent-desktop` | `cmd/trading-agent-server` |
| Start it by | double-clicking the .exe | `systemctl start trading-agent` |
| Binds | `127.0.0.1` on a free port | `0.0.0.0:8080` by default |
| Auth | none - loopback only | **token required** |
| Browser | opens automatically | you open the printed URL |
| Console | none (`-H=windowsgui`) | stdout / journal |
| Stops via | 退出 button in the UI, or the taskbar | `SIGTERM` from systemd |
| Logs | `%APPDATA%\trading-agent\desktop.log` | `journalctl -u trading-agent` |
| Reports | `dist\reports\` next to the exe | `/var/lib/trading-agent/reports` |

### Windows desktop

Double-click the exe. It picks a free loopback port, opens the dashboard in the
default browser, and writes reports next to itself. There is no console window
and no flag to pass; the 退出 button in the page stops the server and exits.

If it fails to start, a dialog box appears with the reason - the same text is
appended to `%APPDATA%\trading-agent\desktop.log`.

### Linux server

The server edition binds a network interface, so it **refuses to start without
an access token**. The token can come from `TA_TOKEN`, from a file
(`--token-file`, generated and saved on first run), or you can generate one
yourself with `--print-token`. Serving without auth needs the explicit
`--allow-anonymous` opt-in.

```bash
./trading-agent-server --addr 0.0.0.0:8080 --token-file /var/lib/trading-agent/token

# prints the token and the URL to open, e.g.
#   ready  http://0.0.0.0:8080
#   open   http://localhost:8080/?token=4e55d4d5…
```

Opening the `?token=` link once sets an HttpOnly cookie and redirects to a
clean URL, so the token does not stay in the address bar. Scripts can use the
header instead:

```bash
curl -H "Authorization: Bearer $TA_TOKEN" http://host:8080/api/config
```

`GET /healthz` answers `{"status":"ok"}` for a systemd watchdog, a container
probe or a load balancer. It is behind the same token as everything else.

Deployment files live in `deploy/`:

- `trading-agent.service` - hardened systemd unit (no privileges, read-only
  root filesystem, writes only its own state directory)
- `Dockerfile` - multi-stage build onto a small Alpine runtime

## Trading console

```powershell
.\bin\trading-agent.exe web                    # opens http://localhost:8080
.\bin\trading-agent.exe web --addr :9000       # another port
.\bin\trading-agent.exe web --no-open          # do not launch a browser
```

The console is a single embedded page (Chinese UI) - no CDN, no JavaScript
framework, no build step - so it works offline and ships inside the binary. It
drives the **live trading loop**, not a backtest. The layout is a market
terminal: a slim top bar carries the session state and the start/stop controls,
the workspace is given over to the live readout, and the configuration lives in
a slide-in drawer because it is fixed once a session starts.

- **Config drawer** for instrument, poll interval, venue (spot or USDT-perp), strategy parameters, risk limits and costs
- **Hero readout** with marked equity, total return against initial cash, and an inline equity sparkline
- **Stat strip** for cash, peak equity, last price, cycle count and the latest action
- **Open position panel** with direction, size, average entry, stop and take-profit levels, and unrealized PnL
- **Tabs** for the per-cycle run log and the closed-trade ledger
- **Start / stop / step** controls, with a pulsing indicator that answers "is it alive?" at a glance

The palette is deliberately narrow: a deep neutral ground, amber for anything
the eye must catch, green/red strictly for profit and loss, and one saturated
red surface reserved for the live-money badge. Numbers use tabular figures so
columns do not jitter as they tick.

The session is **paper by default**: fills are simulated locally and no order
leaves the machine. Real orders need the 实盘下单 checkbox *and* the confirmation
phrase `确认实盘` typed in the box that appears, *and* `BINANCE_API_KEY` /
`BINANCE_SECRET_KEY` in the environment. Missing any of the three fails the
start with a message instead of trading.

Session state is written to disk, so stopping and restarting resumes the same
position, peak equity and risk-manager state rather than re-entering. The UI
polls the session endpoint every few seconds while a session runs.

The HTTP API behind it is small enough to script against:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/config` | effective config plus strategy metadata |
| `GET` | `/api/session` | current session status: equity, position, cycle log, trades |
| `POST` | `/api/session/start` | start the trading loop (`interval_seconds`, `execute`, `confirm`, `futures`, `leverage`, `state_path`) |
| `POST` | `/api/session/stop` | stop the loop and persist state |
| `POST` | `/api/session/step` | run exactly one cycle now |
| `POST` | `/api/backtest` | run a backtest, persist it, return metrics and curves (add `"review": true` for an LLM post-mortem) |
| `POST` | `/api/tune` | run the LLM parameter-tuning loop, return the round-by-round report |
| `GET` | `/api/runs` | list saved runs, newest first |
| `GET` | `/api/run?name=...` | reopen a saved run's curves, trades and orders |
| `GET` | `/runs/<name>/report.html` | the static report of a saved run |
| `GET` | `/healthz` | liveness probe, `{"status":"ok"}` |
| `POST` | `/api/shutdown` | stop the process - **desktop build only** |

`/api/shutdown` is registered only when the desktop shell supplies a cancel
function, so a dashboard reachable over a network can never be stopped by an
HTTP request.

The LLM features stay available from the CLI and from the JSON API; the console
itself is focused on trading. `POST /api/backtest` still accepts `"review": true`
for a post-mortem, and `POST /api/tune` still runs the tuning loop.

```powershell
# paper session: simulate fills, no orders leave the machine
curl.exe -X POST http://localhost:8080/api/session/start `
  -H "Content-Type: application/json" `
  -d '{\"symbol\":\"BTCUSDT\",\"strategy\":\"ma_cross\",\"days\":730,\"interval_seconds\":60}'

# stop it, then inspect what happened
curl.exe -X POST http://localhost:8080/api/session/stop
curl.exe http://localhost:8080/api/session
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
cmd/trading-agent/           CLI entry point
cmd/trading-agent-desktop/   Windows desktop entry point (GUI, no console)
cmd/trading-agent-server/    Linux server entry point (headless, token auth)
deploy/                      systemd unit and Dockerfile
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
  webui/               HTTP server, JSON API, token auth and the embedded dashboard
  tune/                the LLM parameter-tuning loop (shared by CLI and web)
  cli/                 command line interface
  llm/                 OpenAI-compatible client: strategy, veto, review, tune
  shell/               OS helpers: open a browser, pick a free port, error dialog
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

| Feature | CLI | Web dashboard | Flag / config | Without a key |
| --- | --- | --- | --- | --- |
| LLM strategy | `--strategy llm` | strategy picker | `llm.*` | backtest still runs; the strategy emits an all-flat (hold) curve |
| LLM entry veto | `trade --veto` | — (live only) | `live.veto_enabled` | fail-open: entries pass, protective stops are never blocked |
| LLM post-mortem | `backtest --review`, `trade --review` | **LLM 复盘** checkbox on `/api/backtest` | `llm.*` | the report/UI notes "LLM review unavailable" |
| LLM tuning loop | `tune` | **LLM 调参** button → `/api/tune` | `llm.*` | runs the baseline, skips proposal rounds, saves the baseline |

The tuning loop and the post-mortem are single reusable implementations
(`internal/tune` and `llm.Review`): the CLI command and the web endpoint run
the exact same code, so both surfaces clamp proposals into the strategy's
legal parameter ranges, early-stop on the stall guard, and degrade
identically when the model is absent.

In the live loop the LLM strategy takes a single-decision path
(`LastDecision`): one model call per poll on the most recent bar, not a full
per-bar regeneration across the lookback window. A 400-day lookback therefore
costs one call a poll instead of ~400. Backtests keep the per-bar signal
(`Generate`), which genuinely needs every bar; raise `llm_step` there to cap
the model calls.

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
