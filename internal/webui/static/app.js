"use strict";

// The trading console: configure a session in the drawer, watch every decision
// cycle, and stop it. It polls /api/session rather than pushing, so a dropped
// connection costs one stale interval instead of a silent gap in the log.

// ------------------------------------------------------------------ helpers

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

const fmt = {
  money: (v) => (v == null || Number.isNaN(v) ? "—" : v.toLocaleString(undefined, { maximumFractionDigits: 2, minimumFractionDigits: 2 })),
  num: (v, digits = 2) => (v == null || Number.isNaN(v) ? "—" : v.toFixed(digits)),
  pct: (v, digits = 2) => (v == null || Number.isNaN(v) ? "—" : `${v.toFixed(digits)}%`),
  signedPct: (v, digits = 2) => (v == null || Number.isNaN(v) ? "—" : `${v >= 0 ? "+" : ""}${v.toFixed(digits)}%`),
  qty: (v) => (v == null ? "—" : v.toLocaleString(undefined, { maximumFractionDigits: 6 })),
  time: (iso) => (iso ? new Date(iso).toLocaleString() : "—"),
};

// The engine emits stable English codes; the UI shows Chinese labels.
const sideLabel = { buy: "买入", sell: "卖出" };

const reasonLabel = {
  entry_long: "开多",
  entry_short: "开空",
  signal_exit: "信号离场",
  stop_loss: "止损",
  take_profit: "止盈",
  "end of backtest": "回测结束平仓",
  hold: "持仓观望",
  flat: "空仓观望",
  startup: "启动",
};

// translateReason covers the parameterised codes ("risk halt: ...") as well as
// the fixed ones, and falls back to the raw string so nothing is ever hidden.
function translateReason(reason) {
  if (!reason) return "";
  if (reasonLabel[reason]) return reasonLabel[reason];
  if (reason.startsWith("risk halt: ")) return `风控熔断：${translateReason(reason.slice("risk halt: ".length))}`;
  if (reason.startsWith("max drawdown breached")) return "触发最大回撤限制";
  if (reason.startsWith("daily loss ")) return "单日亏损超限，暂停开仓";
  if (reason.startsWith("veto")) return `模型否决：${reason}`;
  return reason;
}

const signClass = (v) => (v == null || Number.isNaN(v) ? "" : v >= 0 ? "pos" : "neg");

async function api(path, options) {
  let response;
  try {
    response = await fetch(path, options);
  } catch (error) {
    // A bare "Failed to fetch" is what the browser says when the server is
    // gone, and it reads like a bug in the page. Say what actually happened
    // and what to do about it.
    throw new Error(offlineMessage());
  }
  const payload = await response.json().catch(() => ({ error: `HTTP ${response.status}` }));
  if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
  return payload;
}

function offlineMessage() {
  return "连接不上本地服务：程序可能已经退出，或页面还开着上一次运行的地址。请重新打开 trading-agent 桌面程序，并刷新它打开的页面。";
}

// ------------------------------------------------------------------- state

let strategies = [];
let activeTab = "cycles";
let session = null;
let pollTimer = null;
let equitySeries = [];

function strategyByName(name) {
  return strategies.find((s) => s.name === name);
}

// --------------------------------------------------------------- form logic

function renderStrategyParams() {
  const spec = strategyByName($("#strategy-select").value);
  const host = $("#strategy-params");
  $("#strategy-description").textContent = spec ? spec.description : "";
  host.innerHTML = "";
  if (!spec) return;

  for (const param of spec.params) {
    const label = document.createElement("label");
    label.className = "field";
    label.innerHTML = `
      <span>${param.label}${param.hint ? ` <span class="muted">(${param.hint})</span>` : ""}</span>
      <input type="number" step="${param.step}" value="${param.default}" data-param="${param.key}">`;
    host.appendChild(label);
  }
}

// collectRequest mirrors the fields the session start endpoint accepts. The
// UI works in percent; the engine takes fractions.
function collectRequest() {
  const form = $("#run-form");
  const params = {};
  $$("[data-param]", form).forEach((input) => {
    const value = Number(input.value);
    if (Number.isFinite(value)) params[input.dataset.param] = value;
  });
  const pct = (name) => Number($(`[name="${name}"]`).value) / 100;

  return {
    symbol: $('[name="symbol"]').value.trim().toUpperCase(),
    strategy: $('[name="strategy"]').value,
    params,
    days: Number($('[name="days"]').value) || 0,
    interval_seconds: Number($('[name="interval_seconds"]').value) || 60,
    initial_cash: Number($('[name="initial_cash"]').value) || 0,
    futures: $('[name="futures"]').checked,
    leverage: Number($('[name="leverage"]').value) || 1,
    execute: $('[name="execute"]').checked,
    confirm: $('[name="confirm"]').value,
    risk: {
      max_position_pct: pct("max_position_pct"),
      max_risk_per_trade_pct: pct("max_risk_per_trade_pct"),
      stop_loss_pct: pct("stop_loss_pct"),
      take_profit_pct: pct("take_profit_pct"),
      max_drawdown_pct: pct("max_drawdown_pct"),
      max_daily_loss_pct: pct("max_daily_loss_pct"),
      commission_bps: Number($('[name="commission_bps"]').value),
      slippage_bps: Number($('[name="slippage_bps"]').value),
      allow_short: $('[name="allow_short"]').checked,
    },
  };
}

function setStatus(message, kind) {
  const el = $("#status");
  if (!message) { el.hidden = true; return; }
  el.hidden = false;
  el.className = `status ${kind || ""}`;
  el.textContent = message;
}

// ------------------------------------------------------------------ drawer

function setDrawer(open) {
  const drawer = $("#config-drawer");
  drawer.classList.toggle("open", open);
  drawer.setAttribute("aria-hidden", String(!open));
  $("#config-toggle").setAttribute("aria-expanded", String(open));
  $("#drawer-backdrop").hidden = !open;
  // Opening the form is the user's answer to whatever the banner complained
  // about, so clear it instead of leaving a stale error over the inputs.
  if (open && !(session && session.running)) setStatus("");
  if (open) {
    const first = $('[name="symbol"]');
    if (first) first.focus({ preventScroll: true });
  }
}

const drawerOpen = () => $("#config-drawer").classList.contains("open");

// ------------------------------------------------------------------ render

function table(headers, rows, emptyMessage) {
  if (!rows.length) return `<p class="empty-note" style="padding:16px">${emptyMessage}</p>`;
  const head = headers.map((h) => `<th class="${h.num ? "num" : ""}">${h.label}</th>`).join("");
  const body = rows.map((cells) => `<tr>${cells.join("")}</tr>`).join("");
  return `<div class="scroll"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}

// The equity sparkline is the only chart: it answers "which way is it going?"
// at a glance without competing with the numbers for attention.
function renderSpark(series) {
  const host = $("#hero-spark");
  if (series.length < 2) {
    host.innerHTML = '<p class="empty-note">还没有足够的数据点。</p>';
    return;
  }
  const w = 300, h = 78, pad = 3;
  const min = Math.min(...series);
  const max = Math.max(...series);
  const span = max - min || Math.max(Math.abs(max), 1) * 0.002;
  const x = (i) => pad + (i / (series.length - 1)) * (w - pad * 2);
  const y = (v) => h - pad - ((v - min) / span) * (h - pad * 2);

  const line = series.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(" ");
  const area = `${line} L${x(series.length - 1).toFixed(1)} ${h - pad} L${x(0).toFixed(1)} ${h - pad} Z`;
  const rising = series[series.length - 1] >= series[0];
  const stroke = rising ? "var(--up)" : "var(--down)";
  const id = rising ? "sparkUp" : "sparkDown";

  host.innerHTML = `
    <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="权益走势">
      <defs>
        <linearGradient id="${id}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${stroke}" stop-opacity=".28"/>
          <stop offset="100%" stop-color="${stroke}" stop-opacity="0"/>
        </linearGradient>
      </defs>
      <path d="${area}" fill="url(#${id})"/>
      <path d="${line}" fill="none" stroke="${stroke}" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round"/>
      <circle cx="${x(series.length - 1).toFixed(1)}" cy="${y(series[series.length - 1]).toFixed(1)}" r="2.6" fill="${stroke}"/>
    </svg>`;
}

function renderHero(s) {
  $("#session-title").textContent = `${s.symbol} · ${s.strategy}`;
  $("#hero-equity").textContent = fmt.money(s.equity);
  $("#hero-initial").textContent = fmt.money(s.initial_cash);

  const ret = $("#hero-return");
  ret.textContent = fmt.signedPct(s.total_return_pct);
  ret.className = signClass(s.total_return_pct);

  const venue = s.venue === "futures" ? `合约 ${s.leverage || 1}x` : "现货";
  const state = s.running ? `运行中 · 已 ${s.cycles} 个周期` : `已停止 · 共 ${s.cycles} 个周期`;
  $("#session-sub").textContent = `${venue} · 每 ${s.interval_seconds}s 轮询 · ${state}`;
}

function renderStats(s) {
  const cards = [
    { k: "可用现金", v: fmt.money(s.cash), d: "未占用部分" },
    { k: "峰值权益", v: fmt.money(s.peak_equity), d: "历史最高" },
    { k: "最新价", v: fmt.money(s.last_price), d: s.symbol },
    { k: "运行周期", v: String(s.cycles ?? 0), d: `间隔 ${s.interval_seconds}s` },
    { k: "最近动作", v: translateReason(s.last_action) || "—", d: fmt.time(s.last_tick) },
  ];
  $("#session-cards").innerHTML = cards
    .map((c) => `<div class="stat"><div class="k">${c.k}</div><div class="v ${c.c || ""}">${c.v}</div><div class="d">${c.d}</div></div>`)
    .join("");
}

function renderPosition(p) {
  const host = $("#position-body");
  const note = $("#position-note");
  if (!p || !p.open) {
    note.textContent = "";
    host.innerHTML = '<p class="empty-note">当前空仓。引擎会在策略给出目标仓位时开仓。</p>';
    return;
  }

  note.innerHTML = `<span class="pill ${p.side === "buy" ? "long" : "short"}">${sideLabel[p.side] || p.side}</span>`;
  const fields = [
    { k: "数量", v: fmt.qty(p.quantity) },
    { k: "开仓价", v: fmt.money(p.entry_price) },
    { k: "最新价", v: fmt.money(p.mark_price) },
    { k: "浮动盈亏", v: `<span class="${signClass(p.unrealized)}">${fmt.money(p.unrealized)}</span>` },
    { k: "止损价", v: p.stop_price ? fmt.money(p.stop_price) : "未设置" },
    { k: "止盈价", v: p.target_price ? fmt.money(p.target_price) : "未设置" },
    { k: "浮动收益率", v: `<span class="${signClass(p.return_pct)}">${fmt.signedPct(p.return_pct)}</span>` },
    { k: "开仓时间", v: fmt.time(p.opened_at), wide: true },
  ];
  host.innerHTML = `<div class="kv">${fields
    .map((f) => `<div><span class="k">${f.k}</span><span class="v ${f.wide ? "wide" : ""}">${f.v}</span></div>`)
    .join("")}</div>`;
}

function renderCycles(log) {
  // Newest first: the interesting row is always at the top.
  const rows = [...log].reverse().map((c) => {
    const action = c.error
      ? `<span class="pill stop">错误</span>`
      : `<span class="pill ${c.action === "hold" || c.action === "flat" ? "" : "long"}">${translateReason(c.action) || c.action}</span>`;
    return [
      `<td class="nowrap">${c.time}</td>`,
      `<td class="tag">${action}</td>`,
      `<td class="num">${c.price ? fmt.money(c.price) : "—"}</td>`,
      `<td class="num">${fmt.money(c.equity)}</td>`,
      `<td class="num">${fmt.money(c.cash)}</td>`,
      `<td class="tag">${c.position || "空仓"}</td>`,
      `<td class="tag">${c.error ? `<span class="muted">${c.error}</span>` : ""}</td>`,
    ];
  });
  return table(
    [{ label: "时间" }, { label: "动作" }, { label: "价格", num: true }, { label: "权益", num: true },
     { label: "现金", num: true }, { label: "持仓" }, { label: "说明" }],
    rows,
    "还没有运行记录。点击「开始交易」或「单步」。"
  );
}

function renderTrades(trades) {
  const rows = trades.map((t) => [
    `<td>${t.entry_time}</td>`,
    `<td>${t.exit_time}</td>`,
    `<td class="tag"><span class="pill ${t.side === "buy" ? "long" : "short"}">${sideLabel[t.side] || t.side}</span></td>`,
    `<td class="num">${fmt.qty(t.quantity)}</td>`,
    `<td class="num">${fmt.money(t.entry_price)}</td>`,
    `<td class="num">${fmt.money(t.exit_price)}</td>`,
    `<td class="num ${signClass(t.pnl)}">${fmt.money(t.pnl)}</td>`,
    `<td class="num ${signClass(t.return_pct)}">${fmt.signedPct(t.return_pct)}</td>`,
    `<td class="tag"><span class="pill ${String(t.reason).includes("stop") ? "stop" : ""}">${translateReason(t.reason)}</span></td>`,
  ]);
  return table(
    [{ label: "开仓时间" }, { label: "平仓时间" }, { label: "方向" }, { label: "数量", num: true },
     { label: "开仓价", num: true }, { label: "平仓价", num: true }, { label: "盈亏", num: true },
     { label: "收益率", num: true }, { label: "离场原因" }],
    rows,
    "本次会话还没有已平仓的交易。"
  );
}

function renderTab() {
  if (!session) return;
  const body = $("#tab-body");
  if (activeTab === "cycles") body.innerHTML = renderCycles(session.log || []);
  if (activeTab === "trades") body.innerHTML = renderTrades(session.trades || []);
  $$(".tab").forEach((tab) => tab.classList.toggle("active", tab.dataset.tab === activeTab));
}

function renderSession(s) {
  session = s;
  const started = s.cycles > 0 || s.running;
  $("#empty-state").hidden = started;
  $("#session").hidden = !started;
  // Reaching this point means the server answered, so a previous "cannot
  // reach the server" banner is stale and must not linger.
  setServerDown(false);
  if (!started) return;

  // Keep one equity point per recorded cycle so the sparkline is a real
  // history rather than a redraw of the same number.
  equitySeries = (s.log || []).map((c) => c.equity).filter((v) => Number.isFinite(v) && v > 0);

  const mode = $("#session-mode");
  mode.textContent = s.mode === "live" ? "实盘" : "纸面";
  mode.className = `badge ${s.mode === "live" ? "live" : "paper"}`;

  const state = $("#session-state");
  state.textContent = s.running ? "运行中" : "已停止";
  state.className = `badge ${s.running ? "running" : "stopped"}`;

  const dot = $("#status-dot");
  const topState = $("#topbar-state");
  if (s.last_error) {
    dot.className = "dot error";
    topState.textContent = `周期出错：${s.last_error}`;
  } else if (s.running) {
    dot.className = "dot live";
    topState.textContent = `运行中 · ${s.symbol} · ${fmt.money(s.last_price)}`;
  } else {
    dot.className = "dot idle";
    topState.textContent = s.cycles > 0 ? `已停止 · 共 ${s.cycles} 个周期` : "待机";
  }

  renderHero(s);
  renderSpark(equitySeries);
  renderStats(s);
  renderPosition(s.position);

  $("#cycles-count").textContent = (s.log || []).length;
  $("#trades-count").textContent = (s.trades || []).length;

  if (s.last_error) setStatus(`最近一次周期出错：${s.last_error}`, "error");
  else if (s.running) setStatus("");

  renderTab();
}

// ---------------------------------------------------------------- session

async function refreshSession() {
  try {
    const status = await api("/api/session");
    renderSession(status);
    setRunningUi(status.running);
    // Poll fast while a loop is live, slowly when idle so an open tab does not
    // hammer the server for a session that is not running.
    schedulePoll(status.running ? 3000 : 15000);
  } catch (error) {
    setServerDown(true);
    setStatus(error.message, "error");
    // Back off while the server is unreachable: retrying every 15s forever
    // just fills the console with network errors.
    schedulePoll(30000);
  }
}

// serverDown remembers the last connection state so the banner is only written
// when it actually changes, instead of being re-rendered on every poll.
let serverDown = false;
function setServerDown(down) {
  if (down === serverDown) return;
  serverDown = down;
  if (down) setStatus(offlineMessage(), "error");
  else setStatus("");
}

function schedulePoll(delay) {
  if (pollTimer) clearTimeout(pollTimer);
  pollTimer = setTimeout(refreshSession, delay);
}

function setRunningUi(running) {
  $("#run-button").disabled = running;
  $("#run-button").textContent = running ? "交易中…" : "开始交易";
  $("#drawer-run").disabled = running;
  $("#drawer-run").textContent = running ? "交易中…" : "开始交易";
  $("#step-button").disabled = !running;
  $("#stop-button").disabled = !running;
}

// ---------------------------------------------------------------- bootstrap

async function bootstrap() {
  const config = await api("/api/config");
  strategies = config.strategies || [];

  $("#strategy-select").innerHTML = strategies
    .map((s) => `<option value="${s.name}">${s.title}</option>`)
    .join("");

  // Seed the form from the effective server config.
  $('[name="symbol"]').value = config.symbol;
  $('[name="days"]').value = config.days;
  $('[name="initial_cash"]').value = config.initial_cash;
  $("#strategy-select").value = config.strategy;
  $("#config-source").textContent = config.output_dir ? `报告目录 → ${config.output_dir}` : "本地配置";

  // The desktop edition can stop itself; the server edition must not expose
  // that control, so the button only appears when the backend says so.
  if (config.desktop) {
    const quit = $("#quit-button");
    quit.hidden = false;
    quit.addEventListener("click", async () => {
      quit.disabled = true;
      quit.textContent = "退出中…";
      try {
        await api("/api/shutdown", { method: "POST" });
      } catch (error) {
        // The connection usually drops as the server exits, which is expected.
      }
      document.body.innerHTML =
        '<div class="bye"><h1>已退出</h1><p>服务已停止，可以关闭此页面。</p></div>';
    });
  }

  const risk = config.risk || {};
  const toPct = (v) => (v == null ? "" : (v * 100).toFixed(2).replace(/\.?0+$/, ""));
  if (risk.max_position_pct != null) $('[name="max_position_pct"]').value = toPct(risk.max_position_pct);
  if (risk.max_risk_per_trade_pct != null) $('[name="max_risk_per_trade_pct"]').value = toPct(risk.max_risk_per_trade_pct);
  if (risk.stop_loss_pct != null) $('[name="stop_loss_pct"]').value = toPct(risk.stop_loss_pct);
  if (risk.take_profit_pct != null) $('[name="take_profit_pct"]').value = toPct(risk.take_profit_pct);
  if (risk.max_drawdown_pct != null) $('[name="max_drawdown_pct"]').value = toPct(risk.max_drawdown_pct);
  if (risk.max_daily_loss_pct != null) $('[name="max_daily_loss_pct"]').value = toPct(risk.max_daily_loss_pct);
  if (risk.commission_bps != null) $('[name="commission_bps"]').value = risk.commission_bps;
  if (risk.slippage_bps != null) $('[name="slippage_bps"]').value = risk.slippage_bps;
  $('[name="allow_short"]').checked = Boolean(risk.allow_short);

  renderStrategyParams();
  await refreshSession();
}

$("#strategy-select").addEventListener("change", renderStrategyParams);

// Leverage only exists on a perpetual venue, so the field follows the venue
// toggle instead of being a permanently visible no-op.
$('[name="futures"]').addEventListener("change", (event) => {
  $("#leverage-field").hidden = !event.target.checked;
  if (!event.target.checked) $('[name="leverage"]').value = 1;
});

// Arming real orders reveals the confirmation box and is deliberately noisy:
// this is the only control in the UI that can move real money.
$('[name="execute"]').addEventListener("change", (event) => {
  $("#live-confirm").hidden = !event.target.checked;
  if (!event.target.checked) $('[name="confirm"]').value = "";
});

// -------------------------------------------------------------- drawer wiring

$("#config-toggle").addEventListener("click", () => setDrawer(!drawerOpen()));
$("#drawer-close").addEventListener("click", () => setDrawer(false));
$("#drawer-backdrop").addEventListener("click", () => setDrawer(false));
$("#onboarding-open").addEventListener("click", () => setDrawer(true));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") setDrawer(false);
});

$$(".tab").forEach((tab) => {
  tab.addEventListener("click", () => { activeTab = tab.dataset.tab; renderTab(); });
});

$("#run-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  setRunningUi(true);
  setStatus("正在初始化交易会话：加载行情、恢复状态、校验持仓…", "running");
  try {
    const payload = await api("/api/session/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(collectRequest()),
    });
    setDrawer(false);
    renderSession(payload);
    setStatus("");
    schedulePoll(3000);
  } catch (error) {
    setStatus(error.message, "error");
    setRunningUi(false);
  }
});

$("#step-button").addEventListener("click", async () => {
  $("#step-button").disabled = true;
  try {
    const payload = await api("/api/session/step", { method: "POST" });
    renderSession(payload);
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    $("#step-button").disabled = !session || !session.running;
  }
});

$("#stop-button").addEventListener("click", async () => {
  $("#stop-button").disabled = true;
  setStatus("正在停止并保存会话状态…", "running");
  try {
    const payload = await api("/api/session/stop", { method: "POST" });
    renderSession(payload);
    setStatus("");
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    setRunningUi(false);
  }
});

// First paint: the workspace sections rise in sequence instead of snapping.
requestAnimationFrame(() => {
  ["#empty-state", "#session"].forEach((selector, i) => {
    const el = $(selector);
    if (!el) return;
    el.classList.add("reveal");
    el.style.animationDelay = `${i * 60}ms`;
  });
});

bootstrap().catch((error) => setStatus(`无法加载配置：${error.message}`, "error"));
