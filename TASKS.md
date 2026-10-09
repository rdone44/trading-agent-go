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

### T0. 账号隔离与实盘对账重构（2026-10-09）

- 背景：对 `5e0cd75` 的只读审查确认 5 个 P1 缺陷，均会在多账号部署或 AI/网络异常时造成实际损失。
- 范围：`internal/webui/`、`internal/live/`、`internal/strategy/`、`internal/engine/`、`internal/config/`、`internal/state/`、`internal/auth/`。
- 状态：done（5 项 P1 全部修复 + CI 红灯修复 + Windows 权限测试修复）
- 落地：
  1. **账号密钥隔离**：`config.Live.NoEnvKeys`（yaml:"-"）在账号模式下关闭 Binance 环境变量回退；`live.New` 与 `session.hasExchangeKeys` 同时遵守。无密钥账号不再能借用部署者的 `BINANCE_API_KEY` 开实盘，改为明确报错。
  2. **账本隔离**：账号模式禁止自定义 `state_path`，路径改为 `reports/accounts/<user>/sessions/<mode>-<venue>-<symbol>.json`；`state.State.Owner` 记录归属，`live.NewForOwner` 拒绝恢复他人账本。
  3. **报告隔离**：`/runs/` 不再直挂共享目录；新增 `reportFileHandler` 按登录账号重新根目录，匿名与跨账号读取被拒；`/api/runs`、`/api/run`、`report.Write` 全部走 `accountDir(username)`。
  4. **合约保护成交对账**：`Runner.Cycle` 在执行中的合约会话先核验交易所持仓；不一致时持久化 `OrderUncertain` 并 halt，绝不按持仓差猜成交价/手续费，也不再用过期账本下单。
  5. **AI 故障语义**：`strategy.LiveDecision.LastDecision` 增加 `ok` 返回值；模型缺失/超时/无法解析时引擎维持原仓并标记 `ai_unavailable`，不再误判为 `signal_exit` 清仓。回测路径行为不变（仍 forward-fill）。
  6. **附带修复**：`/api/backtest` 的 review 改用账号自己的 LLM 配置；`webui_test` 注入离线 `SymbolList` 修掉 CI 上 `/api/symbols` 打真网导致的 502；`auth` vault 写入显式 `Chmod(0600)` 且 Windows 上跳过 Unix 权限位断言。
- 验证：`gofmt -l .` 空、`go build ./...`、`go vet ./...`、`go test -count=1 ./...` 19 包全绿；`node --test tests/ui/settings.test.cjs` 5 项通过；Windows desktop 与 Linux server 交叉构建成功。
- 未完成：`spotProtective` 仍未在 `New` 中启用（spot 保护单只做过离线契约验证，未对真交易所验证）；race 检测因本机无 gcc 未跑。

### T2. AI 功能可见性重构（2026-10-09）

- 背景：用户反馈「这个项目的核心是 AI，让我去哪里找 AI 呢」。代码里 AI 有五条能力，但 UI 里只有一条（提示词迭代）能看到，而且还要求先把策略下拉切到 `llm` 才显示；入场否决、复盘、调参在页面上根本没有入口。
- 范围：`internal/webui/static/`（`index.html`/`app.js`/`app.css`）、`internal/webui/webui.go`、`internal/live/session/session.go`。
- 状态：done
- 落地：
  1. **AI 升为一级页面**：导航新增「AI」(`#/ai`)，页面含四张卡片 —— AI 决策、AI 复盘、AI 自我迭代、以及提示词人设。原先藏在策略表单里的 `#prompt-tune` fieldset 整体迁到该页，不再依赖策略下拉。
  2. **入场否决开关**：`ai-veto` → `StartSessionRequest.Veto` → `cfg.LLM.VetoEnabled`，会话状态回显 `veto_enabled` 与 `settings.veto`，所以表单能预填。此前该闸门只能改 YAML 或走 CLI `--veto`。
  3. **AI 复盘两个入口**：回测复盘（`/api/backtest` 带 `review:true`，此前前端从不发送该字段）与实盘复盘（新增 `POST /api/session/review`，用 `Session.ReviewFacts` 把本会话真实成交渲染成事实串交给 `llm.Review`）。无密钥时返回 `review_unavailable` 提示，不报错、不影响交易。
  4. **调参入口**：`ai-tune-run` → `/api/tune`（服务端路由早已存在，前端从未调用），并新增「采纳参数」把获胜参数写回策略表单。
  5. **不再因缺密钥而整体置灰**：所有 AI 控件保持可点，缺密钥时由后端降级并在日志里说明，避免重现「找不到 AI」的观感。
  6. **文案修正**：`/api/config` 新增 `llm_env_key` 与 `llm_model`（只报是否配置，绝不含密钥），AI 页据此显示模型就绪状态；侧栏「请通过 HTTPS 配置密钥」改为按 `desktop` 区分，桌面版说明只监听 127.0.0.1。
- 验证：`gofmt -l .` 空、`go vet ./...`、`go test -count=1 ./...` 19 包全绿；新增 8 项回归测试（`internal/webui/ai_page_test.go`）覆盖 AI 页路由、`/api/config` 就绪字段、实盘复盘无会话/无密钥/正常路径、否决开关开与关；`node --test tests/ui/settings.test.cjs` 5 项通过；Windows desktop 与 Linux server 交叉构建成功；桌面版实机启动并在浏览器中逐屏核对。
- 未完成：AI 页目前不展示「本次会话是否触发过否决」的历史记录，只在运行日志里体现。

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

- 现货撤单全批次预检子项 done：任何 DELETE 前验证全批次订单 NEW/零成交、正有限数量/触发价及唯一 orderId/clientOrderId；后续坏行不会导致先撤有效保护。新增离线 15 场景拒写与双有效订单正例；旧代码回归复现 writes=2，修复后零写入。验证：Go 1.23.4 gofmt clean、go build ./...、go vet ./...、go test ./... -count=1、broker/live race 全绿。T5/P0-1 仍 partial：保护成交身份入账与生产接入待完成；New 未启用现货适配器，Leverage:1/CVFolds=0 原路径未变。

- 现货撤单响应严格确认子项 done：核验完整原订单身份、类型/方向/独立订单、数量/触发价、CANCELED/零成交，缺失或冲突拒绝确认；新增 broker 离线 15 场景及 live 身份冲突回归，修正旧 stub 的不完整响应。验证：Go 1.23.4 gofmt clean、go build ./...、go vet ./...、go test ./... -count=1、broker/live race 全绿。T5/P0-1 仍 partial：保护成交身份对账入账与生产接入待完成；New 未启用适配器，Leverage:1/CVFolds=0 原路径未变。

- 现货 cycle 库存差异阻断子项 done：显式安装 spotProtective 后，每周期先核验总库存（含冻结量）；完全/部分成交疑点、额外库存、微差、非有限余额及查询失败均保存 OrderUncertain + halt，行情/策略/本地卖出不执行，重复周期及恢复后不重试；不猜成交价/手续费、不改账本。离线 8 场景三轮与状态恢复验证通过，gofmt/build/vet/test、broker/live race 全绿。T5/P0-1 仍 partial：缺按保护单身份自动成交入账与生产接入，New 未启用适配器；Leverage:1/CVFolds=0 原路径未变。本轮另独立格式提交修复已有 CLI/tune/webui 的 gofmt 漂移，以恢复全仓提交门禁。

- 现货重启保护恢复子项 done：显式安装 spotProtective 后，runner 对账使用含冻结量的总库存，严格核验净数量/订单确定性，再幂等校验或补挂同源止损；空仓精撤残留。离线 12 场景三轮验证补挂/撤单各只写一次、冲突/查询错误/成交导致余额变化拒写、账本不变。gofmt/build/vet/test 与 broker/live race 全绿。T5/P0-1 仍 partial：New 未启用现货适配器，待 cycle 保护成交对账与生产接入；未改变 Leverage:1/CVFolds=0 现有行为。

- 现货净持仓适配/撤单余额护栏子项 done：live spotProtective 读取扣 base 手续费后的账本数量，沿用 risk stop，broker HasSpotStops 作存在性查询；精撤后核验总持仓与释放的可用余额，保护单已成交消失/部分成交/仍冻结/额外库存/账户错误均拒绝本地卖出许可。离线测试验证 0.999 净数量、重复调用只写一次、拒绝空头与撤单余额边界；全量 gofmt/build/vet/test、broker/live race 通过。T5/P0-1 仍 partial：未在 New 安装适配器，待 cycle 保护成交对账与重启补挂完成后启用；本轮不改变现货执行、Leverage:1 或 CVFolds=0 路径。

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
- 「设置面板重构」落地（commit `f99ba4d`，用户诉求："设置功能重构"）：rail 按交易决策心智重新分组——「交易什么」只留标的/场所/杠杆（数据节奏拆出独立「数据与节奏」fieldset，回看+轮询加一句话解释）；「允许做空」从高级风控折叠区移到「交易什么」，与场所联动（现货下隐藏且自动取消勾选，`applySettings`/futures change 双处同步）；实盘开关下新增 `#execute-key-hint` 密钥来源提示，三态：无账号系统→走环境变量（历史文案）、已登录未存 key→黄警告指向密钥表单、key 就绪→绿确认；「密钥与模型」表单从"auth 启用即显示"收紧为"仅已登录账号可见"（匿名用户只见登录卡，保存不会 401）；实盘确认文案更新（密钥来自账号库而非写死环境变量）。全部 `name=` 不变，`TestUIPageAndScriptAgree` 绿；浏览器实测匿名/登录后无 key/保存 key 三场景提示正确，永续切换联动 `#short-field` 显隐正确。无后端改动、无新依赖。
- 「登录/注册 + 按账号保存密钥」落地（commit `f17bb49`，用户诉求："要登录和注册功能，因为设置里要填 Binance API 和 AI 的 url/token"）：新包 `internal/auth` 零外部依赖（PBKDF2-HMAC-SHA256 密码哈希 10 万轮、HMAC 签名 24h 会话 cookie、0600 单一 JSON vault）；vault 按账号存 `BinAPIKey`/`BinSecretKey`/`LLMAPIKey`/`LLMBaseURL`，密钥经 `config.Live`/`config.LLM` 注入点（`yaml:"-"`，不落盘到 YAML）注入 `live.New`/`llm.New`，缺省回落环境变量。webui 5 个 `/api/auth/*` handler + 会话中间件：启用账号时 `/api/session*`、`/api/backtest`、`/api/tune`、`/api/runs` 等 401 保护，行情与 `/api/config` 保持公开让登录卡可渲染；`/api/auth/credentials` 只回"是否已配置"标志位，永不回显明文。前端：顶栏用户 chip + 登出、board 顶部登录/注册卡（登录/注册 tab 切换、401 失效自动回退登录卡而非"连接中断"）、rail 里「密钥与模型」表单（Binance Key/Secret + AI URL/Token，保存后清空密码类输入、留 URL 可看）。`main.go` 加 `-users` flag：启用账号且未显式指定 token 时自动关 token 层，避免两套鉴权打架（`disableTokenLayer` 纯函数 + `main_test.go` 覆盖 4 种组合与 token 优先级/生成/持久化）。桌面/CLI 不带 `-users` 时 `Auth=nil`，全部历史行为不变（e2e byte-identity 守护）。测试：`auth_test.go` 7 项离线（校验/文件权限/无明文/会话/过期/篡改/用户隔离）+ `auth_accounts_test.go` 4 项（路由守卫、账号生命周期、存密钥过 execute 密钥门对照、禁用回历史）。全仓 build/vet/test 19 包绿；联网验证对真实 Binance 行情跑通注册→存密钥→`/api/auth/me`→带 cookie 过 401→错密码 401→vault 600 权限→UI 全流程（登录卡/密钥表单/用户 chip/登出）真实浏览器确认。
- 「桌面版本机凭据 + 本机实盘开关」落地（用户诉求："那我怎么设置 api 呢"——桌面版凭据表单永久隐藏，页面却让人"找管理员"，而桌面版没有管理员）：新增 `internal/localcreds`（单一 0600 JSON、原子 temp+rename、`Status` 只回布尔位、`Apply` 只覆盖已保存字段所以环境变量回落不变）；桌面 `main.go` 读 `%APPDATA%\trading-agent\credentials.json` 并在启动时套用到 cfg，同时把 `live.DesktopGate` 指向该文件的 `allow_live`；`live.CheckExecutionAllowed` 改为 env `TA_ALLOW_LIVE=1` 优先、显式非空值（含 `0`）一律拒绝、仅变量缺失时由桌面开关决定，服务器版不接线所以仍只认环境变量；webui 新增 `Server.LocalCreds` + `POST /api/local/credentials`（仅 LocalCreds 非 nil 时注册，服务器版 404）、`/api/config` 增加 `local_settings`/`live_gate_env`，`authStatusForRequest` 在桌面模式返回 `enabled/local/username=本机/path` 的合成视图；前端 `applyAuthUi` 在 local 模式直接展开凭据表单、隐藏登录卡与登出 chip、显示文件路径，rail 新增「允许本机实盘下单」开关（独立请求，不会误改密钥），`renderKeyHint`/AI 页文案分别指向设置面板而非环境变量。测试：`internal/localcreds` 6 项（缺文件/0600/不回显/部分保存/覆盖不吞回落/损坏文件可修复/开关往返）、`internal/webui/local_credentials_test.go` 8 项（config 声明、保存不回显、存储密钥过密钥门、URL 校验、服务器版 404、开关往返与互不干扰、auth 端点仍 404）、`internal/live` 新增桌面开关优先级 4 场景、`tests/ui/settings.test.cjs` 新增桌面展开/服务器隐藏 2 项。gofmt/vet/test 全绿，三平台发行构建通过，桌面真机验证：`/api/config` 返回 `local_settings:true, live_gate:true`，浏览器保存密钥后写入 `credentials.json`。

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
