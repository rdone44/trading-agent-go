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

## 认领规则（双 agent 协作：coding 统筹 + as 并写）

1. 两个 agent 并行开发，**按领地划分，不碰同一批文件**，避免合并冲突：
   - **coding（本 profile）** 主导 + 统筹验证：`internal/broker/`、`internal/live/`、`internal/engine/`、`internal/risk/`、`internal/llm/`、`internal/tune/`、`internal/strategy/`、`internal/webui/`（钱路径 + AI + 实盘会话）。
   - **as（assistant profile，A2A 9902）** 并写：`internal/marketdata/`、`internal/portfolio/`、`internal/report/`、`deploy/`、`cmd/`、`README.md`/文档（行情、记账、部署、文档——与 coding 钱路径零重叠）。
2. 各用独立 worktree + 独立分支：
   - coding：`/root/projects/trading-agent-go`（分支 `main`，推 `fork main`）。
   - as：`/root/projects/trading-agent-go-as`（分支 `as/main`，推 `fork as/main`）。
   - 共享同一 fork 远端和同一份 root git 凭据（token 只从 env/`~/.git-credentials` 读）。
3. **合并只由 coding 做**：coding 每个周期 pull `as/main` 的新 commit，`gofmt -l` 干净 + `go build ./...` + `go vet ./...` + `go test ./...` 全绿才 merge 进 `main`；不绿就在 TASKS.md 记一条 as 的分支需修复，不硬合。as 不直接推 `main`。
4. 每个任务保持独立、一次改一件事，便于回退。
5. 不改 `go.mod` 里已有的依赖约束，不新增非必要依赖。
6. 涉及密钥的代码只从环境变量读，不写进配置、不打印明文。
7. live 下单路径保持现有护栏：默认 paper，`--execute` 才真下单，启动时与交易所对账不一致就拒绝继续。
8. 提交用中文或英文均可，commit message 写清改了什么。

## 任务

### T1. 构建与测试基线

- 范围：在本机（Linux）跑通 `go test ./...` 和 server 构建
- 现状：仓库自带 `build.ps1`（Windows）。Linux 侧没有等价脚本，`cmd/trading-agent-server` 是否能直接 `go build` 未在本机验证
- 完成标准：`go test ./...` 全绿；产出可运行的 server 二进制；把 Linux 构建步骤补进 README
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

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
- 状态：open（P 级到达时做）

### T5. 经纪商与实盘安全

- 范围：`internal/broker/`（`binance.go`、`futures.go`、`orders.go`、`journal.go`）、`internal/live/`
- 现状：现货与合约客户端、下单前写 `*.order-pending.json`、超时按 clientOrderId 回查而不是重下、对账不一致拒绝继续
- 可做：补失败路径测试（超时、部分成交、拒单）、合约单向持仓校验
- 约束：不削弱现有护栏。Hedge Mode 目前显式拒绝，不要静默放开
- 认领：coding（钱路径：broker/live）
- 状态：partial — P0-1 交易所侧保护性止损已落地（commit cbf151f，18 包全绿）：开仓后挂 closePosition 的 STOP_MARKET/TAKE_PROFIT_MARKET（MARK_PRICE、价格与本地 risk 同源），平仓前先撤单，reconcile 幂等补挂/清裸单，撤单失败 fail-safe、挂单失败 fail-open。P0-2 execute env 开关（TA_ALLOW_LIVE）done：runner 构造和 Web 会话双层精确校验，仅值 1 放行；paper 不受影响；新增离线 spot/futures、API 400、密钥及状态护栏回归测试。验证：gofmt -l .（空）、go build ./...、go vet ./...、go test ./...。

### T6. 风控与组合记账

- 范围：`internal/risk/`、`internal/portfolio/`、`internal/engine/`
- 现状：杠杆感知 sizing（风险金额不随杠杆放大，名义本金放大）、合约独立保证金账本、止损先于策略执行
- 可做：极端行情（跳空穿透止损）的记账测试、日亏损上限跨日重置
- 约束：止损和熔断平仓不被策略错误或 LLM 否决跳过
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

### T7. Web 控制台与 API

- 范围：`internal/webui/`（`webui.go`、`session.go`、`market.go`、`settings.go`、`convert.go`）
- 现状：中文控制台内嵌在二进制里，无前端框架。API 见 README 的表格
- 可做：API 回归测试、前端展示问题修复
- 约束：`/api/shutdown` 只在 desktop 构建注册；server 构建必须有 token 才能启动（`--allow-anonymous` 除外）
- 认领：coding（单人重构，含原 muse 地盘）
- 状态：open（P 级到达时做）

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

- 本周期 as 本轮无回复（T4）：A2A 返回 `Response Stopped — Repetition Detected`，没有 DONE SHA；核实 as worktree 干净、已同步至 2eb8c72，fork/as/main 仍不存在，没有合并。未修改其他 profile。
- coding P1-2 done（cfc7b5b）：Go 1.23.4 CI 检查 matrix 与桌面/服务器三平台构建；gofmt -l . 空、go build ./...、go vet ./...、go test -count=1 ./... 全绿，三个发行构建全部通过。推送 fork/main 后读取 Actions run 37694155551：completed/success，7 个 job 全绿。公开 API 查询两仓 PR #1 均返回 `curl: (22) The requested URL returned error: 404`，fork 开放 PR 列表为空；当前 profile/env 无 GitHub API token，未更新 PR 描述、未设置分支保护，不改其他 profile。

- 本周期 as 本轮无回复（T4 再派发仍未执行）：provider 原文 `HTTP 426: {"error":"Your Grok CLI version (0.2.99) is outdated. Please update to version 1.0.13 or later via `grok update` or the installation documentation."}`。已核实 as worktree 干净、仍在 c966645，fork/as/main 不存在；没有合并，不改其他 profile。
- coding P1-1 done：调参 CVFolds、CLI --cv、Web cv_folds；训练/验证/holdout 数据隔离，严格多数 + holdout gate，拒绝时恢复基线，逐窗口报告；新增离线 LLM/API、过拟合回退、零值 JSON 字节一致性测试，既有断言未改。验证：gofmt -l . 空、go build ./...、go vet ./...、go test ./... 全绿；额外 go test -race ./internal/tune ./internal/webui ./internal/cli。
- as 本轮无回复（T4 未执行）：A2A 服务返回 provider HTTP 426：`Your Grok CLI version (0.2.99) is outdated. Please update to version 1.0.13 or later via grok update or the installation documentation.`；as worktree 干净，fork/as/main 不存在，无提交可合并。不修改其他 profile 配置。

## 认领记录

| 任务 | 认领人 | 日期 | 状态 |
| --- | --- | --- | --- |
| T1 | coding（单人） | 2026-10-08 | open |
| T2 | coding（单人） | 2026-10-08 | open |
| T3 | coding（LLM/tune 钱路径） | 2026-10-08 | claimed |
| T4 | coding（单人） | 2026-10-08 | open |
| T5 | coding（broker/live 钱路径） | 2026-10-08 | partial (P0-1 done) |
| T6 | coding（单人） | 2026-10-08 | open |
| T7 | coding（web 会话/路由，钱路径相邻） | 2026-10-08 | claimed |
| T8 | coding（单人） | 2026-10-08 | open |
| T9 | coding（单人） | 2026-10-08 | open |
