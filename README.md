# trading-agent (Go)

一个小而可读的 Go 交易 Agent：拉取行情、跑策略、让风控给每一笔交易定仓位、
在纸面或实盘经纪上执行，最后写出一份能在浏览器里打开的报告。

```
data -> strategy -> risk -> broker -> portfolio -> metrics -> report
```

唯一的执行场所是 **Binance USDT 本位永续合约**（可做多做空、可加杠杆）。
实盘循环默认纸面模拟，只有显式传入 `--execute` 并输入确认短语才会真正下单。

现货场所已整体退役：每一个会话、回测和图表都只说永续市场这一种语言。旧的现货
状态文件在加载时会被直接拒绝，而不是自动转换——请先手动核对那些仓位再迁移。

**这是研究与教学代码，不构成投资建议。** 永续与杠杆的亏损可能超过保证金，
请以这样的心态对待每一次实盘运行。

---

## 目录

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [两个版本：桌面与服务器](#两个版本桌面与服务器)
- [交易控制台](#交易控制台)
- [命令行](#命令行)
- [配置](#配置)
- [实盘安全模型](#实盘安全模型)
- [交易所侧保护单与对账](#交易所侧保护单与对账)
- [崩溃锁与人工恢复](#崩溃锁与人工恢复)
- [状态文件与决策日志](#状态文件与决策日志)
- [HTTP API](#http-api)
- [AI 功能](#ai-功能)
- [回测的真实性与局限](#回测的真实性与局限)
- [离线测试与 CI](#离线测试与-ci)
- [部署](#部署)
- [项目结构](#项目结构)
- [自定义策略](#自定义策略)
- [其他文档](#其他文档)
- [许可证](#许可证)

---

## 功能特性

- **四个内置策略**：`ma_cross`（双均线交叉）、`rsi_reversion`（RSI 均值回归）、
  `breakout`（唐奇安通道突破），以及 `llm`（由模型给出目标仓位）
- **风控层**：杠杆感知的仓位计算、ATR / 百分比止损、止盈、最大回撤熔断、当日亏损上限
- **数据源**：Binance USDT 永续公开 REST 接口（`fapi.binance.com`），决策用日线
- **两种经纪**：纸面经纪（本地模拟成交）与 Binance 永续实盘经纪，共用一个 `Broker` 接口
- **真实摩擦**：手续费、滑点、手数取整、最小名义金额
- **无未来函数**：收盘决定信号，下一根开盘执行
- **实盘循环**：状态持久化、重启对账、幂等的客户端订单 ID、交易所时钟校正签名
- **交易所侧保护单**：通过 Binance Algo Order 接口挂 `STOP_MARKET` / `TAKE_PROFIT_MARKET`，
  每个周期逐腿核验，缺失补挂，持仓时遇冲突即停，空仓时自动撤掉自己名下的残留腿
- **AI 功能**：LLM 策略、入场否决、事后复盘、参数调优、交易人设自迭代，全部走同一个
  OpenAI 兼容客户端，模型不可用时全部优雅降级
- **多用户账号**（服务器版可选）：登录 / 注册、每个用户自己的交易所与模型密钥、
  每个账号独立的交易会话与账本
- **报告**：HTML（内联 SVG 资金曲线）、Markdown、CSV、JSON
- **唯一第三方依赖** `gopkg.in/yaml.v3`；Binance 客户端与 LLM 客户端只用 `net/http`

---

## 快速开始

需要 Go 1.23 或更高（`go.mod` 声明 `go 1.23`，CI 固定在 1.23.4）。

```powershell
cd E:\agent\trading-agent-go

go test ./...                                   # 全离线的单元 + 集成测试
go build -o bin/trading-agent.exe ./cmd/trading-agent

.\bin\trading-agent.exe strategies              # 列出内置策略
.\bin\trading-agent.exe web                     # 控制台：http://localhost:8080
.\bin\trading-agent.exe backtest --symbol BTCUSDT --days 730
```

Linux 上等价的入口是 `Makefile`：

```bash
make build      # 编译全部命令（CLI + 桌面版 + 服务器版）
make server     # 服务器版编译到 ./dist/trading-agent-server
make test       # go test ./...
make e2e        # 编译服务器版 + 离线端到端冒烟 + 完整测试套件
make clean      # 删除 dist/
```

`make e2e` 会在真实监听端口上启动服务器版的 HTTP 栈，用桩行情数据、真实 HTTP
客户端依次打 `/healthz` → `/api/config` → 一次回测，断言每一步都是 200，然后跑
完整测试套件。全程不碰真实网络，不需要 LLM 密钥，也不需要实盘密钥。

---
## 两个版本：桌面与服务器

同一份源码出两个控制台构建，按运行环境二选一。`build.ps1` 一次产出两者：

```powershell
.\build.ps1                 # 两个版本都编到 dist\
.\build.ps1 -Only desktop   # 仅 Windows 桌面版
.\build.ps1 -Only server    # 仅 Linux 服务器版（amd64 + arm64）
```

| | Windows 桌面版 | Linux 服务器版 |
| --- | --- | --- |
| 二进制 | `trading-agent-desktop-windows-amd64.exe` | `trading-agent-server-linux-amd64`（另有 arm64） |
| 入口 | `cmd/trading-agent-desktop` | `cmd/trading-agent-server` |
| 启动方式 | 双击 exe | `systemctl start trading-agent` |
| 监听 | `127.0.0.1:8765`（被占用则退回系统分配的空闲端口） | 默认 `0.0.0.0:8080` |
| 鉴权 | 无——只绑定回环地址 | **必须有访问令牌**（或启用账号登录） |
| 浏览器 | 自动打开 | 自行打开日志里打印的 URL |
| 控制台窗口 | 无（`-H=windowsgui`） | stdout / journal |
| 停止方式 | 页面上的「退出」按钮，或任务栏 | systemd 发送 `SIGTERM` |
| 日志 | `%APPDATA%\trading-agent\desktop.log` | `journalctl -u trading-agent` |
| 报告 | exe 旁边的 `reports\` | `/var/lib/trading-agent/reports` |

### Windows 桌面版

双击 exe 即可。它优先绑定 `127.0.0.1:8765`——固定端口意味着书签和控制台 URL
在重启后依然有效——然后用默认浏览器打开面板，报告和 `trade-state.json` 都写在
exe 旁边。没有控制台窗口，也不需要任何参数；页面上的「退出」按钮会停掉服务并退出进程。

端口若被别的程序占用，会退回系统分配的空闲端口。若占用者正是本程序，再次双击只会
重新打开已在运行的控制台，而不是再起一个服务器在背后操作同一个账户。

启动失败时会弹出对话框说明原因，同样的文字也会追加到
`%APPDATA%\trading-agent\desktop.log`。

配置文件查找顺序：exe 旁边的 `config.yaml` → 当前目录的 `config.yaml` → 内置默认值。

**连接设置。** 桌面版是单用户的：没有管理员，也没有登录。设置面板
（**设置 → Binance 与 AI 服务**）把你的 Binance 密钥与模型 URL / Token 直接写进
`%APPDATA%\trading-agent\credentials.json`（Unix 上是 `~/.config/trading-agent/credentials.json`，
权限 0600）。文件在仓库之外，永远不会被误提交；什么都不会上传，也不需要注册账号。
同一个面板里有 **允许本机实盘下单** 开关，相当于桌面版的 `TA_ALLOW_LIVE=1`：默认关闭，
而且即使打开，页面上仍然必须输入「确认实盘」。下文的环境变量仍然是 CLI 与脚本的
兜底方式；若显式导出了 `TA_ALLOW_LIVE=0`，无论开关怎么设都拒绝真实下单。

### Linux 服务器版

服务器版绑定网络接口，因此 **没有访问令牌就拒绝启动**。令牌可以来自 `TA_TOKEN`、
来自文件（`--token-file`，首次运行自动生成并保存，权限 0600），或者用 `--print-token`
自己生成一个。无鉴权暴露需要显式的 `--allow-anonymous`。

```bash
./trading-agent-server --addr 0.0.0.0:8080 --token-file /var/lib/trading-agent/token

# 日志会打印令牌和可直接打开的 URL，例如
#   ready  http://0.0.0.0:8080
#   open   http://localhost:8080/?token=4e55d4d5…
```

带 `?token=` 打开一次后，服务会设置一个 HttpOnly cookie（`ta_token`）并重定向到干净的
URL，令牌不会留在地址栏里。脚本可以改用请求头：

```bash
curl -H "Authorization: Bearer $TA_TOKEN" http://host:8080/api/config
```

令牌中间件包裹整个路由表，**包括 `/healthz`**：systemd watchdog、容器探针或负载均衡器
也要带令牌（`deploy/Dockerfile` 里的 `HEALTHCHECK` 就是这么做的）。`GET /healthz` 返回
`{"status":"ok","time":"…"}`，不碰磁盘也不碰网络。

服务器版的全部参数（每个都有对应的环境变量）：

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `--addr` | `TA_ADDR` | `0.0.0.0:8080` | 监听地址 |
| `--config` | `TA_CONFIG` | `/etc/trading-agent/config.yaml`（存在时），否则二进制旁边的 `config.yaml` | YAML 配置 |
| `--output` | `TA_OUTPUT` | 配置里的 `backtest.output_dir` | 报告目录 |
| `--token` | `TA_TOKEN` | 空 | 访问令牌 |
| `--token-file` | `TA_TOKEN_FILE` | 空 | 从文件读令牌；不存在则生成并写入 |
| `--allow-anonymous` | `TA_ALLOW_ANONYMOUS=1` | 关 | 无令牌提供服务（只在可信代理后面才安全） |
| `--users` | `TA_USERS` | 空 | 启用账号登录：用户凭据保险库文件的路径 |
| `--print-token` | — | — | 生成一个令牌、打印、退出 |

#### 多用户账号模式（`--users`）

给服务器版传 `--users /path/to/users.json` 就开启账号登录：页面上会出现「账号登录」卡片，
用户自己注册、登录，并在设置面板里保存**自己的** Binance 密钥和模型 URL / Token。

- 保险库是一个 0600 权限的 JSON 文件，没有数据库。密码用 PBKDF2-HMAC-SHA256
  （250,000 轮）哈希，会话是 HMAC 签名的 cookie（`ta_session`，HttpOnly，SameSite=Lax，24 小时）。
  全部由标准库实现，项目依然只有一个第三方依赖。
- **每个账号一套会话、一套账本**。启动、单步、停止、读取某个账号的会话都不会影响
  别人；账本里持久化了所有者，另一个账号永远无法接管这个仓位。保存的回测
  （`/api/runs`、`/runs/<name>/…`）同样按所有者隔离。
- **账号模式下永远不回退到部署者的环境变量密钥**。一个没有保存密钥的注册用户不可能
  用部署者的 `BINANCE_API_KEY` 开实盘，也不可能把 `LLM_API_KEY` 泄漏给自己指定的模型端点。
- 开了账号模式但没有显式传 `--token` / `--token-file` 时，令牌层自动关闭——否则会出现两套
  互相竞争的登录。行情、配置、策略列表与 `/healthz` 保持匿名可读，这样未登录的浏览器
  也能看到价格和登录卡片；其余 `/api/*` 都要求已登录。

部署文件在 `deploy/`：

- `trading-agent.service`：加固过的 systemd 单元（无特权、只读根文件系统、只能写自己的状态目录）
- `Dockerfile`：多阶段构建到一个很小的 Alpine 运行时

---

## 交易控制台

```powershell
.\bin\trading-agent.exe web                    # 打开 http://localhost:8080
.\bin\trading-agent.exe web --addr :9000       # 换端口
.\bin\trading-agent.exe web --no-open          # 不自动打开浏览器
```

控制台是嵌在 Go 二进制里的中文交易工作台：没有框架、没有 CDN、没有 Node / Python
运行时依赖，`internal/webui/static/` 下只有 `index.html`、`app.js`、`app.css` 三个文件。

顶部导航分三页：

| 页面 | 路由 | 内容 |
| --- | --- | --- |
| 交易总览 | `#/market` | Binance 永续 K 线为主视图，旁边是合约账户、持仓、止损距离、风控预算、订单 / 成交、决策日志与已平仓交易 |
| 策略与风控 | `#/strategies` | 策略选择、参数、风控设置，描述的是**下一次**会话 |
| AI 决策 | `#/ai` | 决策、否决、复盘、自我迭代集中在一页，并显示模型是否就绪 |

- 图表支持 15m / 1h / 4h / 1d 四个周期，并标注 24 小时涨跌。图表周期不改变 Agent 的
  日线决策周期。
- 行情不需要启动交易会话、也不需要配置密钥就能看。每 15 秒刷新一次，服务端带短暂缓存。
  这是 REST 轮询，不是 WebSocket，也不是逐笔终端。
- 币种选择器可以拉取该场所全部币种（按 24 小时成交额排序）。
- 配置是按需打开的侧栏抽屉。刷新页面加载的是正在运行会话的有效参数，不是服务器默认值。
- 订单 / 成交、决策日志和已平仓交易三个页签会区分真实成交、部分成交、拒单，以及
  交易所状态不确定的订单。
- 顶部的安全条始终显示当前模式（纸面 / 实盘）、会话状态，以及「立即执行」「停止策略」
  「人工核验恢复」等动作按钮。

纸面模式是默认值。实盘下单需要同时满足：显式勾选实盘、输入短语「确认实盘」、一对
Binance 密钥，以及进程级实盘闸门（`TA_ALLOW_LIVE=1`，或桌面版的「允许本机实盘下单」开关）。
密钥来源：桌面版的设置面板；服务器版用 `--users` 时来自每个用户自己的保险库；否则来自环境变量
`BINANCE_API_KEY` / `BINANCE_SECRET_KEY`。真实资金在启动时从交易所读取，「初始资金」输入
只对纸面会话有效。请用专门的账户 / 策略资金：本地账本不是交易所全账户的对账服务。

**停止不等于平仓。** 停止会保存账本，但既不平仓，也不撤掉交易所侧的保护单；同时也停止了
本地监控。离开之前请到 Binance 核对真实持仓和保护单——一份此前已核验的快照并不保证
订单现在仍然存在，也不保证触发时一定能成交。

---
## 命令行

```powershell
# 控制台
.\bin\trading-agent.exe web

# 用 Binance 永续 K 线回测（BTCUSDT、ETHUSDT……）
.\bin\trading-agent.exe backtest --symbol BTCUSDT --days 730

# 在同一份数据上比较策略
.\bin\trading-agent.exe backtest --strategy rsi_reversion --symbol ETHUSDT
.\bin\trading-agent.exe backtest --strategy breakout --symbol BTCUSDT

# 扫描多个币种并排名
.\bin\trading-agent.exe scan --symbols BTCUSDT,ETHUSDT,SOLUSDT

# 纸面循环（本地模拟，不会有订单离开你的机器）
.\bin\trading-agent.exe live --symbol BTCUSDT --iterations 3 --poll 10

# 持续交易循环（默认纸面；只有 --execute 才真下单）
.\bin\trading-agent.exe trade --symbol BTCUSDT --strategy rsi_reversion

# 永续 + 杠杆；--execute 需要 BINANCE_API_KEY / BINANCE_SECRET_KEY，
# 并在 stdin 输入 "yes"（或传 --yes）
.\bin\trading-agent.exe trade --symbol BTCUSDT --leverage 5
.\bin\trading-agent.exe trade --symbol BTCUSDT --leverage 5 --execute

# 机器可读输出，以及保存一份可复现的配置
.\bin\trading-agent.exe backtest --json
.\bin\trading-agent.exe backtest --save-config runs\btc.yaml
```

### 子命令

| 命令 | 作用 |
| --- | --- |
| `backtest` | 跑一次回测并写报告 |
| `scan` | 对多个币种回测并排名 |
| `live` | 对最新的 K 线做纸面交易（绝不真下单） |
| `trade` | 实盘交易循环（默认纸面；`--execute` 才下真单；`--leverage N` 设杠杆） |
| `tune` | LLM 参数调优循环（多轮「提议 + 回测」） |
| `tune-prompt` | LLM 人设迭代循环（模型改写 `llm` 策略的交易人设，回测当裁判） |
| `web` | 启动交互式控制台 |
| `strategies` | 列出内置策略 |
| `version` | 打印版本 |
| `help` / `-h` | 打印用法 |

未知命令退出码为 2。

### 通用参数

每个子命令都接受这些参数（未传的保持零值，配置文件里的值得以保留）：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--config` | 存在时用 `./config.yaml` | YAML 配置文件 |
| `--symbol` | 配置值 | Binance 交易对，如 `BTCUSDT` |
| `--days` | 配置值 | 历史天数（日历日） |
| `--strategy` | 配置值 | `ma_cross` / `rsi_reversion` / `breakout` / `llm` |
| `--cash` | 配置值 | 初始资金 |
| `--output` | 配置值 | 报告目录 |
| `--warmup` | 配置值 | 入场前跳过的 K 线数 |
| `--json` | 关 | 只打印 JSON 指标 |
| `--symbols` | — | 逗号分隔的交易对（`scan`） |
| `--iterations` | 1 | 更新周期数（`live`） |
| `--poll` | 配置值 | 周期间隔秒数（`live`） |
| `--save-config` | — | 把有效配置写到这个路径 |
| `--addr` | `:8080` | 控制台监听地址（`web`） |
| `--no-open` | 关 | 不自动打开浏览器（`web`） |
| `--review` | 关 | 运行结束后让 LLM 写一段复盘（需要 `LLM_API_KEY`） |

### `trade` 专属参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--execute` | 关 | 下真单（默认纸面，不发任何订单） |
| `--yes` | 关 | 配合 `--execute` 时跳过 stdin 确认 |
| `--state` | `./trade-state.json` | 重启持久化的状态文件；实盘模式必须有状态文件 |
| `--cycles` | 0 | 跑满 N 个周期后退出（0 = 一直跑到被停止） |
| `--leverage` | 配置值 | 永续杠杆倍数 |
| `--veto` | 关 | 用 LLM 第二意见把关新入场（fail-open；需要 `LLM_API_KEY`） |

`trade` 是持续循环：每个轮询周期做一次决策（先检查保护性退出，再按市值更新，然后对最后一根
已收盘 K 线的信号行动），结果持久化到状态文件，重启时恢复持仓、权益高水位和风控状态，
而不是重新入场。

### `tune` / `tune-prompt` 专属参数

| 参数 | `tune` 默认 | `tune-prompt` 默认 | 说明 |
| --- | --- | --- | --- |
| `--rounds` | 4 | 3 | 基线之后的模型提议轮数 |
| `--objective` | `sharpe` | `sharpe` | 最大化的指标：`sharpe` / `sortino` / `total_return` / `profit_factor` / `win_rate` |
| `--cv` | 0 | 0 | 不相交的交叉验证折数 + 一个最终留出集（0 关闭） |
| `--stall` | 3 | 3 | 连续 N 轮没有改进就提前停止（0 关闭） |
| `--save-best` | — | — | 把最优参数（`tune`）或最优人设（`tune-prompt`，写入 `llm.prompt`）保存成 YAML 配置 |
| `--no-clamp` | 关 | — | 不把模型提议夹回策略的合法参数范围 |

---

## 配置

`config.yaml` 保存全部设置，命令行参数覆盖它。查找顺序：`--config` 指定的路径 →
当前目录的 `config.yaml` → 内置默认值。命令行能覆盖的只有 `symbol`、`days`、`strategy`、
`cash`、`output`、`warmup` 这几项；其余都在文件里改。

下面是全部键及其内置默认值：

```yaml
agent:
  name: trading-agent
  symbol: BTCUSDT
  timeframe: "1d"
  history_days: 730

data:
  provider: binance        # 唯一的数据源：Binance USDT 永续公开 REST
  bars_per_year: 0         # 0 = 按 provider 推断（Binance 为 365，加密货币全年无休）

strategy:
  name: ma_cross           # ma_cross | rsi_reversion | breakout | llm
  params: {}               # 见下表，缺省项取各策略的默认值

risk:
  initial_cash: 100000
  max_position_pct: 0.95
  max_risk_per_trade_pct: 0.02
  stop_loss_pct: 0.06
  take_profit_pct: 0.18
  max_drawdown_pct: 0.25
  max_daily_loss_pct: 0.05
  allow_short: false       # 打开后才会执行策略的做空信号；只做多是更安全的默认
  max_open_positions: 1
  leverage: 1              # 保证金倍数；1 = 无杠杆的永续仓位，小于 1 会被夹到 1

execution:
  commission_bps: 1.0
  slippage_bps: 5.0
  min_trade_notional: 100
  liquidate_at_end: true
  lot_size: 0.0001

backtest:
  warmup_bars: 60
  output_dir: reports

live:
  poll_seconds: 60
  lookback_days: 400
  paper_trading: true
  state_file: ""           # 空 = trade-state.json（CLI）或 sessions/<mode>-futures-<symbol>.json（控制台）
  margin_mode: ISOLATED    # 或 CROSS

llm:
  base_url: https://api.openai.com/v1
  model: gpt-4o-mini
  timeout_sec: 180         # 单次补全的上限；推理模型可能超过一分钟
  max_tokens: 1024
  temperature: 0
  veto_enabled: false      # 用模型第二意见把关新的实盘入场（fail-open）
  veto_cache_sec: 900      # 同一提议的否决结论缓存秒数；硬风控每周期仍重新检查
  prompt: ""               # llm 策略的交易人设；空 = 内置的保守人设；tune-prompt 会把胜者写回这里
```

API 密钥**绝不**出现在配置文件里：模型密钥在调用时读 `LLM_API_KEY`（回退 `OPENAI_API_KEY`），
交易所密钥读 `BINANCE_API_KEY` / `BINANCE_SECRET_KEY`，桌面版和账号模式则分别从本地凭据文件
和用户保险库注入。这样任何一份配置都不可能泄漏秘密。

### 策略参数

| 策略 | 参数（默认） |
| --- | --- |
| `ma_cross` 双均线交叉 | `fast` 10、`slow` 30、`min_gap_pct` 0 |
| `rsi_reversion` RSI 均值回归 | `period` 14、`lower` 30、`exit_level` 55、`atr_period` 14、`atr_stop_mult` 2.5 |
| `breakout` 唐奇安通道突破 | `lookback` 20、`exit_lookback` 10、`atr_period` 14、`atr_stop_mult` 2.0 |
| `llm` 模型目标仓位 | `fast` 10、`slow` 30、`period` 14、`atr_period` 14、`llm_step` 1、`llm_window` 30 |

每个参数都带合法范围与步长（见 `internal/strategy/spec.go`），控制台用它渲染输入框，
调优循环用它把模型提议夹回合理区间。`llm_step` 控制回测里每隔几根 K 线问一次模型，
`llm_window` 控制送入模型的收盘价根数。

### 仓位计算

仓位计算是杠杆感知的：一笔仓位能控制的名义金额取三者最小值——
`equity × max_position_pct × leverage`、固定比例的风险上限
（`equity × max_risk_per_trade_pct` ÷ 止损距离），以及余额能押的保证金（`cash × leverage`）。
`leverage: 1` 时公式退化为无杠杆情形。**每笔交易的风险金额不随杠杆增长，只有名义金额会。**

年化指标按一年 365 根 K 线计算（Binance，加密货币全年交易）。`config.BarsPerYear()` 读取该
默认值，`data.bars_per_year` 可以覆盖。因为市场天天开盘，「当日亏损上限」和固定止损面对的是
连续的价格行为——隔夜跳空穿过止损正是实盘加密货币的常态失败模式。

---
## 实盘安全模型

真实下单需要**同时**满足三道闸门，缺一不可：

1. **进程级闸门 `TA_ALLOW_LIVE`。** 只有精确等于 `1` 才放行。显式导出了其他非空值
   （比如 `TA_ALLOW_LIVE=0`）视为操作者的明确拒绝，桌面版的开关也无法越过。只有在变量
   **不存在或为空**时，桌面版才会去看自己的「允许本机实盘下单」开关；服务器版不接这个
   开关，环境变量是网络可达部署上唯一的开关。
2. **人工确认。** 控制台必须发送短语 `确认实盘`；CLI 的 `trade --execute` 必须在 stdin 输入
   `yes`（或传 `--yes`）。一次误点永远不该成为真金白银的开关。
3. **交易所密钥。** 没有密钥，经纪层直接拒绝构造下单运行器。

此外：

- `--execute` / 实盘会话必须有状态文件；没有它就不会启动。
- 实盘经纪只支持 **单向持仓模式**。账户若处于 Hedge Mode，启动时直接报错。
- 启动时把本地账本和交易所的带符号持仓对账，不一致就拒绝继续。
- 客户端订单 ID 幂等：超时后按同一个 ID 去查询，而不是盲目再发一单。
- 签名用交易所时钟：从 `/fapi/v1/time` 测得偏差并减去一半往返延迟，所以普通延迟不会被
  误判为时钟漂移。主机时钟偏差超过一秒会让每一个签名请求都以 `code -1021` 失败——从启动时
  的杠杆设置开始，看起来像权限问题，其实只是时钟。若请求仍因时间戳被拒，经纪会重新对时并
  **只重试一次**；`-1021` 在撮合引擎看到请求之前就被拒绝，所以被拒的那次不可能产生订单。
  其他错误一律不重试。Windows 上 `w32tm /resync` 可一劳永逸地校正时钟。

实盘前：先用交易所测试网或演示账户跑一段有意义时长的纸面循环；按你真正打算用的杠杆去
设定 `max_risk_per_trade_pct`；确认该场所对你账户的规则（杠杆上限、逐仓 / 全仓、税务、地区可用性）。

---

## 交易所侧保护单与对账

执行中的永续会话会通过 Binance 的 Algo Order 接口（`POST /fapi/v1/algoOrder`）提交一张
全平仓的 `STOP_MARKET`，以及在配置了正的止盈目标时再提交一张 `TAKE_PROFIT_MARKET`。
两条腿都是：

- `closePosition=true`、`positionSide=BOTH`、`workingType=MARK_PRICE`，触发价来自本地同一套
  风控计算；
- 客户端 ID 以 `tap-` 为前缀，便于识别哪些保护单是自己的；
- 相互独立的市价触发腿，**不是 OCO，也不是限价单**；滑点和交易所故障依然可能发生。

纸面会话不会向交易所提交任何保护单。

启动时和每个实盘周期都会检查自己名下的腿：身份、方向、类型、触发价、状态逐项核对。
缺失的腿在持仓时补挂并复查。冲突的保护单在**持仓时阻断交易**；空仓时则先撤销自己名下
（`tap-` 前缀）的残留腿再复查，只有撤单失败才停下。来历不明（`unknown`）始终阻断。会话的
`protection` 快照报告以下状态之一，并附 `checked_at` 和观察到的腿 ID：

| 状态 | 含义 |
| --- | --- |
| `not_required` | 纸面会话或无持仓，不需要保护 |
| `verified` | 预期的每条腿都在、身份与触发价一致 |
| `partial` | 只有一部分腿得到核验 |
| `missing` | 预期的腿不存在 |
| `conflict` | 交易所上的保护单与本地预期矛盾 |
| `unknown` | 无法确定（查询失败等） |

`protection_active` 只是「会话在跑」的指示，不是「停止后保护单仍在」的证明。

本地退出之前，Agent 会先撤掉自己的保护腿：撤单失败会**阻断这次退出**，而不是冒重复平仓的
风险。撤单 ACK 会核验原始 ID、`code` 与 `msg`，之后再查询一次终态，确认该腿确实已取消且
**没有被触发**；任何缺字段、已触发 / 已成交、查询失败都会让会话停下来，不再撤下一条腿，
也不授权本地平仓。

交易所持仓或开仓价发生了变化就必须对账：Agent 不会从持仓 / 余额的差额里反推成交价和手续费。
全账户自动对账、资金费率与强平模拟**没有实现**。

交易路径在加载历史、询问策略之前先检查保护性退出；策略失败不可能跳过一个已存在的止损。
保护性平仓不会在同一周期内重新开仓。永续使用独立的保证金账本：钱包余额 + 未实现盈亏，
开仓只预留保证金而不是扣除全部名义金额。

---

## 崩溃锁与人工恢复

实盘下单之前，会先把意图持久化为一份耐久文件：

| 文件 | 写入时机 | 含义 |
| --- | --- | --- |
| `<状态文件>.order-pending.json` | 普通订单提交前 | 订单意图；超时后按同一客户端 ID 查询，而不是再发一单 |
| `<状态文件>.protective-pending.json` | 保护单 POST 前 | 原始的保护单客户端 ID |

两份文件都只在账本**成功保存并得到确认**之后才清除。交易所结果不确定或账本持久化失败都会
停止自动下单。崩溃后重启，只要还残留着任何一份，启动就拒绝交易，直到操作者去交易所核对、
修好账本。正常的停止 / 保存 / 重启绝不会抹掉这些证据。**不要删掉 pending 文件来绕过这道锁。**

### 恢复保护单崩溃锁

残留的 `protective-pending.json` 即便所有预期的腿都看得见，也需要人工恢复：

1. 到 Binance 审查持仓、成交和原始保护单 ID。恢复前先停止会话。这条流程只用于保护单意图锁，
   **不**适用于无法解释的成交、持仓不一致，或普通订单的 pending 文件。
2. 通过控制台现有的鉴权调用 `POST /api/session/recover`，请求体 `{"confirm":"确认恢复"}`
   （页面上对应「人工核验恢复」按钮）。恢复会读取交易所持仓、原始保护单身份和 USDT 钱包。
   证据不完整 / 相互冲突、存在普通订单 pending 文件，或有其他无关的不确定性，都会被拒绝。
3. 恢复成功后保存本地账本并清除崩溃锁。它**不发任何交易所订单**、不启动策略，也不是
   全账户对账。再次启动是另一个动作，走正常的实盘闸门。

这些路径都有离线的交易所桩覆盖；这不等于生产级交易所认证，也不保证断网时的保护。

---

## 状态文件与决策日志

控制台的会话文件按「纸面 / 实盘」和交易对分区，命名为 `<mode>-futures-<symbol>.json`，例如
`sessions/paper-futures-BTCUSDT.json`。CLI 的 `trade` 默认用 `./trade-state.json`。
旧的 `trade-state.json` 不会被静默导入或覆盖；请先备份并手动核对仓位再迁移。显式指定的
`state_path` 必须与保存的交易对和模式匹配；旧的现货记账文件需要手动对账。账本里持久化了
所有者，另一个账号无法接管。

决策日志保存在账本旁边、同名加 `.log.jsonl` 的文件里，例如
`sessions/paper-futures-BTCUSDT.log.jsonl`（每个决策周期一行 JSON，追加写入）。日志有
**滚动上限 500 条**：内存只保留最近 500 个周期，每累计 500 次追加就把文件重写为最近 500 条，重启时也只回读
最后 500 条。长期运行的会话会丢弃最旧的决策行；需要完整历史请自行归档这个文件。重启控制台时
最近的历史会被恢复而不是清空；打开控制台会立刻显示最近一次运行的日志，哪怕还没启动会话，
所以一次全新启动不会看起来像丢了历史。

每一行都带策略自己的解释（页面上的「说明」列）：正常周期是模型的一句话理由，降级周期
（例如 `ai_unavailable`）是失败详情。这正是把「密钥错了」「端点不可达」「答案解析失败」
区分开的东西，而不是八行一模一样的「AI 不可用」。控制台会在日志页签下方显示日志路径，
写入失败会在那里报告，而不是悄悄丢掉。日志写不进去**不会**停止会话：日志只是观察交易循环，
不参与它。

---

## HTTP API

背后的 HTTP 接口足够小，可以直接用脚本调用。「鉴权」一列中：*令牌* 指服务器版配置了令牌时
整个路由表都要求的访问令牌；*登录* 指启用 `--users` 后额外要求的用户会话。

| 方法 | 路径 | 鉴权 | 用途 |
| --- | --- | --- | --- |
| `GET` | `/healthz` | 令牌 | 存活探针，返回 `{"status":"ok","time":"…"}` |
| `GET` | `/api/config` | 令牌 | 有效配置、策略元数据与实盘闸门状态 |
| `GET` | `/api/strategies` | 令牌 | 内置策略及其参数规格 |
| `GET` | `/api/market?symbol=BTCUSDT&interval=1h` | 令牌 | 公开的 Binance 报价与 K 线（15m / 1h / 4h / 1d），独立于交易 |
| `GET` | `/api/symbols` | 令牌 | 该场所全部交易对，按 24 小时成交额排序 |
| `POST` | `/api/auth/register` | 令牌 | 注册并直接登录（仅账号模式） |
| `POST` | `/api/auth/login` | 令牌 | 登录，设置 `ta_session` cookie |
| `POST` | `/api/auth/logout` | 令牌 | 退出登录；保险库与已存密钥不受影响 |
| `GET` | `/api/auth/me` | 令牌 | 当前用户，以及哪些密钥已保存（只返回布尔值与模型 URL，不回显秘密） |
| `POST` | `/api/auth/credentials` | 令牌 + 登录 | 保存当前用户的交易所与模型密钥 |
| `POST` | `/api/local/credentials` | 令牌 | 桌面版：把连接设置写到本地凭据文件（**仅桌面版注册**） |
| `GET` | `/api/session` | 令牌 + 登录 | 有效会话设置、权益、持仓、风控、成交与日志 |
| `GET` | `/api/account` | 令牌 + 登录 | 只读地读取当前用户的 USDT 永续钱包，不初始化运行器 |
| `POST` | `/api/session/start` | 令牌 + 登录 | 启动交易循环（`interval_seconds`、`execute`、`confirm`、`leverage`、`state_path`、`veto` 等） |
| `POST` | `/api/session/stop` | 令牌 + 登录 | 停止循环并持久化状态 |
| `POST` | `/api/session/step` | 令牌 + 登录 | 立刻跑一个周期 |
| `POST` | `/api/session/recover` | 令牌 + 登录 | 人工确认后清除保护单崩溃锁，请求体 `{"confirm":"确认恢复"}` |
| `POST` | `/api/session/review` | 令牌 + 登录 | 对本次会话**真实发生**的成交做 LLM 复盘 |
| `POST` | `/api/backtest` | 令牌 + 登录 | 跑回测、持久化、返回指标与曲线（加 `"review": true` 可附带 LLM 复盘） |
| `POST` | `/api/tune` | 令牌 + 登录 | 跑 LLM 参数调优循环，返回逐轮报告 |
| `POST` | `/api/tune-prompt` | 令牌 + 登录 | 跑人设迭代循环，返回胜出的人设 |
| `POST` | `/api/models` | 令牌 + 登录 | 向配置的模型端点查询它提供的模型列表 |
| `GET` | `/api/runs` | 令牌 + 登录 | 列出已保存的运行，最新在前 |
| `GET` | `/api/run?name=…` | 令牌 + 登录 | 重新打开某次运行的曲线、交易与订单 |
| `GET` | `/runs/<name>/report.html` | 令牌 + 登录 | 某次运行的静态报告（按所有者隔离） |
| `POST` | `/api/shutdown` | 令牌 | 停止进程——**仅桌面版** |

`/api/shutdown` 只在桌面外壳提供了取消函数时才注册，所以一个网络可达的控制台永远不可能被
HTTP 请求关掉。同理 `/api/local/credentials` 只在桌面版注册，网络服务器不会接受匿名的凭据写入。

```powershell
# 纸面会话：本地模拟成交，不会有订单离开机器
curl.exe -X POST http://localhost:8080/api/session/start `
  -H "Content-Type: application/json" `
  -d '{\"symbol\":\"BTCUSDT\",\"strategy\":\"ma_cross\",\"days\":730,\"interval_seconds\":60}'

# 停下来，看看发生了什么
curl.exe -X POST http://localhost:8080/api/session/stop
curl.exe http://localhost:8080/api/session
```

---
## AI 功能

五个由模型驱动的功能叠在同一个 Agent 之上。它们共享一个 `llm` 客户端——基于 `net/http` 的
轻量 OpenAI 兼容 REST 封装，项目因此仍然只有一个第三方依赖——而且在模型不可用时全部优雅降级。
API 密钥永远不会写进配置文件：

- **桌面版**：打开 **设置 → Binance 与 AI 服务**，填入 **AI 服务 URL**（如 `https://api.openai.com/v1`）
  和 **AI Token**，然后点 **获取模型**：控制台会问该端点提供哪些模型，并在 **AI 模型名** 字段
  上提供一个选择器，不必凭记忆敲模型名（存过 Token 之后，打开页面会自动拉取）。值保存到上文的
  本地凭据文件。
- **服务器 / CLI**：导出 `LLM_API_KEY`（回退 `OPENAI_API_KEY`），非 OpenAI 端点在 YAML 里设
  `llm.base_url` / `llm.model`。账号模式下每个用户在自己的保险库里保存模型密钥。

任何 OpenAI 兼容端点都行。**AI 决策** 页会说明模型是否就绪；没配置时会准确指出该打开哪个面板。

`POST /api/models` 支撑那个选择器。它能读 OpenAI 及多数网关的 `data[].id` 信封、Ollama 的
`models[].name`，以及裸的字符串数组，所以自托管端点不会被拒之门外；支持对话的模型排前面，
embedding / 音频 / 图像条目排最后。手动输入始终可用——没有 `/models` 路由的网关会报告原因，
而不是留下一个空列表。

| 功能 | CLI | 控制台 | 参数 / 配置 | 没有密钥时 |
| --- | --- | --- | --- | --- |
| LLM 策略 | `--strategy llm` | **AI 决策** → 使用 AI 策略 | `llm.*` | 回测照跑，策略输出全空仓（hold）曲线 |
| LLM 入场否决 | `trade --veto` | **AI 决策** → 入场否决 | `llm.veto_enabled` | fail-open：入场放行，保护性止损永远不被拦 |
| LLM 复盘 | `backtest --review`、`trade --review` | **AI 决策** → 回测复盘 / 实盘复盘 | `llm.*` | 页面标注「模型不可用」而不是报错 |
| LLM 参数调优 | `tune` | **AI 决策** → 调优策略参数 | `llm.*` | 跑基线，跳过提议轮，保存基线 |
| LLM 人设迭代 | `tune-prompt` | **AI 决策** → 迭代交易人设 | `llm.prompt` | 跑基线，保留当前人设 |

控制台给模型单独一页（`#/ai`）：决策、否决、复盘和自我迭代集中在一处，不再藏在策略下拉框后面。
那里的每个控件描述的都是**下一次**会话；正在运行的循环永远不会被改写。实盘复盘
（`POST /api/session/review`）审视的是本次会话真实做出的成交，而不是一次假想的回测。

调优循环和复盘各只有一份可复用实现（`internal/tune` 与 `llm.Review`）：CLI 命令和 Web 端点跑的是
完全相同的代码，所以两个入口都会把提议夹回策略的合法参数范围、都会在停滞守卫上提前停止、
都会在模型缺席时以同样的方式降级。

在实盘循环里，LLM 策略走单次决策路径（`LastDecision`）：每个轮询对最新一根 K 线只调一次模型，
而不是在整个回看窗口上逐根重新生成。400 天的回看因此每轮只花一次调用，而不是约 400 次。
回测保留逐根信号（`Generate`），它确实需要每一根；在那里调高 `llm_step` 可以限制模型调用次数。

否决是**故意 fail-open** 的：客户端被禁用、传输错误、答案解析失败，都会放行交易，这样一个宕掉的
模型永远不可能悄悄拦住止损或风控熔断平仓。只有模型明确拒绝（`approve: false`）才会拦，而且拦的是
**新入场**——保护性退出和回撤熔断永远不受它把关。否决结论按 `llm.veto_cache_sec` 缓存，
硬风控限制（止损距离、仓位、熔断）每周期照样重查，缓存的只是模型的软意见。

`tune` 循环由模型提议：每一轮模型给出一组新参数，循环在同一份序列上回测并按目标指标
（`--objective sharpe|sortino|total_return|profit_factor|win_rate`）留下胜者。`--cv N` 把序列切成
N 个不相交的折再加一个最终留出集，减少过拟合；`--save-best PATH` 把胜出的参数写成可复现的配置。
`tune-prompt` 用同样的骨架迭代 `llm` 策略的交易人设文本，胜者写回 `llm.prompt`。

```powershell
# 让模型决定目标仓位的策略
.\bin\trading-agent.exe backtest --strategy llm --symbol BTCUSDT

# 用模型第二意见把关新的实盘入场（fail-open）
.\bin\trading-agent.exe trade --symbol BTCUSDT --strategy rsi_reversion --veto

# 复盘附加到运行报告里
.\bin\trading-agent.exe backtest --symbol BTCUSDT --review

# 调优：4 轮提议，留下 Sharpe 最高者并保存
.\bin\trading-agent.exe tune --strategy rsi_reversion --objective sharpe --rounds 4 --save-best runs\best.yaml

# 人设迭代：3 轮改写，3 折交叉验证
.\bin\trading-agent.exe tune-prompt --strategy llm --rounds 3 --cv 3 --save-best runs\persona.yaml
```

---

## 回测的真实性与局限

这里的回测诚实但简化：日线、下一根开盘的市价单、固定 bps 成本、没有部分成交、做空没有借币成本，
止损假设按止损价成交（真实的跳空可能糟糕得多）。把结果当作一个想法的合理性检验，而不是预测。

每次运行写到 `reports/<symbol>-<strategy>-<时间戳>/`，包含：

| 文件 | 内容 |
| --- | --- |
| `report.html` | 自包含的 HTML 报告，内联 SVG 资金曲线 |
| `report.md` | Markdown 摘要 |
| `metrics.json` | 指标：初始 / 最终权益、总收益、年化收益与波动、Sharpe、Sortino、Calmar、最大回撤、暴露度、交易数、胜率、盈亏比、平均盈 / 亏、期望、总手续费 |
| `equity.csv` | 资金曲线 |
| `trades.csv` / `orders.csv` | 已平仓交易与订单明细 |
| `run.json` | 这次运行的完整配置与数据，供控制台重新打开 |

无法定义的指标在 JSON 里是 `null` 而不是 `0`。

---

## 离线测试与 CI

单元测试从不碰网络：`internal/testfx` 为给定的 `(symbol, days, seed, end)` 生成确定性的日线，
engine / report / webui 等套件都吃它。注意它生成的是**工作日**序列（跳过周六、周日，波动按 1/252
年化），与真实 Binance 的 7×24 日线并不一样；离线套件覆盖的是指标、风控与记账逻辑，不覆盖
周末 K 线的连续性。任何指标、风控规则或记账路径的改动都会体现为不同的
交易数或收益，所以套件既快又可复现，而 CLI 和控制台仍然与真实的 Binance 对话。
实盘经纪的保护单、撤单、崩溃锁与恢复路径由离线的交易所桩覆盖。

控制台的设置面板另有一个纯 Node 的 UI 测试，不需要 `package.json`：

```bash
node --test tests/ui/settings.test.cjs
```

`.github/workflows/ci.yml` 在每次 push / pull request 上跑两组任务（Go 1.23.4，ubuntu）：

- **checks**：`gofmt -l .` 必须为空、`go build ./...`、`go vet ./...`、`go test -count=1 ./...`
- **editions**：交叉编译三个产物——Windows amd64 桌面版（`-H=windowsgui`）、Linux amd64 与
  arm64 服务器版，均为 `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`

CI 环境显式设置 `TA_ALLOW_LIVE=0` 并清空全部交易所 / 模型密钥，任何测试都不可能意外接触实盘。
`.gitattributes` 把 Go 源码与 shell 脚本强制为 LF，`.ps1` 为 CRLF，避免 Windows 检出后 gofmt
把每个文件都报成未格式化。

---

## 部署

### systemd（`deploy/trading-agent.service`）

```bash
sudo useradd --system --home /var/lib/trading-agent --create-home trading-agent
sudo install -m 0755 trading-agent-server-linux-amd64 /usr/local/bin/trading-agent-server
sudo install -m 0644 config.yaml /etc/trading-agent/config.yaml
sudo install -m 0644 deploy/trading-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now trading-agent
journalctl -u trading-agent -f                 # 看日志
sudo cat /var/lib/trading-agent/token          # 首次启动生成的访问令牌（0600）
```

单元文件做了加固：`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、空的
capability 集合、`MemoryDenyWriteExecute`，只允许写 `/var/lib/trading-agent`。`SIGTERM` 触发优雅
关闭，进行中的回测会跑完而不是被切断。`BINANCE_API_KEY` / `LLM_API_KEY` 这类秘密放在
`EnvironmentFile=/etc/trading-agent/env` 里（单元文件中已注释），不放进配置文件。

### Docker（`deploy/Dockerfile`）

```bash
docker build -f deploy/Dockerfile -t trading-agent .
docker run --rm -p 8080:8080 -v ta-data:/data trading-agent
docker logs trading-agent          # 打印令牌和要打开的 URL
```

多阶段构建，运行时是 Alpine 3.20 + 非 root 用户（uid 10001）。报告和访问令牌都在 `/data` 卷上，
容器替换后依然保留。镜像预设 `TA_ADDR=0.0.0.0:8080`、`TA_CONFIG=/etc/trading-agent/config.yaml`、
`TA_OUTPUT=/data/reports`、`TA_TOKEN_FILE=/data/token`、`TZ=UTC`，并带一个每 30 秒用
Bearer 令牌打 `/healthz` 的 `HEALTHCHECK`。

---

## 项目结构

```
cmd/trading-agent/           CLI 入口
cmd/trading-agent-desktop/   Windows 桌面入口（GUI、无控制台、无参数）
cmd/trading-agent-server/    Linux 服务器入口（无头、令牌鉴权、可选账号登录）
deploy/                      systemd 单元与 Dockerfile
docs/ui-review.html          交易工作台的 UI 设计评审与优化方案
tests/ui/                    控制台设置面板的 Node 测试
internal/
  model/          Bar 与 Series 等共享行情类型
  indicators/     SMA、EMA、RSI、ATR、滚动最高 / 最低、shift；预热区用 NaN 标记
  strategy/       ma_cross、rsi_reversion、breakout、llm，以及参数规格（spec.go）
  risk/           仓位计算、保护性止损、组合级熔断
  portfolio/      现金、持仓、已实现盈亏、按市值的资金曲线
  broker/         纸面经纪 + Binance 永续实盘客户端：订单、保护单、意图日志、时钟校正
  engine/         事件循环（回测 + 实盘单步）、LLM 复盘
  metrics/        Sharpe、Sortino、Calmar、回撤、胜率、盈亏比……
  marketdata/     Binance 永续 K 线、标记价、24h 行情与币种列表客户端
  state/          实盘会话的 JSON 持久化（原子写入）
  live/           实盘会话运行器：经纪接线、对账、崩溃锁、实盘闸门
  live/session/   控制台的实盘循环状态机：启动 / 单步 / 停止 / 恢复，决策日志
  report/         CSV / JSON / Markdown / HTML 写入器
  webui/          HTTP 服务、JSON API、令牌与账号鉴权、嵌入的控制台（static/）
  auth/           多用户账号、登录会话与每用户凭据保险库（纯标准库 PBKDF2 + HMAC）
  localcreds/     桌面版的本地连接设置文件（credentials.json）
  llm/            OpenAI 兼容客户端：策略、否决、复盘、调优、人设提议、模型列表
  tune/           LLM 参数调优与人设迭代循环（CLI 与 Web 共用），含交叉验证
  config/         类型化配置与 CLI 覆盖规则
  cli/            命令行界面
  shell/          操作系统小助手：打开浏览器、挑空闲端口、错误对话框
  testfx/         仅供单元测试使用的确定性离线 K 线
```

---

## 自定义策略

实现 `strategy.Strategy` 接口，并在 `strategy.New` 的 `switch` 里注册：

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

信号是一个**目标仓位**：`1` 多头、`0` 空仓、`-1` 空头。仓位大小、下单和止损由引擎处理；
`Signals.Normalize` 会补齐你没设置的任何水平。想让它出现在控制台与调优循环里，再到
`internal/strategy/spec.go` 的 `Specs()` 中加一条参数规格即可。

---

## 其他文档

- `LIVE_TRADING_REFACTOR.md`：实盘重构总方案，当前的执行入口
- `PLAN.md`：基于代码审查的安全实用重构计划与逐项验证记录
- `ROADMAP_AGENT.md`：项目完善与扩展规划路线图
- `TASKS.md`：任务认领与交付记录
- `docs/ui-review.html`：交易工作台 UI 设计评审与优化方案

---

## 许可证

MIT
