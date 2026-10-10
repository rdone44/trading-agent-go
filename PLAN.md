# 安全实用重构计划

> 基于 2026-10-08 的真实代码审查（fork @ bec06fc）。每项都有风险、改法、验证。
> 优先级按「错了会亏钱」排。2h 定时任务按本文件从上往下推进，一次做一项。

## 当前基线变更与本轮验证

- P0-1/T5 永续撤单失败持久化阻断子项：**done** — 新增 `TestProtectiveCancelFailurePersistsWithoutFlattenOrRetry`，以真实离线 broker→adapter→engine→runner 链路覆盖多空、首腿/第二腿、空 ACK/身份错误/HTTP 503；第二腿失败时首腿已成功也不得放行本地平仓。断言原仓位/开仓手续费/现金不变、零市价单/零本地成交、halt + OrderUncertain 落盘；随后三轮及保存账本恢复后的周期均不再调用行情或交易所。验证：Go 1.23.4 gofmt/build/vet/go test ./... -count=1 全绿，新增定向 verbose 与 race 通过。只新增测试，Leverage:1/CVFolds=0 生产行为不变；总体仍 **partial**，撤单 ACK 后终态/触发竞态及保护成交身份/实际手续费对账未完成，不调用真 Binance。

- P0-1/T5 永续撤单 ACK 身份核验子项：**done** — 按官方 Cancel Algo Order 契约核验原 algoId/clientAlgoId、code=200（字符串或数字）及 msg=success；HTTP 200 空对象、缺字段、身份冲突或失败码不再放行，首腿失败立即停止，不重试/不继续撤第二腿。BUY/SELL 离线响应矩阵先复现 `CancelProtective = <nil>, want success=false` 再修复；既有 stub 改为完整 ACK。验证：Go 1.23.4，gofmt/build/vet/go test ./... -count=1 全绿，CancelProtective 定向 race 通过。总体仍 **partial**：ACK 不是完整终态/零成交证明，触发竞态与完整保护成交身份/实际手续费对账待做；paper、Leverage:1/CVFolds=0 路径未改，无真交易所调用。

- P0-1 永续撤保护单全批次预检回归子项：**done** — 新增 `TestCancelProtectivePreflightPreservesFirstLeg`，对 BUY/SELL 两种平仓方向验证后续腿缺字段、身份/类型重复、状态变化、方向冲突及非法触发价；每例连续三轮只有 openAlgoOrders GET，零写入，避免坏后续腿导致先撤有效第一腿。只新增离线测试，生产代码及 Leverage:1/CVFolds=0 行为不变。
- 本轮验证：Go 1.23.4；gofmt clean、go build ./...、go vet ./...、go test ./... -count=1 全绿；新增定向 verbose 测试与全部 CancelProtective 定向 race 测试通过。P0-1/T5 总体仍 **partial**：完整保护成交身份/实际手续费自动对账未完成；本子项不证明撤单成功响应或成交竞态已核验，不调用真交易所。

- P0-1 合约保护成交疑点阻断回归子项：**done** — 新增 `TestProtectiveFillGuardPreservesLedgerAcrossRestart`，多空分别覆盖完全/部分退出、额外库存、方向反转、均价变化、NaN 数量及查询失败。每例连续三轮及从保存账本恢复后均拒绝继续，断言零行情调用/零交易所写入/零本地成交，原仓位、开仓手续费及现金不变，halt + OrderUncertain 持久化；只新增测试，不改变 Leverage:1/CVFolds=0 生产行为。
- 本轮验证：Go 1.23.4；gofmt clean、go build ./...、go vet ./...、go test ./... -count=1 全绿；新增定向 verbose 测试及 live 定向 race 测试通过。P0-1/T5 总体仍 **partial**：本轮证明 fail-safe 阻断，不是按保护单成交身份/实际手续费自动对账；不恢复已退役现货代码，未调用真交易所。

- 当前 `main` 已包含 `ffb142b` / `1c534cf`：现货已退役，保护单核验、持久化意图及人工恢复已接入永续路径。下文现货 partial/待生产接入均为历史记录，不得据此重建已删除的现货代码。
- T9 保护与恢复操作说明子项：**done** — README 修正“交易所保护单未实现”的过时表述，明确独立 MARKET 保护腿不是 OCO、停止不平仓/不撤保护单、快照时效、pending 证据保留与仅只读人工恢复。按 broker/live/session/HTTP handler 逐项核对；本轮只改文档，不改变 Leverage:1 或 CVFolds=0 的当前行为。
- 验证：Go 1.23.4；gofmt clean、go build ./...、go vet ./...、go test ./... -count=1 全绿；保护状态核验、遗留意图阻断与人工恢复不写交易所的定向离线测试通过。T9 总体 partial，配置默认值全面核对尚未完成。
- P0-1 不在本轮改为 done：历史验收与当前永续基线存在差异，完整成交身份/手续费对账仍未验收。本轮不宣称全计划完成，也未验证真交易所。

## 现状审计（已经安全的，别再动坏）

- 交易所密钥只从 env 读（`BINANCE_API_KEY` / `BINANCE_SECRET_KEY`），config 文件绝不存，代码里不打印明文。
- HMAC-SHA256 签名，cookie `HttpOnly + SameSite=Lax`，query token 用后即从 URL 清除。
- server 版绑定 0.0.0.0 时**强制 token**，匿名需 `--allow-anonymous` 显式 opt-in；桌面版绑 `127.0.0.1`。
- 实盘启动要短语「确认实盘」+ 检查交易所密钥。
- LLM 各功能 fail-open 降级；veto 有 TTL 缓存；`Leverage: 1` 与旧现货行为逐字节一致。
- Docker 有 `HEALTHCHECK`。

这些是红线，任何重构不得破坏。

## P0 — 安全（会亏钱的洞，必须最先做）

### P0-1 交易所侧保护性止损（最高优先）
- 现货撤单全批次预检子项：**done** — 撤任何止损前，先验证全部同源订单的 NEW/零成交、正有限数量/触发价及唯一 orderId/clientOrderId；后续行缺字段、非法数值或身份重复时零 DELETE，避免先撤有效保护后才发现坏数据。新增离线 15 场景拒写及双有效订单精撤正例；先复现旧代码 writes=2，再修复为零写入。Go 1.23.4 gofmt/build/vet/test 全绿，broker/live race 通过。**总体仍 partial**：保护成交按身份入账与生产接入未做；New 未启用 spotProtective，Leverage:1/CVFolds=0 历史路径未改。
- 现货撤单响应严格确认子项：**done** — CancelSpotStops 核验原 orderId/clientOrderId/symbol、SELL/STOP_LOSS、独立订单、原数量/触发价、CANCELED 与零成交；缺字段或不一致拒绝确认，防止错误响应放行本地平仓。新增离线 15 场景 broker 测试及 live 身份冲突阻断回归；修正重启/撤单 stub 为完整响应。Go 1.23.4 gofmt/build/vet/test 全绿，broker/live race 通过。**总体仍 partial**：保护成交按身份入账与生产接入未做，New 未启用 spotProtective，Leverage:1/CVFolds=0 历史路径未改。
- 现货 cycle 库存差异阻断子项：**done** — 显式安装 spotProtective 时，每周期先核验含冻结量的总库存；疑似完全/部分保护成交、额外库存、微差、非有限余额或查询失败均持久化 OrderUncertain + halt，阻断行情/策略/本地卖出及后续重试，账本不靠余额差猜成交。离线 8 场景三轮及恢复验证通过；gofmt/build/vet/test 与 broker/live race 全绿。**总体仍 partial**：这是 fail-safe 检测，不是自动成交对账；待按保护单身份核验成交价/手续费并安全入账后，再生产接入。New 尚未启用现货适配器，Leverage:1/CVFolds=0 历史路径不变。
- 现货重启保护恢复子项：**done** — 在显式安装 spotProtective 的 runner 对账路径中，以含冻结量的总库存核验净持仓，再校验/幂等补挂同源止损；空仓仅精撤本程序残留止损。数量微差、部分/完全成交、额外库存、订单不确定、查询错误或已有单字段冲突均拒绝恢复写入，不猜成交、不改账本。离线 12 场景各跑三轮，验证补挂/撤单只写一次；gofmt/build/vet/test 与 broker/live race 全绿。**总体仍 partial**：New 尚未启用现货适配器，剩 cycle 保护成交对账与生产接入；Leverage:1/CVFolds=0 现有路径不变。
- 现货净持仓适配/撤单余额护栏子项：**done** — 新增 live `spotProtective` 适配器，从 ApplyFill 后账本读取扣除 base 手续费的净数量；只允许多头，沿用本地 stop，HasSpotStops 只作存在性查询。Cancel 精撤后同时确认总持仓匹配且可用余额释放；已完全/部分成交导致 openOrders 为空、余额仍冻结、无关库存或账户查询错误均拒绝确认可平仓。离线测试覆盖扣费 1→0.999、三轮幂等只写一次、方向拒绝与撤单余额护栏；gofmt/build/vet/test 全绿，broker/live race 通过。**总体仍 partial**：适配器尚未在 New 中启用，现货生产路径保持不变（Leverage:1/CVFolds=0 不变）；先完成 cycle 保护成交对账与重启补挂再启用，避免 stop 已成交后按旧账本重复卖出。
- 现货 broker 保护原语子项：**done** — 依据官方 Spot Trade 契约新增 `PlaceSpotStop(netQuantity, stop)`：单腿 SELL STOP_LOSS（不是无效的 STOP 类型），写前查 openOrders，严格校验数量/同源触发价/NEW/零成交/独立订单，重复与冲突拒写；响应丢失仅按原 origClientOrderId 查单一次，不盲重发。`CancelSpotStops` 仅逐单撤本程序 tas- 独立止损，保留手工/异币种/非止损订单，撤单返回已有成交则要求对账，防止用旧数量再平仓。离线 TCP 断连、幂等/冲突/查询错误/撤单成交竞态/dry-run 覆盖；gofmt/build/vet/test 全绿，broker race 通过。**总体仍 partial**：本轮只完成 broker 原语，未接入 live；下一子项需净持仓数量适配、重启补挂、撤单释放冻结余额与保护成交对账。未改变 Leverage:1、CVFolds=0 现有路径。
- 保护单写入响应丢失按 ID 查单子项：**done** — 合约保护腿 POST 返回错误后，仅 GET `/fapi/v1/algoOrder?clientAlgoId=原提交ID` 一次；核验 algoId、身份、币种、类型、方向、BOTH、MARK_PRICE、closePosition、NEW 与同源触发价后才确认成功，不盲重发。离线模拟服务端已挂单但断开响应，覆盖多空与两腿，后续多轮 reconcile 无重复；查询失败/坏 JSON/字段冲突均拒绝确认。验证：gofmt/build/vet/test 全绿，broker/live race 通过。P0-1 仍 partial，剩余 spot 保护单。进程在 POST/查单之间崩溃仍依赖重启 openAlgoOrders 逐腿对账；本子项不声称新增保护单持久化意图日志。
- 挂单失败后本地退出子项：**done** — 已确认入场后的保护单挂单失败只设置持久化 halt（阻止新入场），不再把已确认成交标成 OrderUncertain；下一轮 Protect 先确认撤掉可能部分写入的保护腿，再通过 risk halt 平仓，不等止损触发。撤单失败/成交不确定仍要求对账，禁止盲目再次平仓。离线覆盖 engine 后续退出、runner 保存/恢复后退出、撤单超时不重试；engine/live race 通过，gofmt/build/vet/test 全绿。此记录替代下文历史 OrderUncertain 阻断待办；总体仍 partial，仅 spot 保护单与保护单超时按 ID 查单待完成。
- 逐腿幂等补挂子项：**done** — `PlaceProtective` 写入前查询 `openAlgoOrders`，按类型、方向、BOTH、MARK_PRICE、同源 triggerPrice 与有效 algoId 核验已有腿；只补缺失腿，冲突/重复/查询失败拒绝写入。重启有持仓时始终逐腿检查，不再把“任意一腿存在”误判为完整覆盖。离线覆盖多空、双腿/单腿/空列表、多轮幂等、部分写入失败后恢复、冲突拒绝及 runner 重启补止损。验证：gofmt/build/vet/test 全绿；broker/live race 测试通过。P0-1 总体仍 partial；spot、超时按 ID 查单及 OrderUncertain 阻断本地退出仍待完成。
- **状态：partial** — 本轮修正合约保护单的 API 契约：POST `/fapi/v1/algoOrder`（`algoType=CONDITIONAL`、两腿均用 `triggerPrice`、`clientAlgoId`）；GET `/fapi/v1/openAlgoOrders` 按 symbol 查询；只对本程序 `tap-` close-all 保护腿逐个 DELETE `/fapi/v1/algoOrder?algoId=...`，不再批量撤掉无关订单。依据 Binance 当前官方 New/Cancel/Open Algo Order 文档，离线 httptest 覆盖多空、参数、归属过滤、坏 JSON/503/缺 ID、写入失败不盲重试；gofmt/build/vet/test 全绿。尚未完成：spot 保护单；超时按 ID 查单幂等（逐腿补挂已在本轮完成，见上）；挂单失败的 OrderUncertain 会阻断后续本地 Protect，不能声称仍有本地 fail-open 保护。独立 MARKET 腿不是 OCO，LIMIT_STOP 不是已核实的 Binance 类型；不将限价单描述为极端行情保证成交。
- **问题**：开仓只有市价单，无交易所侧止损。进程崩溃/断网/systemd 重启时，持仓裸奔，本地止损失效。
- **改法**（含规划 agent 复核后的升级）：
  - `futures`：开仓成功后立刻挂保护单。基线 `STOP_MARKET`（`closePosition=true`、`stopPrice=本地止损价`、`workingType=MARK_PRICE`）；**更稳首选 `LIMIT_STOP`**（执行价可控、避市场滑点）配 **OCO**（止损+止盈同组，系统级保证至少一个生效）。本地止盈若不走 OCO 则单独挂 `TAKE_PROFIT_MARKET`（可选）。
  - `spot`：开仓后挂 `STOP`（多头止损 = SELL STOP 保护单）。
  - 平仓 / 本地止盈止损失 / `Stop()` 时：先撤交易所侧保护挂单（`DELETE /fapi/v1/openOrders` 按 symbol 精撤，避免误撤无关单 / 重复平仓）。
  - 重启 `reconcile`：若本地无持仓但交易所残留保护单，撤掉；本地有持仓但保护单缺失，补挂。**补挂前必须拉 `openOrders` 幂等去重**——已有同类保护单则跳过，不重复挂（防极端行情下挂单/网络重试导致成倍单）。
  - 保护单价格**必须与本地 risk engine 同源**：一律取 `risk.StopAndTarget` 已算出的 stopPrice，禁止另写一套价格逻辑（本地/交易所两套价会漂移导致保护失效）。
  - 极端行情：保护单若被滑点触及或撤单，需幂等重试 + 在日志/上报里报警；本地无仓但交易所仍有保护单（裸单）必须 reconcile 清掉。
- **验证**：futures_test 用 httptest stub 断言开仓后紧跟保护单（OCO/LIMIT_STOP 或 STOP_MARKET）；平仓路径先撤单；断网后 reconcile 补挂且**幂等去重**（openOrders 已有同类单则不重挂）。全离线。

### P0-2 execute 的进程级环境开关（纵深防御）
- **状态：done** — `live.New` 与 Web 会话均要求精确 `TA_ALLOW_LIVE=1`；未设置/错误值拒绝，paper 不变；确认短语、密钥及状态文件护栏继续保留。离线 constructor/API 回归测试覆盖 spot/futures 与仅显式值放行。
- **问题**：拿到访问 token + 在 UI 输入短语即可开实盘。缺进程级第二道门。
- **改法**：新增 `TA_ALLOW_LIVE=1` 环境变量；`execute=true` 时必须它为 1，否则拒绝启动并打印说明。默认 0。
- **验证**：不设变量时 `POST /api/session/start {execute:true}` 返回 400；设了才放行。

## P1 — 稳健 / 实用

### P1-3 账号隔离与实盘对账（2026-10-09 done）
- **状态：done** — 针对 `5e0cd75` 审查确认的 5 个 P1 缺陷全部修复，详见 `TASKS.md` T0。
  - 账号无密钥时不再回退部署者的 Binance 环境密钥（`Live.NoEnvKeys`）。
  - 账号账本按用户分目录并记录 owner，禁止自定义 `state_path`，拒绝恢复他人账本。
  - `/runs/` 报告按账号隔离，匿名/跨账号读取被拒。
  - 合约 cycle 先核验交易所持仓，保护单成交后持久化 halt，不按差值猜成交。
  - AI 故障不再被当成平仓信号，`LastDecision` 增加 `ok`，失败时维持原仓。
  - 附带：review 用账号自己的模型配置；CI 的 `/api/symbols` 真网调用修复；vault 写入显式 0600。
- **验证**：gofmt 空、build/vet/`go test -count=1 ./...` 19 包全绿、前端 5 项通过、双端交叉构建成功。
- **剩余风险**：`spotProtective` 生产接入仍待真交易所验证；race 检测本机缺 gcc 未跑。

### P1-1 调参多窗口交叉验证（防单窗口过拟合）
- **状态：done** — `CVFolds` / CLI `--cv` / Web `cv_folds` 已贯通；首窗口独立训练、K 个不重叠验证窗口、末尾独立 holdout。严格多数验证窗口不劣于基线且 holdout 通过才保留 winner，否则恢复基线；undefined 指标拒绝。Report/CLI 输出逐窗口摘要；CVFolds=0 不新增 JSON 字段，旧测试保持不变。验证：离线 httptest、拒绝过拟合 winner/恢复基线、零值 JSON 字节一致性、窗口边界/余数/非法值回归，以及全量 gofmt/build/vet/test。
- `tune.Options` 加 `CVFolds int`（0=关闭，向后兼容）；数据切 K 个不重叠子窗口（末尾留 holdout），基线与 winner 在每个窗口回测，**多数窗口上 objective 不低于基线**才认 winner；Report 加逐窗口摘要；CLI `--cv N` + web `cv_folds` 透传。
- **验证**：全离线 httptest；`CVFolds=0` 行为与现状逐字节一致（旧断言不改）。

### P1-2 CI（GitHub Actions）
- **状态：done（cfc7b5b）** — 新增 `.github/workflows/ci.yml`，push/PR/手动触发；Go 1.23.4 的 format/build/vet/test matrix，Windows amd64 desktop 与 Linux amd64/arm64 server 交叉构建；只读权限、action SHA 固定、禁实盘且不注入密钥。本地相同命令全部通过；fork/main 的 GitHub Actions run 37694155551 已读回 completed/success，7 个 job 全绿：https://github.com/rdone44/trading-agent-go/actions/runs/37694155551 。fork 无开放 PR，上游 PR #1 查询 404，未更新 PR 描述；未设置分支保护，因此不声称已强制阻止红灯合并。
- `.github/workflows/ci.yml`：Go 1.23，matrix（build + vet + `go test ./...`），加 desktop/server 双端 `go build`。每次 PR 强制全绿。
- **验证**：本地跑通 workflow 里同一段命令；推 PR 后看 CI 绿。

## P2 — 可维护 / 实用

### P2-1 session.go 与 webui.go 职责拆分
- session.go(596) 管实盘会话状态机，webui.go(814) 管路由+回测+调参。两者都在改 `Server`。把实盘会话收进独立 `internal/live/session` 包，webui.go 只留路由适配，消掉 `Server` 上的会话字段。
- **验证**：build + 既有 webui_test 全过；session_test 迁移。
- **状态：done** — 状态机迁入新包 `internal/live/session`（`Session`/`NewSession`/`StartOptions` + 自有 view 类型 `SessionStatus`/`CycleRecord`/`PositionView`/`RiskView`/`ExecutionView`/`TradeView`/`Settings`）。webui.go 只留路由适配（`/api/session*` 四个 handler + `Serve` 关停钩子），`Server.session` 字段改指 `*livesession.Session`，旧类型经别名（`webui.Session = livesession.Session` 等）保留，外部 `webui_test` 引用的 `webui.SessionStatus` 等不破。
  - wire format 守护：`Settings` 序列化与原 `StartSessionRequest`（含 `json:",inline"` 展开）逐字节一致，`session_wire_test.go` 用复刻的 legacy 结构体做 byte-equal 断言（全字段 + 全 nil 两组）。
  - 无新依赖、无循环 import（`live` 不 import webui）；session_test / E2E 仍全绿。

### P2-2 桌面/服务器 E2E 冒烟
- 一个 `make e2e`（或 workflow 步骤）：起 server 版（TA_ALLOW_LIVE=0、stub 行情）→ `GET /api/config` → 跑一次 backtest → 断言 200。stub LLM 走既有 httptest 模式。
- **验证**：本地跑通，零网络、零真 Binance。
- **状态：done** — `Makefile` 新增 `e2e` 目标：`go build` server 二进制到 `dist/` → `go test ./internal/webui/ -run TestServerEditionE2ESmoke`（真 TCP 监听器 `httptest.NewServer` + 真 `http.Client` 打 `/healthz`→`/api/config`→`POST /api/backtest`，离线 testfx 行情，TA_ALLOW_LIVE 未设）→ `go test ./...` 全量回归。`make e2e` 本地全绿，产物为可执行 ELF。

## 约定（每轮任务都要守）

- 不加新依赖（保持 `gopkg.in/yaml.v3` 单一非 stdlib 依赖）。
- 密钥/访问 token 只从 env 读，代码里绝不写死、不打印。
- `Leverage: 1` 时行为逐字节不变；默认 paper；实盘需 `--execute` + P0-2 的 env 开关。
- 每轮改完 `gofmt -l .` 干净 + `go build ./...` + `go vet ./...` + `go test ./...` 全绿才 commit。
- commit 一改动一件事；push 走 fork 远端；更新 PR #1 描述。

## 规划复核记录
- 2026-10-08 与规划 agent（assistant profile，A2A 9902）对齐：确认 P0-1 方向、P0>P1>P2 排序；并入 OCO/LIMIT_STOP 升级、reconcile 幂等去重、止损价同源、极端行情重试/报警 4 点风险。
