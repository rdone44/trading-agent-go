# trading-agent-go 项目完善与扩展规划路线图 (Roadmap & Implementation Plan)

> **文档定位**：供后续接手的 Agent / 开发者直接阅读并执行的任务规范与技术规划方案。  
> **项目根目录**：`E:\agent\trading-agent-go`  
> **核心架构规范**：
> 1. 唯一外部依赖为 `gopkg.in/yaml.v3`，禁止随意引入第三方依赖。Binance / LLM 通信均基于 `net/http`。
> 2. 资金安全第一（`Money Path`）：默认 Paper 交易；实盘必须由环境变量 `TA_ALLOW_LIVE=1`、口令「确认实盘」、交易所密钥三重守卫；开平仓必须保证交易所侧止损单对账闭环。
> 3. 单测离线化：使用 `internal/testfx` 生成确定性 K 线，严禁在自动化单元测试中打真实网络。
> 4. 代码变更守则：每次修改保证 `gofmt -l .` 无输出、`go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿。

---

## 阶段一：P0 级安全闭环（最高优先级，会亏钱的隐患必须最先做）

### 任务 1.1：~~现货交易所侧保护单（`spotProtective`）生产接入与对账闭环~~ —— 已作废
- **作废原因（2026-10-09）**：现货场所整体退役。`internal/broker/spot_protective.go`、预检与撤单原语、`internal/marketdata/binance.go` 一并删除，程序只交易 USDT 永续，`venue` 不再是可选项。
- 合约侧保护单闭环不受影响，仍在生产启用：`tap-` 前缀、Algo Order API、逐腿幂等补挂、周期前持仓核对、重启对账，全部由 `internal/live/live.go` 的 `reconcileFutures` 承担。
- 若将来恢复现货，本任务原文保留在 git 历史中，且必须连同现货记账模型一起重新设计，不能只挂回一个适配器。

### 任务 1.2：保护单持久化意图日志（`order-pending.json` 覆盖保护单）
- **背景现状**：
  - 普通市价单在提交前会先写 `<statePath>.order-pending.json`，超时回查 ClientOrderID。
  - 但保护性条件单依赖拉取 `openAlgoOrders` 对账，未落盘独立意图文件。如果在发送保护单 HTTP POST 瞬间进程崩溃，重启后对账存在微小盲区。
- **任务目标**：
  - 为保护单建立意图落盘机制（例如 `<statePath>.protective-pending.json`），记录待挂保护腿的 `clientAlgoId` 与价格。
  - 启动对账时，若发现残留保护单意图，优先查单确认，避免重复补挂。
- **验收标准**：
  - 针对挂单中途崩溃场景编写离线崩溃模拟测试，确保重启能精确恢复状态。

---

## 阶段二：P1 级交易引擎与策略增强（扩大实战能力）

### 任务 2.1：规则策略支持双向交易（做空做多全覆盖）
- **背景现状**：
  - 引擎与永续合约底层完全支持双向持仓与做空机制（`signal = -1`）。
  - 但当前 3 个内置规则策略（`ma_cross`、`rsi_reversion`、`breakout`）均为 **Long-Only**（仅产生 `1` 和 `0`）。
- **任务目标**：
  - `ma_cross`：当快线下穿慢线超过 `min_gap_pct` 且配置 `allow_short=true` 时，发出 `-1` 信号。
  - `rsi_reversion`：当 RSI 严重超买（如 `> 70`）且动量向下时，发出做空入场信号；做空止损为 `close + stopMult * atr`。
  - `breakout`：当跌破前 N 根低点时发出做空信号，追踪止损为 `runMin + stopMult * atr`。
  - 所有策略在 `Signals` 中补充 `TakeProfit` 目标止盈计算，与止损一同传入引擎。
- **验收标准**：
  - 补充双向回测测试用例，验证做空信号在合约回测中的胜率与盈亏统计正确。

### 任务 2.2：回测时间周期扩展（支持日内级 K 线）
- **背景现状**：
  - 行情模块 `internal/marketdata/futures.go`（原 `binance.go` 已随现货退役）硬编码了 `interval=1d`。
  - 尽管 WebUI 图表支持 15m/1h/4h/1d 观察，但回测只能跑日线，无法验证日内高频策略。
- **任务目标**：
  - 将 `config.Agent.Timeframe`（如 `15m`, `1h`, `4h`, `1d`）传递给数据获取层。
  - 更新指标年化计算（`internal/metrics/metrics.go`）：根据时间周期动态计算 `barsPerYear`（例如 1h 对应 $24 \times 365 = 8760$ 根）。
- **验收标准**：
  - CLI 与 Web 端支持指定 `--timeframe 1h` 运行回测并输出正确的指标。

### 任务 2.3：本地 K 线数据缓存（加速回测与调参）
- **背景现状**：
  - 每次执行回测、扫描（`scan`）、参数调优（`tune`），都会重复向 Binance 发起公共 REST 请求。
  - 调参和交叉验证跑 10 轮可能触发上百次网络请求，容易被交易所限频，且效率较低。
- **任务目标**：
  - 在 `internal/marketdata/` 增加轻量级文件缓存层（例如存储在 `cache/klines/<venue>_<symbol>_<interval>.json` 或 CSV/二进制）。
  - 读取时若本地缓存存在且最新时间戳覆盖，直接读取本地；不足部分只做增量拉取。
- **验收标准**：
  - 重复回测耗时从数秒降至毫秒级；离线断网模式下读取缓存可正常回测。

---

## 阶段三：P2 级 AI 与调参体系进阶

### 任务 3.1：LLM 特征动态注入与动态模型容灾（Primary/Fallback）
- **背景现状**：
  - `internal/strategy/llm.go` 中喂给模型的指标特征硬编码为固定长度 SMA、RSI 和 ATR。
  - LLM 客户端仅配置单 URL 与单 Token。若主模型触发 429 配额用尽，只能降级为保持原仓。
- **任务目标**：
  - 支持将更多市场动态特征（如成交量异动、布林带宽度、近期支撑阻力位）格式化喂给 LLM。
  - 在 `internal/llm/` 增加双端点配置（Primary 与 Fallback），当主模型 429 或 5xx 时自动无缝降级切换至备用模型。
- **验收标准**：
  - 单测模拟主模型 429 报错，断言客户端自动切换备用模型并成功生成决策。

### 任务 3.2：并行回测调度器与传统优化器基准
- **背景现状**：
  - `internal/tune/tune.go` 中的每轮回测和 K 折交叉验证全为串行同步执行。
  - 缺乏经典优化器（网格搜索 Grid Search、随机搜索）作为基准对比。
- **任务目标**：
  - 封装 Go 协程池，实现多参数/多折交叉验证的并发执行，大幅缩短调参总时间。
  - 增加基于网格搜索（Grid Search）的离线调参器，作为零 Token 成本的基线对照。
- **验收标准**：
  - 跑 5 轮调参耗时下降 50% 以上；多协程并发写入 Report 和 Metrics 保证线程安全。

---

## 阶段四：P3 级系统工程与交互体验完善

### 任务 4.1：Web 控制台 SSE 实时推送与全局日志查询
- **背景现状**：
  - Web 前端采用定时 HTTP 轮询（3s 一次），且运行日志分散在各账本的 `.log.jsonl` 中，缺乏集中查看与告警推送通道。
- **任务目标**：
  - 在 `internal/webui/` 引入 Server-Sent Events (`/api/events`)，实时向前端推送周期动作、成交结果与风控报警。
  - 增加 `/api/logs` 接口，支持按时间段、按严重级别（Info/Warn/Error）聚合过滤历史决策日志。
- **验收标准**：
  - 浏览器打开控制台无需持续发起状态轮询请求；实盘触发止损或异常时毫秒级弹窗提示。

### 任务 4.2：指标库扩充与算法优化
- **背景现状**：
  - `RollingMax` 和 `RollingMin` 采用嵌套循环，复杂度为 $O(N \times W)$。
  - 缺少 MACD、布林带 (Bollinger Bands)、KDJ、SuperTrend 等量化常用指标。
- **任务目标**：
  - 使用单调双端队列（Monotonic Deque）将滚动极值重构为 $O(N)$。
  - 在 `internal/indicators/` 实现 MACD、Bollinger Bands、SuperTrend。
- **验收标准**：
  - 为所有新指标补充完备的单元测试，覆盖边界值与与基准比对。

---

## Agent 执行约束与行为守则（Strict Directives）

1. **不可破坏现有安全基线**：
   - 绝不在配置文件（`config.yaml`）中落盘任何 API Key 或 Secret。
   - 绝不弱化 `TA_ALLOW_LIVE=1` 环境变量门禁。
   - 绝不在未确认交易所持仓前强行平仓或猜成交。
2. **每次任务独立提交**：
   - 一次只做一个小任务，并编写配套的 `_test.go`。
   - 保持单一外部依赖（`gopkg.in/yaml.v3`），切勿引入庞大第三方库。
3. **自测闭环指令**：
   - 每次修改后依次在项目根目录运行：
     ```powershell
     go test -count=1 ./...
     go vet ./...
     ```
   - 确保全仓 20+ 个包全部通过无告警后再交付。
