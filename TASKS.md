# trading-agent-go 任务认领

仓库：https://github.com/rdone44/trading-agent-go  
本地：`/root/projects/trading-agent-go`（唯一工作副本；/root/trading-agent-go 为旧克隆，勿用）  
语言：Go 1.23，唯一依赖 `gopkg.in/yaml.v3`。Binance 和 LLM 客户端只用 `net/http`。

## 项目是什么

Go 交易 Agent。流水线：

```
data -> strategy -> risk -> broker -> portfolio -> metrics -> report
```

- 场所：Binance 现货 + USDT 永续（可做多做空、带杠杆）
- 策略：`ma_cross`、`rsi_reversion`、`breakout`、`llm`
- 风控：仓位 sizing（杠杆感知）、ATR/% 止损、止盈、最大回撤熔断、日亏损上限
- 经纪商：paper 和 live，统一 `Broker` 接口。live 默认 paper，需 `--execute` + 输入 `yes` 才真下单
- AI：LLM 策略、入场否决（fail-open）、复盘、参数调优。密钥只从 `LLM_API_KEY` / `OPENAI_API_KEY` 读，不进配置文件
- 两个构建：Windows desktop（`127.0.0.1:8765`，无鉴权）和 Linux server（`0.0.0.0:8080`，token 鉴权）
- 单测离线：`internal/testfx` 生成确定性 K 线，不碰网络

## 认领规则（单人：coding 全权，as 已退役 2026-10-08）

1. coding（本 profile）独占全部领地，包括原先划给 as 的行情的 `internal/marketdata/`、`internal/portfolio/`、`internal/report/`、`deploy/`、`cmd/`、`README.md`/文档。
2. 工作副本只有一个：`/root/projects/trading-agent-go`（分支 `main`，推 `fork main`）。as 的 worktree `/root/projects/trading-agent-go-as` 与 `tools/a2a_as.py` 已弃用，勿再参考。
3. 每个任务保持独立、一次改一件事，便于回退。
4. 不改 `go.mod` 里已有的依赖约束，不新增非必要依赖。
5. 涉及密钥的代码只从环境变量读，不写进配置、不打印明文。
6. live 下单路径保持现有护栏：默认 paper，`--execute` 才真下单，启动时与交易所对账不一致就拒绝继续。
7. 提交用中文或英文均可，commit message 写清改了什么。

## 任务

### T1. 构建与测试基线

- 范围：在本机（Linux）跑通 `go test ./...` 和 server 构建
- 现状：仓库自带 `build.ps1`（Windows）。Linux 侧没有等价脚本，`cmd/trading-agent-server` 是否能直接 `go build` 未在本机验证
- 完成标准：`go test ./...` 全绿；产出可运行的 server 二进制；把 Linux 构建步骤补进 README
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：done（2026-10-08）
- 落地：
  1. `Makefile`（Linux 侧等价 build.ps1）：`build`/`server`/`test`/`e2e`/`clean`，`server` 目标 `go build -o dist/trading-agent-server ./cmd/trading-agent-server`。
  2. 本机验证：`make server` 产出 13MB ELF x86-64（`file` 确认可执行）；`go test ./...` 18 包全绿。
  3. README `## Two editions` 下补 Linux 的 `make build/server/test/e2e` 命令块。

### T2. 策略层

- 范围：`internal/strategy/`（`strategy.go`、`spec.go`、`llm.go`）
- 现状：四种策略，参数范围和 clamp 逻辑在 `spec.go`
- 可做：新增策略（实现 `strategy.Strategy` 并在 `New` switch 和 `Specs()` 注册）、给现有策略补边界测试
- 约束：信号是目标仓位，`1` 多、`0` 空仓、`-1` 空；缺省值交给 `Signals.Normalize`
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

### T3. LLM 客户端与 AI 功能

- 范围：`internal/llm/`（`llm.go`、`veto.go`、`vetogate.go`、`tune.go`）、`internal/tune/`、`internal/strategy/llm.go`
- 现状：OpenAI 兼容 `/chat/completions`，无 key 时策略全平、veto fail-open、tune 只跑基线
- 可做：错误重试与超时处理、veto 缓存正确性、tune 的 early-stop 与参数 clamp 回归测试
- 约束：veto 必须保持 fail-open，模型挂了不能挡住止损和熔断平仓
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

### T4. 行情数据

- 范围：`internal/marketdata/`（`binance.go`、`futures.go`、`market.go`）
- 现状：Binance 公共 REST，日线
- 可做：请求失败重试、限频处理、K 线缺口检测
- 约束：单测继续走 `internal/testfx`，不在测试里打真网
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：done（P 级全部完成，coding 直接承接；as 的 A2A 派发三次未落到 git，由 coding 补完）
- 落地：
  1. 退避改为可注入：`binanceBackoffs`（1s/2s/4s 指数）+ `binanceSleep`（测试可置 no-op），`binanceRequest` 与 `doGetWithRetry` 共用。
  2. 价格端点补重试：`LastPrice`/`FuturesLastPrice`/`FuturesMarkPrice` 改走 `doGetWithRetry`（原先零重试）；4xx 不重试、5xx/网络 3 次。
  3. K 线缺口检测：`model.CountGapDays`（纯函数）+ `Series.GapDays` 字段，Binance/futures loader 返回前填充。
  4. 测试：`gap_test.go`（CountGapDays 纯函数 3 例、loader 填 GapDays 回归、价格端点重试/4xx 不重试）+ `binance_test.go` 加 `fastRetry` 注入、既有重试测试改为 no-op sleep（不拖慢 CI）。
  - 全绿：gofmt clean、`go build ./...`、`go vet`、`go test ./... -count=1` 18 包通过。

### T5. 经纪商与实盘安全

- 现货 broker 保护原语子项 done：新增单腿 SELL STOP_LOSS，净数量按步长处理、写前幂等查单、字段冲突拒写、响应丢失按原 client ID 查询一次；只精撤 tas- 独立止损，撤单发现已有成交要求对账。离线 httptest/TCP 断连、归属过滤、失败/冲突/成交竞态、dry-run 测试通过；gofmt/build/vet/test 全绿，broker race 通过。T5/P0-1 仍 partial：尚未接 live 的净持仓适配、重启补挂与冻结余额/保护成交对账；现货运行路径本轮未改，不声称端到端保护完成。

- 本轮子项 done：合约保护单写入响应丢失后，按原 clientAlgoId 查询官方 Algo Order 端点一次，完整核验有效 NEW 保护腿，不盲重发；离线 TCP 断连覆盖多空与两腿、后续多轮幂等及查询失败/坏 JSON/字段冲突。gofmt/build/vet/test 全绿，broker/live race 通过。T5/P0-1 仍 partial，仅 spot 保护单未完成；跨进程恢复仍沿用 openAlgoOrders 逐腿对账，未新增保护单持久化意图日志。此记录替代下文历史超时按 ID 查单待办。

- 本轮子项 done：保护单挂单失败后保留本地退出。已确认入场只 halt 新入场，不标记成交不确定；下一轮先确认撤单再 risk halt 平仓。撤单失败/成交不确定仍封锁后续操作；halt 已持久化，离线 runner 保存/恢复后验证退出与撤单超时不重试。engine/live race、全量 gofmt/build/vet/test 通过。本记录替代下文历史“挂单失败 fail-open”及其阻断待办；T5/P0-1 仍 partial，剩余 spot 保护单和保护单超时按 ID 查单。

- 本轮子项 done：合约保护单逐腿幂等补挂。broker 写前查单并严格核验已有腿，只补缺失腿；runner 重启时即使仅剩止盈也补止损。离线多空/重复调用/部分失败恢复/冲突拒写/runner 重启测试；gofmt/build/vet/test 全绿，broker/live race 通过。T5/P0-1 仍 partial（spot、超时按 ID 查单、本地保护被 OrderUncertain 阻断尚未解决）。

- 范围：`internal/broker/`（`binance.go`、`futures.go`、`orders.go`、`journal.go`）、`internal/live/`
- 现状：现货与合约客户端、下单前写 `*.order-pending.json`、超时按 clientOrderId 回查而不是重下、对账不一致拒绝继续
- 可做：补失败路径测试（超时、部分成交、拒单）、合约单向持仓校验
- 约束：不削弱现有护栏。Hedge Mode 目前显式拒绝，不要静默放开
- 认领：coding（钱路径：broker/live）
- 状态：partial — P0-1 交易所侧保护性止损已落地（commit cbf151f，18 包全绿）：开仓后挂 closePosition 的 STOP_MARKET/TAKE_PROFIT_MARKET（MARK_PRICE、价格与本地 risk 同源），平仓前先撤单，reconcile 幂等补挂/清裸单，撤单失败 fail-safe、挂单失败 fail-open。P0-2 execute env 开关（TA_ALLOW_LIVE）done：runner 构造和 Web 会话双层精确校验，仅值 1 放行；paper 不受影响；新增离线 spot/futures、API 400、密钥及状态护栏回归测试。验证：gofmt -l .（空）、go build ./...、go vet ./...、go test ./...。

- P0-1 本轮修正：合约条件单迁到官方 Algo Order API（`triggerPrice` / `clientAlgoId`），查询 `openAlgoOrders`，只按 algoId 撤本程序 `tap-` 保护腿，保留手工/异币种挂单。新增严格契约、归属过滤及错误路径离线测试。验证：gofmt 空、go build ./...、go vet ./...、go test ./... -count=1 全绿。T5/P0-1 仍 partial：spot 保护单、逐腿幂等补挂、超时查单及挂单失败阻断本地保护问题尚未解决；上文历史“挂单失败 fail-open”不能理解为之后本地退出仍可执行。

### T6. 风控与组合记账

- 范围：`internal/risk/`、`internal/portfolio/`、`internal/engine/`
- 现状：杠杆感知 sizing（风险金额不随杠杆放大，名义本金放大）、合约独立保证金账本、止损先于策略执行
- 可做：极端行情（跳空穿透止损）的记账测试、日亏损上限跨日重置
- 约束：止损和熔断平仓不被策略错误或 LLM 否决跳过
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：done（离线回归验证，无生产行为改动）
- 落地：`internal/engine/gap_accounting_test.go` 覆盖现货多头、合约多/空跳空穿透止损，断言按更差开盘价加不利滑点成交、手续费/净损益/钱包/已实现损益一致、保证金释放且不重复平仓；`internal/risk/daily_reset_test.go` 覆盖 UTC+8 午夜重置、新日基线、同日重启暂停延续、事件去重，以及永久熔断/订单不确定状态不被跨日清除。仅新增测试；Leverage:1 与 CVFolds=0 生产代码未变。
- 验证：gofmt -l . 空；go build ./...、go vet ./...、go test -count=1 ./... 全绿；risk/engine 定向 verbose 测试通过。

### T7. Web 控制台与 API

- 范围：`internal/webui/`（`webui.go`、`session.go`、`market.go`、`settings.go`、`convert.go`）；P2-1 起实盘会话状态机独立在 `internal/live/session/`
- 现状：中文控制台内嵌在二进制里，无前端框架。API 见 README 的表格
- 可做：API 回归测试、前端展示问题修复
- 约束：`/api/shutdown` 只在 desktop 构建注册；server 构建必须有 token 才能启动（`--allow-anonymous` 除外）
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：partial（P2-1 会话状态机拆分 done）
- P2-1 落地：状态机收进新包 `internal/live/session`（`Session`/`NewSession`/`StartOptions` + 自有 view 类型），webui.go 只留路由适配（`/api/session*` 四 handler + 关停钩子）；旧类型经别名 `webui.Session/livesession.Session` 保留，外部 `webui_test` 不破；`Settings` wire format 由 `session_wire_test.go` byte-equal 守护；无新依赖、无循环 import（`live` 不 import webui）。
- 「全部币种」落地（commit `44be2d0`，用户诉求："设置不方便，逻辑不通，应获取全部币种而不是单选"）：`marketdata.AllSymbols(venue, limit)` 一次 `/ticker/24hr`（spot）或 `/fapi/v1/ticker/24hr`（futures），只留 USDT/USDC、按 24h 成交额排序；webui `/api/symbols` 带 per-venue 30s 缓存（`symbolsCache`/`symbolsMu`，Server 字段，非全局）；控制台「交易对」挂 `<datalist>` + 「全部币种」按钮（拉全量、可点选、仍可手输），venue 切换自动重拉。离线测试：`symbols_test.go`（过滤/排序/limit/重试/FAPI 端点）+ `regression_test.go`（缓存命中、limit 裁剪、400/405、loader 失败 502）。CLI `scan` 是回测路径、刻意不动。无新依赖、全仓 build/vet/test 绿。
- 「登录/注册 + 按账号保存密钥」落地（commit `f17bb49`，用户诉求："要登录和注册功能，因为设置里要填 Binance API 和 AI 的 url/token"）：新包 `internal/auth` 零外部依赖（PBKDF2-HMAC-SHA256 密码哈希 10 万轮、HMAC 签名 24h 会话 cookie、0600 单一 JSON vault）；vault 按账号存 `BinAPIKey`/`BinSecretKey`/`LLMAPIKey`/`LLMBaseURL`，密钥经 `config.Live`/`config.LLM` 注入点（`yaml:"-"`，不落盘到 YAML）注入 `live.New`/`llm.New`，缺省回落环境变量。webui 5 个 `/api/auth/*` handler + 会话中间件：启用账号时 `/api/session*`、`/api/backtest`、`/api/tune`、`/api/runs` 等 401 保护，行情与 `/api/config` 保持公开让登录卡可渲染；`/api/auth/credentials` 只回"是否已配置"标志位，永不回显明文。前端：顶栏用户 chip + 登出、board 顶部登录/注册卡（登录/注册 tab 切换、401 失效自动回退登录卡而非"连接中断"）、rail 里「密钥与模型」表单（Binance Key/Secret + AI URL/Token，保存后清空密码类输入、留 URL 可看）。`main.go` 加 `-users` flag：启用账号且未显式指定 token 时自动关 token 层，避免两套鉴权打架（`disableTokenLayer` 纯函数 + `main_test.go` 覆盖 4 种组合与 token 优先级/生成/持久化）。桌面/CLI 不带 `-users` 时 `Auth=nil`，全部历史行为不变（e2e byte-identity 守护）。测试：`auth_test.go` 7 项离线（校验/文件权限/无明文/会话/过期/篡改/用户隔离）+ `auth_accounts_test.go` 4 项（路由守卫、账号生命周期、存密钥过 execute 密钥门对照、禁用回历史）。全仓 build/vet/test 19 包绿；联网验证对真实 Binance 行情跑通注册→存密钥→`/api/auth/me`→带 cookie 过 401→错密码 401→vault 600 权限→UI 全流程（登录卡/密钥表单/用户 chip/登出）真实浏览器确认。

### T8. 部署

- 范围：`deploy/`（systemd unit、Dockerfile）、`cmd/trading-agent-server`、`cmd/trading-agent-desktop`
- 现状：README 描述了 hardened systemd unit 和多阶段 Alpine 镜像，未在本机验证
- 可做：验证 Dockerfile 能构建、systemd unit 的路径和权限与文档一致
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

### T9. 文档与配置

- 范围：`README.md`、`config.yaml`、`internal/config/`
- 现状：README 以 PowerShell 为例，Linux 用法散落各节
- 可做：补 Linux 命令示例、配置字段与代码默认值核对
- 约束：不把任何密钥示例写成可直接用的真值
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

## 本轮协作记录

> 2026-10-08 起 as 通道退役（用户决定），coding 全权。以下为历史记录，保留备查。

- 本周期 as 本轮无有效完成回复（T8）：A2A 返回 `Run this command to get the current commit SHA:`，要求调用方执行 `git rev-parse HEAD`，无 `DONE`/`BLOCKED` 或 SHA；复核 worktree 干净、HEAD 仍为 d7941f8，`fork/as/main` 不存在，没有可合并提交。未修改其他 profile。
- coding T6 done：新增离线跳空止损记账与跨日/重启风险状态回归；仅新增测试，不改生产行为。全量 gofmt/build/vet/test 绿后提交并推送 fork/main。

- 本周期 as 本轮无回复（T4）：A2A 返回 `Response Stopped — Repetition Detected`，没有 DONE SHA；核实 as worktree 干净、已同步至 2eb8c72，fork/as/main 仍不存在，没有合并。未修改其他 profile。
- coding P1-2 done（cfc7b5b）：Go 1.23.4 CI 检查 matrix 与桌面/服务器三平台构建；gofmt -l . 空、go build ./...、go vet ./...、go test -count=1 ./... 全绿，三个发行构建全部通过。推送 fork/main 后读取 Actions run 37694155551：completed/success，7 个 job 全绿。公开 API 查询两仓 PR #1 均返回 `curl: (22) The requested URL returned error: 404`，fork 开放 PR 列表为空；当前 profile/env 无 GitHub API token，未更新 PR 描述、未设置分支保护，不改其他 profile。

- 本周期 as 本轮无回复（T4 再派发仍未执行）：provider 原文 `HTTP 426: {"error":"Your Grok CLI version (0.2.99) is outdated. Please update to version 1.0.13 or later via `grok update` or the installation documentation."}`。已核实 as worktree 干净、仍在 c966645，fork/as/main 不存在；没有合并，不改其他 profile。
- coding P1-1 done：调参 CVFolds、CLI --cv、Web cv_folds；训练/验证/holdout 数据隔离，严格多数 + holdout gate，拒绝时恢复基线，逐窗口报告；新增离线 LLM/API、过拟合回退、零值 JSON 字节一致性测试，既有断言未改。验证：gofmt -l . 空、go build ./...、go vet ./...、go test ./... 全绿；额外 go test -race ./internal/tune ./internal/webui ./internal/cli。
- as 本轮无回复（T4 未执行）：A2A 服务返回 provider HTTP 426：`Your Grok CLI version (0.2.99) is outdated. Please update to version 1.0.13 or later via grok update or the installation documentation.`；as worktree 干净，fork/as/main 不存在，无提交可合并。不修改其他 profile 配置。

## 认领记录

| 任务 | 认领人 | 日期 | 状态 |
| --- | --- | --- | --- |
| T1 | coding（单人） | 2026-10-08 | done |
| T2 | coding（单人） | 2026-10-08 | open |
| T3 | coding（LLM/tune 钱路径） | 2026-10-08 | claimed |
| T4 | coding（单人） | 2026-10-08 | done |
| T5 | coding（broker/live 钱路径） | 2026-10-08 | partial (合约保护/现货 broker 原语 done；剩 spot live 接入及对账) |
| T6 | coding（risk/engine 离线回归） | 2026-10-08 | done |
| T7 | coding（web 会话/路由，钱路径相邻） | 2026-10-08 | partial (P2-1 done) |
| T8 | coding（单人） | 2026-10-08 | open |
| T9 | coding（单人） | 2026-10-08 | open |
