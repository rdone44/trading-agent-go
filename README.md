# trading-agent (Go)

A small, readable trading agent in Go: load market data, run a strategy, size
every trade through a risk manager, execute on a paper broker, and write a
report you can open in the browser.

```
data -> strategy -> risk -> paper broker -> portfolio -> metrics -> report
```

**This is research/education code, not investment advice.** It sends no real
orders.

## Features

- Three built-in strategies: `ma_cross`, `rsi_reversion`, `breakout`
- Risk layer: position sizing, ATR/% stops, take-profit, max drawdown kill switch, daily loss limit
- Data source: Binance spot (public REST API, daily bars)
- Realistic frictions: commission, slippage, lot rounding, minimum notional
- Look-ahead safe: signals decided on the close, executed on the next open
- Reports: HTML (with an inline SVG equity curve), Markdown, CSV and JSON
- Only one dependency (`yaml.v3`); the Binance client uses `net/http`

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

# machine-readable output, and a saved config for reproducibility
.\bin\trading-agent.exe backtest --json
.\bin\trading-agent.exe backtest --save-config runs\btc.yaml
```

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
```

## Project layout

```
cmd/trading-agent/     main entry point
internal/
  model/               Bar and Series types
  indicators/          SMA, EMA, RSI, ATR, rolling max/min, shift
  strategy/            ma_cross, rsi_reversion, breakout
  risk/                sizing, stops, kill switches
  broker/              paper broker: slippage, commission, lot rounding
  portfolio/           cash, positions, realized PnL, equity curve
  engine/              the event loop
  metrics/             Sharpe, Sortino, Calmar, drawdown, win rate, profit factor
  marketdata/          Binance spot klines client
  report/              CSV / JSON / Markdown / HTML writers
  webui/               HTTP server, JSON API and the embedded dashboard
  cli/                 command line interface
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

`live` re-simulates recent bars and reports the orders it *would* send. To trade
for real you need a broker adapter and, realistically, more work:

1. Implement a broker client (Alpaca, IBKR, Binance, ...) with the same shape as
   `broker.PaperBroker.MarketOrder`, then swap it into `engine.Agent`.
2. Keep `risk.Manager` in front of every order - it is the only thing standing
   between a bug and your capital.
3. Add state persistence (positions must survive a restart), reconciliation
   against the broker, idempotent order IDs, and alerting.
4. Test on a broker paper account for weeks before considering real money.
5. Check the rules that apply to you: PDT in the US, leverage limits, taxes,
   market data licensing.

## License

MIT
