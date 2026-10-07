# trading-agent-go 任务认领

仓库：https://github.com/rdone44/trading-agent-go  
本地：`/root/trading-agent-go`  
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

## 认领规则（单人模式：coding 全责）

1. 当前由 coding 一个 agent 负责全部任务，不再拆分给 muse。做完把状态改成 `done`，附上验证方式（跑了什么命令、结果是什么）。
2. 每个任务仍保持独立、一次改一件事，便于回退。
3. 不改 `go.mod` 里已有的依赖约束，不新增非必要依赖。
4. 涉及密钥的代码只从环境变量读，不写进配置、不打印明文。
5. live 下单路径保持现有护栏：默认 paper，`--execute` 才真下单，启动时与交易所对账不一致就拒绝继续。
6. 提交用中文或英文均可，commit message 写清改了什么。

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
- 状态：claimed — P0 最先做：交易所侧保护性止损 + execute env 开关（见 PLAN.md）

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

## 认领记录

| 任务 | 认领人 | 日期 | 状态 |
| --- | --- | --- | --- |
| T1 | coding（单人） | 2026-10-08 | open |
| T2 | coding（单人） | 2026-10-08 | open |
| T3 | coding（LLM/tune 钱路径） | 2026-10-08 | claimed |
| T4 | coding（单人） | 2026-10-08 | open |
| T5 | coding（broker/live 钱路径） | 2026-10-08 | claimed |
| T6 | coding（单人） | 2026-10-08 | open |
| T7 | coding（web 会话/路由，钱路径相邻） | 2026-10-08 | claimed |
| T8 | coding（单人） | 2026-10-08 | open |
| T9 | coding（单人） | 2026-10-08 | open |
