# 安全实用重构计划

> 基于 2026-10-08 的真实代码审查（fork @ bec06fc）。每项都有风险、改法、验证。
> 优先级按「错了会亏钱」排。2h 定时任务按本文件从上往下推进，一次做一项。

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
