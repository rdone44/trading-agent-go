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

On a Linux host the equivalent is the `Makefile` (no PowerShell needed):

```bash
make build                  # compile every command
make server                 # server edition into ./dist/trading-agent-server
make test                   # go test ./...
make e2e                    # P2-2 smoke: build + offline end-to-end test + full suite
```

`make e2e` boots the server edition's HTTP stack on a real listener with
offline (stub) market data, drives it with a real HTTP client through
`/healthz` → `/api/config` → a backtest, asserts every step is 200, then runs
the full test suite. No real network, no LLM key, no live keys.

| | Windows desktop | Linux server |
| --- | --- | --- |
| Binary | `trading-agent-desktop-windows-amd64.exe` | `trading-agent-server-linux-amd64` (also arm64) |
| Entry point | `cmd/trading-agent-desktop` | `cmd/trading-agent-server` |
| Start it by | double-clicking the .exe | `systemctl start trading-agent` |
| Binds | `127.0.0.1:8765` (falls back to a free port if taken) | `0.0.0.0:8080` by default |
| Auth | none - loopback only | **token required** |
| Browser | opens automatically | you open the printed URL |
| Console | none (`-H=windowsgui`) | stdout / journal |
| Stops via | 退出 button in the UI, or the taskbar | `SIGTERM` from systemd |
| Logs | `%APPDATA%\trading-agent\desktop.log` | `journalctl -u trading-agent` |
| Reports | `dist\reports\` next to the exe | `/var/lib/trading-agent/reports` |

### Windows desktop

Double-click the exe. It binds `127.0.0.1:8765` - a stable port, so the console
URL and any bookmark survive a restart - opens the dashboard in the default
browser, and writes reports and `trade-state.json` next to itself. There is no
console window and no flag to pass; the 退出 button in the page stops the server
and exits.

If the port is already taken by something else, it falls back to an
OS-assigned one. If *this app* is already running on it, a second double-click
just reopens the live console instead of starting a rival server that would
trade the same account behind the first one's back.

If it fails to start, a dialog box appears with the reason - the same text is
appended to `%APPDATA%\trading-agent\desktop.log`.

**Connection settings.** The desktop build is single-user: there is no
administrator and no login, so the settings panel (**设置 → Binance 与 AI 服务**)
writes your Binance keys and your model URL/token straight to
`%APPDATA%\trading-agent\credentials.json` (mode 0600 on Unix; on Windows the
file sits under your per-user `%APPDATA%`, outside the repository, so it can
never be committed). Nothing is uploaded and no account is needed. The same
panel has the **允许本机实盘下单** switch, the desktop equivalent of
`TA_ALLOW_LIVE=1`; it is off by default and the typed `确认实盘` phrase is still
required on top of it. The environment variables below remain the fallback for
CLI and scripted use, and an explicitly exported `TA_ALLOW_LIVE=0` still
refuses real orders no matter what the switch says.

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

The console is a Chinese trading workbench embedded in the Go binary. It has
no framework, CDN or Node/Python runtime dependency.

- Binance candlesticks are the primary view, with 15m / 1h / 4h / 1d chart
  intervals and labelled 24-hour price changes. Chart intervals do not change
  the agent's daily-bar decision timeframe.
- Account, open position, stop distance and risk budgets stay beside the chart
  on desktop. Mobile prioritizes price, position and risk before the ledger.
- Configuration is an on-demand drawer. A refreshed page loads the running
  session's effective parameters, not the server defaults.
- Order/fill, decision log and closed-trade tabs distinguish actual fills,
  partial fills, rejections and orders whose exchange state is uncertain.
- Market data works without starting a trading session or configuring keys.
  It refreshes every 15 seconds, with a short server-side cache. This is REST
  polling, not a websocket or tick-by-tick terminal.

Paper mode is the default. Live orders require the explicit checkbox, the
phrase `确认实盘`, a Binance key pair, and the process-level live gate
(`TA_ALLOW_LIVE=1`, or the desktop's 允许本机实盘下单 switch). Keys come from
the settings panel on the desktop, from a per-user vault when the server is
started with `-users`, or from `BINANCE_API_KEY` / `BINANCE_SECRET_KEY` in the
environment. Real capital is read from the exchange during startup; the
initial-cash input is for paper sessions. Use a dedicated account/strategy
balance: the local ledger is not an exchange-wide account reconciliation
service.

**Stopping is not liquidation.** Stops and targets are evaluated locally at
each poll, not submitted as exchange-native protective orders. Stopping,
quitting or losing connectivity leaves positions exposed. The interface
asks for confirmation when stopping with a position and displays a persistent
warning afterwards. Exchange-native protection, automatic account-wide
reconciliation and funding/liquidation simulation are not implemented.

The trade path checks protective exits before loading history or asking a
strategy. A failed strategy cannot skip an existing stop. A protective close
does not reopen in the same cycle. Futures use a separate margin ledger:
wallet plus unrealized PnL, with entry margin reserved rather than debiting
full notional. The live futures broker currently supports one-way positions
only; Hedge Mode is rejected.

Session files are partitioned by paper/live, spot/futures and symbol:
`sessions/paper-spot-BTCUSDT.json`, for example. Legacy `trade-state.json`
files are not silently imported or overwritten. Back them up and reconcile
positions before migrating. An explicit `state_path` must match the saved
symbol and mode. Old futures accounting files require manual reconciliation.

Live orders write a durable `*.order-pending.json` intent before submission.
A timeout looks up the same client ID rather than blindly submitting another
order. Uncertain exchange results or failed ledger persistence halt automated
orders. The pending intent remains until a successfully saved, confirmed
ledger clears it; after a crash, startup refuses to trade until an operator
has checked the exchange and repaired the ledger. Do not simply delete a
pending intent to get past this guard.

The HTTP API behind it is small enough to script against:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/config` | effective config plus strategy metadata |
| `GET` | `/api/session` | effective session settings, equity, position, risk, fills and logs |
| `GET` | `/api/market?symbol=BTCUSDT&venue=spot&interval=1h` | public Binance quote and candles, independent of trading |
| `POST` | `/api/session/start` | start the trading loop (`interval_seconds`, `execute`, `confirm`, `futures`, `leverage`, `state_path`, `veto`) |
| `POST` | `/api/session/stop` | stop the loop and persist state |
| `POST` | `/api/session/step` | run exactly one cycle now |
| `POST` | `/api/session/review` | LLM post-mortem of the fills this session actually made |
| `POST` | `/api/backtest` | run a backtest, persist it, return metrics and curves (add `"review": true` for an LLM post-mortem) |
| `POST` | `/api/tune` | run the LLM parameter-tuning loop, return the round-by-round report |
| `POST` | `/api/tune-prompt` | run the prompt-iteration loop, return the winning persona |
| `GET` | `/api/runs` | list saved runs, newest first |
| `GET` | `/api/run?name=...` | reopen a saved run's curves, trades and orders |
| `GET` | `/runs/<name>/report.html` | the static report of a saved run |
| `GET` | `/healthz` | liveness probe, `{"status":"ok"}` |
| `POST` | `/api/shutdown` | stop the process - **desktop build only** |

`/api/shutdown` is registered only when the desktop shell supplies a cancel
function, so a dashboard reachable over a network can never be stopped by an
HTTP request.

The LLM features are reachable from the CLI, from the JSON API, and from the
console's **AI** page (`#/ai`).

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
  `BINANCE_API_KEY` / `BINANCE_SECRET_KEY` environment variables (the CLI has
  no settings panel; the desktop edition stores the same pair in its
  credentials file).
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
gracefully when the model is unavailable. The API key is never stored in the
config file, so a config can never leak a secret:

- **Desktop edition** — open **设置 → Binance 与 AI 服务** and fill in
  **AI 服务 URL** (`https://api.openai.com/v1`), **AI 模型名** (`gpt-4o-mini`)
  and **AI Token**. That is all the AI page needs; the values are saved to the
  local credentials file described above.
- **Server / CLI** — export `LLM_API_KEY` (falling back to `OPENAI_API_KEY`),
  or set `llm.base_url` / `llm.model` in the YAML config for a non-OpenAI
  endpoint.

Any OpenAI-compatible endpoint works. The **AI** page states whether a model is
ready, and when none is configured it says exactly which panel to open.

| Feature | CLI | Web dashboard | Flag / config | Without a key |
| --- | --- | --- | --- | --- |
| LLM strategy | `--strategy llm` | **AI** page → 使用 AI 策略 | `llm.*` | backtest still runs; the strategy emits an all-flat (hold) curve |
| LLM entry veto | `trade --veto` | **AI** page → 入场否决 | `live.veto_enabled` | fail-open: entries pass, protective stops are never blocked |
| LLM post-mortem | `backtest --review`, `trade --review` | **AI** page → 回测复盘 / 实盘复盘 | `llm.*` | the page notes "模型不可用" instead of failing |
| LLM tuning loop | `tune` | **AI** page → 调优策略参数 | `llm.*` | runs the baseline, skips proposal rounds, saves the baseline |
| LLM prompt tuning | `tune-prompt` | **AI** page → 迭代交易人设 | `llm.prompt` | runs the baseline, keeps the current persona |

The dashboard gives the model a page of its own (`#/ai`): 决策, 否决, 复盘 and
自我迭代 in one place, so none of it hides behind a strategy dropdown. Every
control there describes the *next* session; a running loop is never rewritten.
Live post-mortems (`POST /api/session/review`) review the fills the session
actually made, rather than a hypothetical backtest.

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
