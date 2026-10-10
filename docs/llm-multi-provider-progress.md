# LLM 多提供商：进行中的工作交接

- **日期**：2026-10-10
- **状态**：设计阶段。**尚未改动任何代码**，`internal/` 下没有为这个功能写过一个字节。
- **下一步**：等你确认下面的「设计第 1、2 节」，然后出第 3、4 节。

---

## 一句话目标

给**桌面版**和**多用户账号模式**加「保存多家 AI 提供商、切换当前生效的那一家」的能力。

## 已确认的四项决策

1. **形态**：保存多家，切换当前一家。不是「按用途分配不同提供商」，也不是「多家并行投票」。
2. **生效范围**：仅桌面版（`credentials.json`）+ 账号模式（每人一个保险库）。
   **YAML 配置文件、CLI、纯服务器版完全不变**，仍然只有一套 `llm:`。
3. **添加方式**：内置常见厂商预设 + 自定义地址。
4. **采用方案 A**（见下）。

## 方案 A 要点（已批准）

- 新增包 `internal/llmprovider`：类型、字段校验、内置预设表、旧数据迁移辅助。**不含存储实现**。
- `localcreds.Creds` 与 `auth.userRecord` 各加 `LLMProviders []Provider` 和 `LLMActiveID string`。
  不新增文件，不新增密钥暴露面。
- **`config.LLM` 保持单数**：读取时把「当前生效那家」摊平进 `cfg.LLM.BaseURL/Model/APIKey`。
  因此 `internal/llm`、`internal/strategy`、`internal/tune`、`internal/live` **一行都不用改**。
- 两个版本共用同一套 handler，靠一个小接口在 `Auth` / `LocalCreds` 之间分发，页面不需要两条代码路径。

被否决的两个方案（别再提）：

- 方案 B：auth 和 localcreds 各写一份列表逻辑、前端也分两条路。预设表 / 校验 / 迁移会重复两份。
- 方案 C：独立的 `providers.json`。多一个密钥文件，0600 + 原子写 + 权限 + 按用户拆分都要重写一遍。

---

## 设计第 1 节：数据模型与迁移（已提交，**待你确认**）

新增包 `internal/llmprovider`：

```go
type Provider struct {
    ID      string `json:"id"`
    Name    string `json:"name"`     // 用户可见标签，如 "DeepSeek"
    BaseURL string `json:"base_url"`
    Model   string `json:"model"`
    APIKey  string `json:"api_key,omitempty"`
    Preset  string `json:"preset,omitempty"` // 来自哪个内置预设，可空
}
```

- **ID 用名字派生的 slug**（`deepseek`，重名则 `deepseek-2`），不用随机串。
  理由：这两个凭据文件都要能手读，`localcreds` 的包注释里已经写明这个取向。重命名保留原 ID。
- **校验**：名称必填且列表内唯一；`BaseURL` 必须 http/https 且 host 非空 —— 这段逻辑现在写在
  `internal/webui/auth.go` 的 `handleLocalCredentials` 里，移进 `llmprovider` 复用；模型名允许留空。
- **存放**：`localcreds.Creds` + `auth.userRecord` 各加 `LLMProviders []Provider`、`LLMActiveID string`。
  `LLMPrompt` 保持原位不变（人设是全局的，不按提供商分开存）。
- **迁移**（读盘时在内存里做，用户无感）：列表为空且旧字段有值时，合成一家
  `{ID:"default", Name:"默认", BaseURL/Model/APIKey 取自旧字段}` 并设为生效。
  保存时把生效那家的三个值**镜像回旧字段**，降级回旧版本仍能用。
- **摊平顺序**：有 providers 且 active 存在 → 取那家填 `cfg.LLM`；列表为空 → 完全走今天的旧逻辑
  （含环境变量回退）。账号模式继续设 `NoEnvKey = true`，不放松。

## 设计第 2 节：接口与鉴权（已提交，**待你确认**）

五个新路由，全部挂在主 mux（与 `/api/local/credentials` 同级，handler 内部解析当前用户，
账号未启用时 404），沿用现有「一个动作一个路径」和 POST-only 的风格：

| 路由 | 作用 |
|---|---|
| `GET /api/llm/providers` | 返回列表和 `active_id`；**只返回 `has_key` 布尔，永不回显密钥** |
| `POST /api/llm/providers` | 新增或修改一家（带 `id` 即修改）；`api_key` 留空表示保留原值 |
| `POST /api/llm/providers/activate` | 切换生效 |
| `POST /api/llm/providers/delete` | 删除；**正在生效的那家拒绝删除**，提示先切换 |
| `GET /api/llm/presets` | 内置预设表（OpenAI / DeepSeek / Moonshot / 通义 / SiliconFlow / OpenRouter / Ollama 等） |

另外两处调整：

- `/api/models` 增加可选 `provider_id`：带上就按该家从保管库取地址和密钥去查；
  不带就完全保持今天的行为（表单手填地址 + 密钥，未保存也能查）。
- `/api/config` 的 auth 视图（两个版本）增加 `llm_providers` 和 `llm_active`，页面加载一次拿全。
  旧的 `llm_base_url` / `llm_model` / `llm_key` 字段保留，状态行继续能用。

任何一次增删改或切换都调 `refreshRunningSessions`，运行中的会话下一轮决策即用新服务商。

## 待办（按顺序）

1. **等你确认第 1、2 节**（这是硬闸门，确认前不动代码）。
2. 出**第 3 节：设置页交互**（`internal/webui/static/index.html` 的 `#credentials-fieldset` 区域，
   以及 `app.js` 里的 `saveCredentials` / `renderCredStatus` / `loadModels`）。
3. 出**第 4 节：测试与文档**。
4. 把四节写成规格文档 `docs/superpowers/specs/2026-10-10-llm-multi-provider-design.md`。
5. 用 writing-plans 技能出实施计划，然后才动手写代码。

---

## 关键现状事实（已核实到行，避免重复调研）

- 所有 AI 功能最终只读 `config.LLM`：`internal/strategy/llm.go:85`、`internal/llm/veto.go:38,65`、
  `internal/llm/vetogate.go:39`、`internal/llm/tune.go:25`、`internal/llm/propose_prompt.go:32`、
  `internal/tune/tune.go:196`、`internal/tune/prompt.go:132`、`internal/webui/models.go:40`。
- 凭据写入的两个入口：`internal/webui/auth.go` 的 `handleAuthCredentials`（账号模式，第 312 行附近）
  与 `handleLocalCredentials`（桌面版，第 185 行附近）；两者都在保存后调 `refreshRunningSessions`。
- `refreshRunningSessions` 在 `internal/webui/webui.go:297`，内部走 `applyUserConfig(r)` + `Session.Reload`。
- `Session.Reload` 在 `internal/live/session/session.go:771`，只合并 `LLM.APIKey/BaseURL/Model/Prompt`，
  会话保留启动时的 symbol / cash / leverage / lookback 和每会话的 veto 开关。
- 路由注册在 `internal/webui/webui.go:138` 起：公开路由 → 账号路由 → `protected` 子 mux →
  `s.requireUserSession(protected)`，最后 `s.authenticate(mux)` 用令牌包住整个路由表（含 `/healthz`）。
- `localcreds` 存储：`internal/localcreds/localcreds.go`，0600 原子写（temp + rename），
  `Apply(cfg)` 在 154 行，`Status()` 只回布尔值 + 非密钥字段。
- `auth` 保险库：`internal/auth/auth.go`，`SetCredentials` 在 249 行，`userRecord` 在 74 行，
  `Status` 在 283 行附近。密码 PBKDF2-HMAC-SHA256 250,000 轮，cookie `ta_session`（24h）。
- 桌面版 URL 校验的现成代码（要复用/搬走）：`internal/webui/auth.go` 里
  `url.Parse` + scheme 必须 http/https + host 非空那段。
- `config.yaml` 里**没有** `llm:` 块，走内置默认值；YAML 永远不存密钥（`yaml:"-"`）。

## 约束与红线（别踩）

- **密钥永远不进 YAML**。`config.LLM.APIKey`、`Live.ExchangeAPIKey/SecretKey` 都是 `yaml:"-"`。
- 账号模式下 `NoEnvKey = true` 不能松：否则用户把 `BaseURL` 指向自己的端点、密钥留空，
  会把部署者的 `LLM_API_KEY` 泄露给用户控制的地址。
- 不新增第三方依赖（项目只有 `gopkg.in/yaml.v3` 一个）。
- 返回值里**永不回显密钥**，只回 `has_key` 布尔。
- 不删除正在生效的提供商。

## 本次会话已完成、与上面无关的工作

`README.md` 已重写为中文（841 行，UTF-8，无 CRLF）。后台对抗式审计查实 18 条、驳回 5 条，
逐条复核后：13 条在重写时已修好，2 条在新版里已不存在，3 条为新版真实偏差并已改正
（保护单冲突只在持仓时阻断、决策日志 500 条滚动上限、`testfx` 的 `end` 参数与工作日序列）。
**这部分已收尾，不需要再做。**
