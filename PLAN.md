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
- **问题**：开仓只有市价单，无交易所侧止损。进程崩溃/断网/systemd 重启时，持仓裸奔，本地止损失效。
- **改法**：
  - `futures`：开仓成功后立刻挂一张 `STOP_MARKET` 保护单（带 `closePosition=true`、`stopPrice=本地止损价`、`workingType=MARK_PRICE`）；本地止盈单独挂 `TAKE_PROFIT_MARKET`（可选）。
  - `spot`：开仓后挂 `STOP`（多头止损 = SELL STOP 保护单）。
  - 平仓 / 本地止盈止盈触发 / `Stop()` 时：先撤交易所侧保护挂单（`DELETE /fapi/v1/openOrders`），避免重复平仓。
  - 重启 `reconcile`：若本地无持仓但交易所残留 STOP_MARKET，撤掉；本地有持仓但保护单缺失，补挂。
  - 保护单价格 = `risk.StopAndTarget` 已算出的 stopPrice，别新算一套。
- **验证**：futures_test 用 httptest stub 断言开仓后紧跟一张 STOP_MARKET；平仓路径先撤单；断网后 reconcile 补挂。全离线。

### P0-2 execute 的进程级环境开关（纵深防御）
- **问题**：拿到访问 token + 在 UI 输入短语即可开实盘。缺进程级第二道门。
- **改法**：新增 `TA_ALLOW_LIVE=1` 环境变量；`execute=true` 时必须它为 1，否则拒绝启动并打印说明。默认 0。
- **验证**：不设变量时 `POST /api/session/start {execute:true}` 返回 400；设了才放行。

## P1 — 稳健 / 实用

### P1-1 调参多窗口交叉验证（防单窗口过拟合）
- `tune.Options` 加 `CVFolds int`（0=关闭，向后兼容）；数据切 K 个不重叠子窗口（末尾留 holdout），基线与 winner 在每个窗口回测，**多数窗口上 objective 不低于基线**才认 winner；Report 加逐窗口摘要；CLI `--cv N` + web `cv_folds` 透传。
- **验证**：全离线 httptest；`CVFolds=0` 行为与现状逐字节一致（旧断言不改）。

### P1-2 CI（GitHub Actions）
- `.github/workflows/ci.yml`：Go 1.23，matrix（build + vet + `go test ./...`），加 desktop/server 双端 `go build`。每次 PR 强制全绿。
- **验证**：本地跑通 workflow 里同一段命令；推 PR 后看 CI 绿。

## P2 — 可维护 / 实用

### P2-1 session.go 与 webui.go 职责拆分
- session.go(596) 管实盘会话状态机，webui.go(814) 管路由+回测+调参。两者都在改 `Server`。把实盘会话收进独立 `internal/live/session` 包，webui.go 只留路由适配，消掉 `Server` 上的会话字段。
- **验证**：build + 既有 webui_test 全过；session_test 迁移。

### P2-2 桌面/服务器 E2E 冒烟
- 一个 `make e2e`（或 workflow 步骤）：起 server 版（TA_ALLOW_LIVE=0、stub 行情）→ `GET /api/config` → 跑一次 backtest → 断言 200。stub LLM 走既有 httptest 模式。
- **验证**：本地跑通，零网络、零真 Binance。

## 约定（每轮任务都要守）

- 不加新依赖（保持 `gopkg.in/yaml.v3` 单一非 stdlib 依赖）。
- 密钥/访问 token 只从 env 读，代码里绝不写死、不打印。
- `Leverage: 1` 时行为逐字节不变；默认 paper；实盘需 `--execute` + P0-2 的 env 开关。
- 每轮改完 `gofmt -l .` 干净 + `go build ./...` + `go vet ./...` + `go test ./...` 全绿才 commit。
- commit 一改动一件事；push 走 fork 远端；更新 PR #1 描述。
