"use strict";

// The trading terminal: configure a session in the left rail, watch the board,
// stop it. It polls /api/session rather than pushing, so a dropped connection
// costs one stale interval instead of a silent gap in the log.

// ------------------------------------------------------------------ helpers

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

const ok = (v) => typeof v === "number" && Number.isFinite(v);

const fmt = {
  money: (v) => (ok(v) ? v.toLocaleString(undefined, { maximumFractionDigits: 2, minimumFractionDigits: 2 }) : "—"),
  signedMoney: (v) => (ok(v) ? `${v >= 0 ? "+" : "-"}${Math.abs(v).toLocaleString(undefined, { maximumFractionDigits: 2, minimumFractionDigits: 2 })}` : "—"),
  pct: (v, digits = 2) => (ok(v) ? `${v.toFixed(digits)}%` : "—"),
  signedPct: (v, digits = 2) => (ok(v) ? `${v >= 0 ? "+" : ""}${v.toFixed(digits)}%` : "—"),
  qty: (v) => (ok(v) ? v.toLocaleString(undefined, { maximumFractionDigits: 6 }) : "—"),
  clock: (iso) => (iso ? new Date(iso).toLocaleTimeString() : ""),
  stamp: (iso) => (iso ? new Date(iso).toLocaleString() : "—"),
};

// The engine emits stable English codes; the UI shows Chinese labels.
const sideLabel = { buy: "买入", sell: "卖出" };

const reasonLabel = {
  entry_long: "开多",
  entry_short: "开空",
  signal_exit: "信号离场",
  stop_loss: "止损",
  take_profit: "止盈",
  ai_unavailable: "AI 不可用 · 维持原仓",
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

const signClass = (v) => (ok(v) ? (v >= 0 ? "pos" : "neg") : "");

// escape keeps server-supplied strings (errors, reasons) out of the HTML
// grammar: they are rendered as text, never parsed as markup.
function escape(value) {
  return String(value == null ? "" : value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

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
  if (!response.ok) {
    if (response.status === 401) {
      // Auth is enabled and the session has expired or is missing. Tag the
      // error so the caller can fall back to the login card instead of the
      // "server is gone" banner.
      const error = new Error(payload.error || "需要登录");
      error.status = 401;
      throw error;
    }
    throw new Error(payload.error || `HTTP ${response.status}`);
  }
  return payload;
}

function offlineMessage() {
  return "连接不上本地服务：程序可能已经退出，或页面还开着上一次运行的地址。请重新打开 trading-agent 桌面程序，并刷新它打开的页面。";
}

// ------------------------------------------------------------------- state

let strategies = [];
let activeTab = "orders";
let session = null;
let pollTimer = null;
let market = null;
let marketTimer = null;
let marketInterval = "1h";
let formIdentity = "";
let busy = false;
let closing = false;
let authEnabled = false;
// localSettings is the desktop single-user mode: the credential form is
// editable without a login and the live switch lives in the same file.
let localSettings = false;
let authMode = "login";
let lastAuthState = null;
let liveGate = false;
// liveGateEnv records whether the gate came from TA_ALLOW_LIVE=1, so the
// desktop switch can explain that the environment overrides the file.
let liveGateEnv = false;
// modelsLoaded guards the one automatic /api/models call per page load: a
// stored key should populate the model picker without the user asking, but a
// repeated config refresh must not re-query the provider.
let modelsLoaded = false;
let lastConfig = null;
let accountPending = false;
let ledgerMode = null;

const strategyByName = (name) => strategies.find((s) => s.name === name);

// --------------------------------------------------------------- form logic

function renderStrategyParams(values = {}) {
  const spec = strategyByName($("#strategy-select").value);
  const host = $("#strategy-params");
  $("#strategy-description").textContent = spec ? spec.description : "";
  host.innerHTML = "";
  if (!spec) return;

  for (const param of spec.params) {
    const label = document.createElement("label");
    label.className = "field";
    label.innerHTML = `
      <span>${escape(param.label)}</span>
      <input type="number" step="${escape(param.step)}" min="${param.min ?? 0}" ${param.max ? `max="${param.max}"` : ""} value="${escape(values[param.key] ?? param.default)}" data-param="${escape(param.key)}">`;
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
  const pct = (name) => { const value = $(`[name="${name}"]`).value; return value === "" ? null : Number(value) / 100; };

  return {
    symbol: $('[name="symbol"]').value.trim().toUpperCase(),
    strategy: $('[name="strategy"]').value,
    params,
    days: Number($('[name="days"]').value) || 0,
    interval_seconds: Number($('[name="interval_seconds"]').value) || 60,
    initial_cash: Number($('[name="initial_cash"]').value) || 0,
    leverage: Number($('[name="leverage"]').value) || 1,
    // The entry veto lives on the AI page but belongs to the session request.
    veto: Boolean($("#ai-veto")?.checked),
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

// --------------------------------------------------------------- rail (narrow)

// On a wide screen the rail is part of the grid and these functions are inert;
// below 1000px it becomes an overlay and needs explicit open/close.
function setRail(open) {
  const rail = $("#rail");
  rail.classList.toggle("open", open);
  rail.inert = !open;
  rail.setAttribute("aria-hidden", String(!open));
  $("#rail-toggle").setAttribute("aria-expanded", String(open));
  $("#rail-backdrop").hidden = !open;
  if (open) {
    const first = $("#rail-close");
    if (first) first.focus({ preventScroll: true });
  } else $("#rail-toggle").focus({ preventScroll: true });
}

const railOpen = () => $("#rail").classList.contains("open");

// loadAllSymbols pulls the venue's whole tradable pair list (liquidity-ranked)
// and offers it as the datalist behind the 交易对 field. This is the "get me
// all the coins, don't make me type a ticker I might not know" path: pick a
// venue, press 全部币种, choose from the market. Manual typing still works.
async function loadAllSymbols() {
  const venue = "futures"; // perpetual-only terminal
  const button = $("#load-symbols");
  if (button) { button.disabled = true; button.textContent = "拉取中…"; }
  try {
    const data = await api(`/api/symbols?venue=${venue}`);
    const options = document.getElementById("symbol-options");
    const rows = data.symbols || [];
    options.innerHTML = rows
      .map((s) => `<option value="${escape(s.symbol)}">${escape(s.symbol)} · 24h ${fmt.money(s.volume_24h)}</option>`)
      .join("");
    // Offer the list to the user: focus the field so the datalist pops open.
    const input = $('[name="symbol"]');
    if (input && rows.length) input.focus();
  } catch (error) {
    setStatus(`无法加载币种列表：${error.message}`, "error");
  } finally {
    if (button) { button.disabled = false; button.textContent = "全部币种"; }
  }
}


function applySettings(settings) {
  for (const name of ["symbol", "days", "interval_seconds", "initial_cash", "leverage", "strategy"]) {
    const input = $(`[name="${name}"]`);
    if (settings[name] != null) input.value = settings[name];
  }
  for (const name of ["execute"]) $(`[name="${name}"]`).checked = Boolean(settings[name]);
  for (const [name, value] of Object.entries(settings.risk || {})) {
    const input = $(`[name="${name}"]`); if (!input) continue;
    if (input.type === "checkbox") input.checked = Boolean(value);
    else input.value = value == null ? "" : name.endsWith("_pct") ? Number((value*100).toFixed(4)) : value;
  }
  $("#live-confirm").hidden = !settings.execute;
  if (settings.veto != null) $("#ai-veto").checked = Boolean(settings.veto);
  renderKeyHint();
  $('[name="confirm"]').value = "";
  renderStrategyParams(settings.params || {});
}

async function refreshMarket() {
  clearTimeout(marketTimer);
  if (closing) return;
  const symbol = session?.running ? session.symbol : $('[name="symbol"]').value;
  const venue = "futures";
  const key = `${symbol}/${venue}/${marketInterval}`;
  $("#market-state").textContent = "更新行情中";
  try {
    const next = await api(`/api/market?symbol=${encodeURIComponent(symbol)}&venue=${venue}&interval=${marketInterval}`);
    const currentSymbol = session?.running ? session.symbol : $('[name="symbol"]').value;
    if (symbol !== currentSymbol || next.interval !== marketInterval) return;
    market = next;
    renderMarket();
    marketTimer = setTimeout(refreshMarket, 15000);
  } catch (error) {
    $("#market-state").textContent = market ? "行情过期 · 重连中" : "行情连接失败";
    $("#market-state").className = "market-state neg";
    if (!market || `${market.symbol}/${market.venue}/${market.interval}` !== key) {
      $("#market-chart").innerHTML = `<div class="market-empty">${escape(error.message)}<br>行情暂不可用，不影响查看已保存的会话状态。</div>`;
    }
    marketTimer = setTimeout(refreshMarket, 30000);
  }
}

function renderMarket() {
  if (!market) return;
  $("#ticker-symbol").textContent = market.symbol;
  $("#ticker-price").textContent = fmt.money(market.price);
  $("#ticker-delta").textContent = `${fmt.signedPct(market.change_pct)} · 24时`;
  $("#ticker-delta").className = `quote-delta ${signClass(market.change_pct)}`;
  $("#ticker-time").textContent = fmt.clock(market.updated_at);
  $("#market-symbol").textContent = market.symbol.replace(/(USDT|USDC)$/, " / $1");
  $("#market-venue").textContent = "USDT 永续";
  $("#market-state").textContent = "行情已连接";
  $("#market-state").className = "market-state";
  $("#market-update").textContent = `更新 ${fmt.clock(market.updated_at)}`;
  $("#market-stats").innerHTML = [
    { k: "24时涨跌", v: fmt.signedPct(market.change_pct), c: signClass(market.change_pct) },
    { k: "24时最高", v: fmt.money(market.high) },
    { k: "24时最低", v: fmt.money(market.low) },
    { k: "24时成交额", v: `${(market.volume/1e6).toFixed(2)} M` },
  ].map(x => `<div class="market-stat"><span class="k">${x.k}</span><span class="v ${x.c || ""}">${x.v}</span></div>`).join("");
  renderCandles();
}

function renderCandles() {
  const candles = (market?.candles || []).filter(c => [c.open,c.high,c.low,c.close].every(ok));
  if (!candles.length) return;
  const w = 860, h = 350, right = 72, top = 18, bottom = 42;
  const floor = Math.min(...candles.map(c => c.low)), ceiling = Math.max(...candles.map(c => c.high));
  const pad = (ceiling-floor || ceiling*.005)*.08;
  const lo = floor-pad, hi = ceiling+pad;
  const y = v => top+(hi-v)/(hi-lo)*(h-top-bottom);
  const stride = (w-right-20)/candles.length;
  const x = i => 12+(i+.5)*stride;
  let svg = "";
  for (let i=0; i<5; i++) {
    const value = lo+(hi-lo)*i/4, yy = y(value);
    svg += `<line x1="0" y1="${yy}" x2="${w-right}" y2="${yy}" stroke="var(--line-soft)"/><text x="${w-right+8}" y="${yy+3}" fill="var(--ink-3)" font-size="10">${fmt.money(value)}</text>`;
  }
  candles.forEach((c,i) => {
    const color = c.close>=c.open ? "var(--up)" : "var(--down)";
    svg += `<g><title>${escape(fmt.stamp(c.time))} 开 ${fmt.money(c.open)} 高 ${fmt.money(c.high)} 低 ${fmt.money(c.low)} 收 ${fmt.money(c.close)}</title><line x1="${x(i)}" y1="${y(c.high)}" x2="${x(i)}" y2="${y(c.low)}" stroke="${color}"/><rect x="${x(i)-stride*.28}" y="${Math.min(y(c.open),y(c.close))}" width="${stride*.56}" height="${Math.max(1,Math.abs(y(c.open)-y(c.close)))}" fill="${color}"/></g>`;
  });
  for (let i=0; i<4; i++) {
    const index = Math.round(i*(candles.length-1)/3);
    const label = new Date(candles[index].time).toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit" });
    svg += `<text x="${x(index)}" y="${h-13}" text-anchor="middle" fill="var(--ink-3)" font-size="10">${label}</text>`;
  }
  const lastY = Math.min(h-bottom, Math.max(top,y(market.price)));
  svg += `<line x1="0" y1="${lastY}" x2="${w-right}" y2="${lastY}" stroke="var(--accent)" stroke-dasharray="4 4" opacity=".6"/>`;
  $("#market-chart").innerHTML = `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="${escape(market.symbol)} ${market.interval} K线图">${svg}</svg>`;
}

function renderOrders(orders) {
  const labels = { filled: "已成交", partial: "部分成交", rejected: "已拒绝", unknown: "待核对" };
  return table(["时间", "方向", "成交数量", "成交均价", "手续费", "状态", "订单编号"], [...orders].reverse().map(o => [
    `<td>${escape(fmt.clock(o.time))}</td>`, `<td class="tag">${escape(sideLabel[o.side] || o.side)}</td>`,
    `<td class="num">${fmt.qty(o.quantity)}</td>`, `<td class="num">${fmt.money(o.price)}</td>`, `<td class="num">${fmt.money(o.fee)}</td>`,
    `<td class="tag" title="${escape(o.reason)}"><span class="pill ${o.uncertain || o.status === "rejected" ? "stop" : "long"}">${o.uncertain ? "待核对" : labels[o.status] || o.status || "已成交"}</span></td>`,
    `<td class="num">${escape(o.order_id || o.client_order_id || "纸面成交")}</td>`]), "暂无订单。策略启动后，开仓、平仓及拒单会显示在这里。");
}

// ------------------------------------------------------------------ render

function table(headers, rows, emptyMessage) {
  if (!rows.length) return `<p class="empty" style="padding:14px">${escape(emptyMessage)}</p>`;
  const head = headers.map((h) => `<th>${escape(h)}</th>`).join("");
  const body = rows.map((cells) => `<tr>${cells.join("")}</tr>`).join("");
  return `<table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table>`;
}

// ---------------------------------------------------------------- top bar

function renderTopbar(s) {
  const started = s.cycles > 0 || s.running;

  if (!market) {
    $("#ticker-symbol").textContent = started ? s.symbol : $('[name="symbol"]').value;
    $("#ticker-price").textContent = s.last_price > 0 ? fmt.money(s.last_price) : "—";
  }
  const mode = $("#mode-badge");
  mode.textContent = s.mode === "live" ? "实盘" : "纸面";
  mode.className = `mode ${s.mode === "live" ? "live" : ""}`;

  const dot = $("#status-dot");
  const state = $("#run-state");
  if (s.starting) {
    dot.className = "dot live";
    state.textContent = "初始化中";
  } else if (s.recovering) {
    dot.className = "dot live";
    state.textContent = "保护单核验中";
  } else if (s.last_error) {
    dot.className = "dot error";
    state.textContent = "周期出错";
  } else if (s.running) {
    dot.className = "dot live";
    state.textContent = `运行中 ${s.cycles}`;
  } else {
    dot.className = "dot";
    state.textContent = s.cycles > 0 ? `已停止 ${s.cycles}` : "待机";
  }
  const open = s.position?.open;
  const protection = s.protection?.state || "unknown";
  $("#recovery-button").hidden = !s.recovery_required;
  $("#recovery-button").disabled = Boolean(s.recovering || busy);
  $("#recovery-button").textContent = s.recovering ? "保护单核验中…" : "人工核验恢复";
  const warning = s.risk?.order_uncertain || (open && s.mode === "live" && protection !== "verified") || s.mode === "live";
  $("#safety-strip").classList.toggle("warn", Boolean(warning));
  $("#safety-text").textContent = s.recovering ? "正在只读核验 Binance 持仓、USDT 钱包和原始保护单 ID · 不会发送订单"
    : s.recovery_required ? "发现遗留保护单意图 · 自动交易已锁定，请先在 Binance 人工核对后执行恢复"
    : s.risk?.order_uncertain ? "订单状态待核对 · 自动交易已锁定，请在交易所核对成交与持仓"
    : open && s.mode === "live" && protection !== "verified" ? `交易所保护单${protection === "partial" ? "不完整" : protection === "missing" ? "缺失" : "未核验"} · 禁止新开仓，请人工核对`
    : open && s.mode === "live" && !s.running ? "策略已停止，持仓仍在 · 保护单上次核验不代表当前仍有效，请在 Binance 核对"
    : open && s.mode === "live" ? `交易所保护单已核验 · ${s.protection?.checked_at ? new Date(s.protection.checked_at).toLocaleString("zh-CN") : "时间未知"}，请持续核对`
    : s.mode === "live" ? "实盘资金 · 请持续核对 Binance 持仓、保护单和本地账本"
    : s.running ? "纸面策略运行中 · 使用真实 Binance 行情，成交仅本地模拟"
    : "纸面模式 · 可独立查看行情，在策略与风控页核对参数";

  renderAI(s);
}

// renderAI gives the model a visible face. While the LLM strategy is cycling
// it shows the model name; once the model explains a call, its own words take
// over the strip. Non-LLM strategies never touch this strip — the AI is only
// on stage when it is actually deciding.
function renderAI(s) {
  const strip = $("#ai-strip");
  const text = $("#ai-text");
  const model = (s.ai_model || "").trim();
  const reason = (s.ai_reason || "").trim();
  const active = s.cycles > 0;

  if (reason) {
    strip.hidden = false;
    strip.classList.remove("thinking");
    const tag = model ? `${model} · ` : "";
    text.textContent = `AI 决策 · ${tag}${reason}`;
    text.title = reason;
    return;
  }
  if (active && s.strategy === "llm" && model) {
    strip.hidden = false;
    strip.classList.add("thinking");
    text.textContent = `AI 思考中 · ${model}`;
    text.title = model;
    return;
  }
  strip.hidden = true;
  strip.classList.remove("thinking");
}

// ---------------------------------------------------------------- account

function renderAccount(s) {
  const started = s.cycles > 0 || s.running;
  const venue = `USDT 永续 · ${s.leverage || 1}x`;
  if (ledgerMode !== s.mode) {
    $("#session-ledger").open = s.mode === "live";
    ledgerMode = s.mode;
  }

  $("#account-note").textContent = started
    ? `${s.mode === "live" ? "交易账本" : "纸面资金"} · ${venue}`
    : "尚未启动";

  if (!started) {
    $("#equity-value").textContent = fmt.money(Number($('[name="initial_cash"]').value) || 0);
    $("#equity-value").className = "big muted";
    $("#equity-return").textContent = "—";
    $("#equity-return").className = "";
    $("#equity-abs").textContent = "待启动";
    $("#equity-initial").textContent = "—";
  } else {
    const pnl = ok(s.equity) && ok(s.initial_cash) ? s.equity - s.initial_cash : NaN;
    $("#equity-value").textContent = fmt.money(s.equity);
    $("#equity-value").className = "big";
    $("#equity-return").textContent = fmt.signedPct(s.total_return_pct);
    $("#equity-return").className = signClass(s.total_return_pct);
    $("#equity-abs").textContent = fmt.signedMoney(pnl);
    $("#equity-abs").className = signClass(pnl);
    $("#equity-initial").textContent = fmt.money(s.initial_cash);
  }

  // One equity point per recorded cycle, so the curve is a real history rather
  // than a redraw of the same number.
  const series = (s.log || []).map((c) => c.equity).filter((v) => ok(v) && v > 0);

  renderChart(series, s);
  renderMetrics(s, started);
}

async function refreshAccount() {
  if (accountPending || closing || (authEnabled && !localSettings && !lastAuthState?.username)) return;
  accountPending = true;
  const button = $("#account-refresh");
  const state = $("#exchange-state");
  const values = $("#exchange-values");
  const host = $("#exchange-account");
  const owner = lastAuthState?.username || "";
  button.disabled = true;
  state.textContent = "正在读取 Binance USDT 永续合约账户…";
  try {
    const account = await api("/api/account");
    if (owner !== (lastAuthState?.username || "")) return;
    $("#exchange-wallet").textContent = fmt.money(account.wallet);
    $("#exchange-available").textContent = fmt.money(account.available);
    $("#exchange-unrealized").textContent = fmt.signedMoney(account.cross_unrealized);
    $("#exchange-updated").textContent = `Binance 实时读取 · ${fmt.stamp(account.updated_at)} · 只读，不启动交易`;
    state.textContent = "已连接真实合约账户";
    values.hidden = false;
    host.classList.remove("error");
  } catch (error) {
    if (owner !== (lastAuthState?.username || "")) return;
    // A failed refresh must never leave an old number looking like a live
    // balance. Session/paper values below are intentionally unaffected.
    values.hidden = true;
    host.classList.add("error");
    state.textContent = `真实账户未读取：${error.message}`;
  } finally {
    button.disabled = false;
    accountPending = false;
  }
}

function renderChart(series, s) {
  const host = $("#equity-chart");
  const range = $("#chart-range");

  if (series.length < 2) {
    range.textContent = series.length ? "等待第二个数据点" : "权益走势";
    host.innerHTML = `<p class="empty" style="align-self:center">${
      s.running ? "正在采集数据，下个周期开始绘图。" : "启动后按周期绘制权益曲线。"
    }</p>`;
    return;
  }

  const min = Math.min(...series);
  const max = Math.max(...series);
  range.textContent = `${series.length} 个周期 · ${fmt.money(min)} – ${fmt.money(max)}`;

  const w = 600, h = 120, pad = 4;
  // A flat curve would divide by zero; give it a hairline band so the line
  // renders through the middle instead of collapsing onto an edge.
  const span = max - min || Math.max(Math.abs(max), 1) * 0.002;
  const x = (i) => pad + (i / (series.length - 1)) * (w - pad * 2);
  const y = (v) => h - pad - ((v - min) / span) * (h - pad * 2);

  const line = series.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(" ");
  const area = `${line} L${x(series.length - 1).toFixed(1)} ${h} L${x(0).toFixed(1)} ${h} Z`;
  const rising = series[series.length - 1] >= series[0];
  const stroke = rising ? "var(--up)" : "var(--down)";
  const id = rising ? "curveUp" : "curveDown";
  // The baseline is the initial cash: above it the session is up, below it
  // down, which a bare sparkline cannot show.
  const base = ok(s.initial_cash) && s.initial_cash >= min && s.initial_cash <= max
    ? `<line x1="0" y1="${y(s.initial_cash).toFixed(1)}" x2="${w}" y2="${y(s.initial_cash).toFixed(1)}"
            stroke="var(--ink-3)" stroke-width="1" stroke-dasharray="3 4" opacity=".55"/>`
    : "";

  host.innerHTML = `
    <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="权益走势">
      <defs>
        <linearGradient id="${id}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${stroke}" stop-opacity=".26"/>
          <stop offset="100%" stop-color="${stroke}" stop-opacity="0"/>
        </linearGradient>
      </defs>
      ${base}
      <path d="${area}" fill="url(#${id})"/>
      <path d="${line}" fill="none" stroke="${stroke}" stroke-width="1.6"
            stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>
    </svg>`;
}

function renderMetrics(s, started) {
  const p = s.position || {};
  const exposure = p.open && ok(p.notional) && s.equity > 0 ? (p.notional / s.equity) * 100 : 0;

  const cells = [
    { k: "可用现金", v: started ? fmt.money(s.cash) : "—" },
    { k: "占用保证金", v: fmt.money(s.margin_used) },
    { k: "名义仓位 / 权益", v: p.open ? fmt.pct(exposure, 1) : "0%" },
    { k: "净收益", v: started ? fmt.signedMoney(s.equity-s.initial_cash) : "—" },
  ];
  $("#account-metrics").innerHTML = cells
    .map((c) => `<div class="metric"><div class="k">${escape(c.k)}</div><div class="v">${c.v}</div></div>`)
    .join("");
}

// --------------------------------------------------------------- position

function renderPosition(s) {
  const host = $("#position-body");
  const note = $("#position-note");
  const p = s.position;

  if (!p || !p.open) {
    note.textContent = "空仓";
    host.innerHTML = s.running
      ? '<p class="empty">当前<b>空仓</b>。引擎会在策略给出目标仓位、且风控允许时开仓。</p>'
      : '<p class="empty">当前<b>空仓</b>。请在「交易配置」核对交易对、策略与风控后开始交易。</p>';
    return;
  }

  note.innerHTML = `<span class="pill ${p.side === "sell" ? "short" : "long"}">${p.side === "sell" ? "空头" : "多头"}</span>`;

  const fields = [
    { k: "数量", v: fmt.qty(p.quantity) },
    { k: "名义持仓", v: fmt.money(p.notional) },
    { k: "开仓价", v: fmt.money(p.entry_price) },
    { k: "最新价", v: fmt.money(p.mark_price) },
    { k: "浮动盈亏", v: `<span class="${signClass(p.unrealized)}">${fmt.signedMoney(p.unrealized)}</span>` },
    { k: "浮动收益率", v: `<span class="${signClass(p.return_pct)}">${fmt.signedPct(p.return_pct)}</span>` },
    { k: "开仓时间", v: fmt.stamp(p.opened_at), sm: true, wide: true },
  ];

  host.innerHTML =
    `<div class="kv">${fields
      .map((f) => `<div class="${f.wide ? "wide" : ""}"><span class="k">${escape(f.k)}</span><span class="v ${f.sm ? "sm" : ""}">${f.v}</span></div>`)
      .join("")}</div>` + renderStopRail(p);
}

// renderStopRail places the mark between the stop and the target. "How much
// room is left before I am stopped out" is the question an open position
// raises, and no column of absolute prices answers it at a glance.
function renderStopRail(p) {
  const hasStop = ok(p.stop_price) && p.stop_price > 0;
  const hasTarget = ok(p.target_price) && p.target_price > 0;
  if (!hasStop && !hasTarget) {
    return '<div class="rail-bar"><p class="empty">未设置止损与止盈，离场只由策略信号决定。</p></div>';
  }

  const mark = p.mark_price;
  // Bounds are the two bracket levels, so the mark's offset is the real ratio
  // of the distances: a stop 6% away and a target 18% away puts it at 25%,
  // which is the asymmetry the bar exists to show. Only a *missing* end is
  // synthesised, by mirroring the one that exists, so a one-sided bracket
  // still centres the mark instead of pinning it to an edge.
  const levels = [mark];
  if (hasStop) levels.push(p.stop_price);
  if (hasTarget) levels.push(p.target_price);
  let lo = Math.min(...levels);
  let hi = Math.max(...levels);
  if (!hasStop || !hasTarget) {
    const reach = Math.max(hi - mark, mark - lo);
    lo = Math.min(lo, mark - reach);
    hi = Math.max(hi, mark + reach);
  }
  const span = hi - lo;
  const at = span > 0 ? Math.min(100, Math.max(0, ((mark - lo) / span) * 100)) : 50;

  // A short's stop sits above the mark and its target below, so the ends are
  // labelled by role rather than by a fixed stop-on-the-left assumption.
  const stopIsLower = p.side !== "sell";
  const stopEnd = { role: "stop", label: "止损", price: p.stop_price, dist: p.stop_distance_pct, set: hasStop };
  const targetEnd = { role: "target", label: "止盈", price: p.target_price, dist: p.target_distance_pct, set: hasTarget };
  const left = { cls: "lo", ...(stopIsLower ? stopEnd : targetEnd) };
  const right = { cls: "hi", ...(stopIsLower ? targetEnd : stopEnd) };

  const end = (e) => `
    <span class="${e.cls} ${e.role}">
      ${escape(e.label)}
      <b>${e.set ? fmt.money(e.price) : "未设置"}</b>
      ${e.set && ok(e.dist) ? `${fmt.signedPct(e.dist, 1)}` : ""}
    </span>`;

  const room = ok(p.stop_distance_pct) && hasStop ? `距止损 ${fmt.pct(Math.abs(p.stop_distance_pct), 1)}` : "";
  return `
    <div class="rail-bar">
      <div class="rail-bar-head"><span>止损 / 最新价 / 止盈</span><span>${escape(room)}</span></div>
      <div class="rail-track ${stopIsLower ? "" : "flip"}"><span class="rail-mark" style="left:${at.toFixed(1)}%"></span></div>
      <div class="rail-legend">${end(left)}${end(right)}</div>
    </div>`;
}

// ------------------------------------------------------------------- risk

function renderRisk(s) {
  const host = $("#risk-body");
  const note = $("#risk-note");
  const r = s.risk || {};
  const started = s.cycles > 0 || s.running;

  if (!started) {
    note.textContent = "";
    host.innerHTML = '<p class="empty">启动后显示单日亏损、回撤额度与熔断状态。</p>';
    return;
  }

  const blocks = [];

  if (r.halted) {
    blocks.push(`<div class="alert"><b>已熔断</b>：${escape(translateReason(r.halt_reason) || "风控已停止交易")}。引擎不会再开新仓。</div>`);
  } else if (r.entries_blocked_day) {
    blocks.push('<div class="alert"><b>今日禁止开仓</b>：单日亏损已达上限，持仓仍受止损保护。</div>');
  }

  // Each budget is "used / allowed": a loss of 2% against a 5% daily cap is
  // 40% of the budget spent, which is the number that decides whether to
  // intervene.
  if (r.max_daily_loss_pct > 0) {
    const used = r.day_return_pct < 0 ? Math.abs(r.day_return_pct) : 0;
    blocks.push(gauge({
      label: "单日亏损额度",
      value: `${fmt.signedPct(r.day_return_pct)} / ${fmt.pct(r.max_daily_loss_pct, 1)}`,
      ratio: used / r.max_daily_loss_pct,
      foot: `今日盈亏 ${fmt.signedMoney(r.day_pnl)} · 日初权益 ${fmt.money(r.day_start_equity)}`,
    }));
  }

  if (r.max_drawdown_pct > 0) {
    const used = r.drawdown_pct < 0 ? Math.abs(r.drawdown_pct) : 0;
    blocks.push(gauge({
      label: "回撤额度",
      value: `${fmt.pct(used, 2)} / ${fmt.pct(r.max_drawdown_pct, 1)}`,
      ratio: used / r.max_drawdown_pct,
      foot: `距峰值 ${fmt.money(s.peak_equity)}`,
    }));
  }

  const flags = [
    { text: r.halted ? "风控熔断" : "风控正常", bad: r.halted },
    { text: !s.running ? "策略已停止" : r.halted || r.entries_blocked_day ? "禁止开仓" : "允许开仓", bad: r.halted || r.entries_blocked_day },
    { text: r.stop_loss_pct > 0 ? `止损 ${fmt.pct(r.stop_loss_pct, 1)}` : "无止损", bad: !(r.stop_loss_pct > 0) },
    { text: r.take_profit_pct > 0 ? `止盈 ${fmt.pct(r.take_profit_pct, 1)}` : "无止盈", bad: false },
    { text: s.mode === "live" ? "实盘下单" : "纸面模拟", bad: s.mode === "live" },
  ];
  blocks.push(`<div class="flags">${flags
    .map((f) => `<span class="flag ${f.bad ? "bad" : "ok"}">${escape(f.text)}</span>`)
    .join("")}</div>`);
  const p = s.protection || {};
  const protection = s.mode === "live"
    ? (p.state === "verified"
      ? `交易所侧已核验 · ${p.checked_at ? new Date(p.checked_at).toLocaleString("zh-CN") : "时间未知"} · 止损 ${p.stop?.client_algo_id || "—"}${p.target?.present ? ` / 止盈 ${p.target.client_algo_id}` : ""}`
      : p.state === "not_required" ? "交易所空仓且无需保护单"
      : `未核验（${p.state || "unknown"}）${p.reason ? ` · ${p.reason}` : ""}；禁止新开仓并到 Binance 人工核对`)
    : "纸面风控仅本地模拟，不存在交易所保护单";
  blocks.push(`<div class="empty">${escape(strategyByName(s.strategy)?.title || s.strategy)} · 日线决策 / ${s.interval_seconds} 秒轮询<br>保护状态：${protection}</div>`);

  if (s.last_error) {
    blocks.push(`<div class="alert">最近一次周期出错：${escape(s.last_error)}</div>`);
  }

  note.textContent = r.halted ? "已停止交易" : r.entries_blocked_day ? "今日只平不开" : "正常";
  host.innerHTML = `<div class="risk-rows">${blocks.join("")}</div>`;
}

function gauge({ label, value, ratio, foot }) {
  const pct = Math.min(100, Math.max(0, (Number.isFinite(ratio) ? ratio : 0) * 100));
  const tone = pct >= 80 ? "hot" : pct >= 50 ? "warn" : "";
  return `
    <div class="gauge">
      <div class="top"><span>${escape(label)}</span><b>${value}</b></div>
      <div class="gauge-track"><div class="gauge-fill ${tone}" style="width:${pct.toFixed(1)}%"></div></div>
      <div class="gauge-foot">已用 ${pct.toFixed(0)}%${foot ? ` · ${escape(foot)}` : ""}</div>
    </div>`;
}

// -------------------------------------------------------------------- log

function renderCycles(log) {
  // Newest first: the interesting row is always at the top.
  const rows = [...log].reverse().map((c) => {
    const action = c.error
      ? '<span class="pill stop">错误</span>'
      : `<span class="pill ${c.action === "hold" || c.action === "flat" ? "" : "long"}">${escape(translateReason(c.action) || c.action)}</span>`;
    // The 说明 cell answers "why did it do that": the model's own explanation
    // on a normal cycle, or the failure detail on a degradation. Without it a
    // run of ai_unavailable rows says nothing about what to fix.
    const note = c.error
      ? `<span class="muted">${escape(c.error)}</span>`
      : c.reason
        ? escape(c.reason)
        : "";
    return [
      `<td>${escape(c.time)}</td>`,
      `<td class="tag">${action}</td>`,
      `<td class="num">${c.price ? fmt.money(c.price) : "—"}</td>`,
      `<td class="num">${fmt.money(c.equity)}</td>`,
      `<td class="num">${fmt.money(c.cash)}</td>`,
      `<td class="tag">${escape(c.position || "空仓")}</td>`,
      `<td class="note">${note}</td>`,
    ];
  });
  return table(
    ["时间", "动作", "价格", "权益", "现金", "持仓", "说明"],
    rows,
    "还没有运行记录。点「开始交易」或「单步」。"
  );
}

function renderTrades(trades) {
  const rows = [...trades].reverse().map((t) => [
    `<td>${escape(t.entry_time)}</td>`,
    `<td>${escape(t.exit_time)}</td>`,
    `<td class="tag"><span class="pill ${t.side === "sell" ? "short" : "long"}">${escape(sideLabel[t.side] || t.side)}</span></td>`,
    `<td class="num">${fmt.qty(t.quantity)}</td>`,
    `<td class="num">${fmt.money(t.entry_price)}</td>`,
    `<td class="num">${fmt.money(t.exit_price)}</td>`,
    `<td class="num ${signClass(t.pnl)}">${fmt.signedMoney(t.pnl)}</td>`,
    `<td class="num ${signClass(t.return_pct)}">${fmt.signedPct(t.return_pct)}</td>`,
    `<td class="tag"><span class="pill ${String(t.reason).includes("stop") ? "stop" : ""}">${escape(translateReason(t.reason))}</span></td>`,
  ]);
  return table(
    ["开仓", "平仓", "方向", "数量", "开仓价", "平仓价", "盈亏", "收益率", "离场原因"],
    rows,
    "本次会话还没有已平仓的交易。"
  );
}

function renderTab() {
  const body = $("#tab-body");
  if (!session) {
    body.innerHTML = '<p class="empty" style="padding:14px">还没有运行记录。</p>';
    return;
  }
  if (activeTab === "cycles") body.innerHTML = renderCycles(session.log || []);
  if (activeTab === "trades") body.innerHTML = renderTrades(session.trades || []);
  if (activeTab === "orders") body.innerHTML = renderOrders(session.orders || []);
  $$(".tab").forEach((tab) => { tab.classList.toggle("active", tab.dataset.tab === activeTab); tab.setAttribute("aria-selected", String(tab.dataset.tab === activeTab)); });
}

// renderLogNote tells the operator where the run log lives and whether it is
// actually being written. The log survives a restart, so "the table is empty"
// and "the history could not be saved" are different problems and must not
// look the same.
function renderLogNote(s) {
  const note = $("#log-note");
  if (!note) return;
  if (s.log_error) {
    note.textContent = `运行日志写入失败：${s.log_error}`;
    note.className = "hint log-note warn";
    return;
  }
  note.textContent = s.log_path ? `运行日志保存在 ${s.log_path}，重启后仍会保留。` : "";
  note.className = "hint log-note";
}

// ------------------------------------------------------------------ session

function renderSession(s) {
  session = s;
  const identity = `${s.started_at || ""}:${s.symbol}:${s.strategy}`;
  if (s.settings && identity !== formIdentity) { applySettings(s.settings); formIdentity = identity; refreshMarket(); }
  // Reaching this point means the server answered, so a previous "cannot
  // reach the server" banner is stale and must not linger.
  setServerDown(false);

  renderTopbar(s);
  renderAccount(s);
  renderPosition(s);
  renderRisk(s);

  $("#cycles-count").textContent = (s.log || []).length;
  $("#trades-count").textContent = (s.trades || []).length;
  $("#orders-count").textContent = (s.orders || []).length;
  renderLogNote(s);
  renderTab();

  // The config in force is part of the display, so lock the rail rather than
  // letting edits look as though they apply to the running session.
  const locked = Boolean(s.running || s.starting || s.recovering);
  $("#run-form").classList.toggle("locked", locked);
  $("#rail-lock").hidden = !locked;
  $("#rail-lock").textContent = s.starting ? "初始化中" : s.recovering ? "保护单核验中" : "运行中只读";
  Array.from($("#run-form").elements).filter(el => el.matches("input, select")).forEach(el => { el.disabled = locked; });
  $("#load-symbols").disabled = locked;

  if (s.last_error) setStatus(`最近一次周期出错：${s.last_error}`, "error");
  else if (s.running) setStatus("");
}

async function refreshSession() {
  try {
    const status = await api("/api/session");
    renderSession(status);
    setRunningUi(status.running);
    // Poll fast while a loop is live, slowly when idle so an open tab does not
    // hammer the server for a session that is not running.
    schedulePoll(status.running || status.starting || status.recovering ? 3000 : 15000);
  } catch (error) {
    if (error.status === 401 && authEnabled) {
      // The session cookie expired while this tab was open: offer the login
      // card again instead of pretending the server died.
      session = null;
      showAuthCard();
      setStatus("登录已失效，请重新登录", "error");
      schedulePoll(30000);
      return;
    }
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
  if (down) {
    setStatus(offlineMessage(), "error");
    $("#status-dot").className = "dot error";
    $("#run-state").textContent = "连接中断";
  } else {
    setStatus("");
  }
}

function schedulePoll(delay) {
  if (pollTimer) clearTimeout(pollTimer);
  pollTimer = setTimeout(refreshSession, delay);
}

function setRunningUi(running) {
  const transitioning = Boolean(session?.starting || session?.recovering);
  const locked = running || transitioning;
  const label = session?.starting ? "初始化中…" : session?.recovering ? "保护单核验中…" : running ? "交易中…" : "启动交易会话 →";
  $("#rail-run").disabled = locked || busy;
  $("#rail-run").textContent = label;
  $("#step-button").disabled = !running || transitioning || busy;
  $("#stop-button").disabled = !running || transitioning || busy;
  if (session) $("#recovery-button").disabled = Boolean(session.recovering || busy);
  // The AI page's controls all describe the NEXT session, so a live loop must
  // not let them look editable.
  renderAiPage(lastConfig || {});
}

// ------------------------------------------------------------------- auth

// applyAuthUi shows the login card when accounts are on and no one is signed
// in, the user chip when a session exists, and the credential form in the
// rail whenever accounts are enabled (it is where the Binance keys and the
// model URL/token live). Every piece degrades to the historical page when
// authEnabled is false.
function applyAuthUi(config) {
  authEnabled = Boolean(config?.auth?.enabled);
  localSettings = Boolean(config?.local_settings);
  lastAuthState = authEnabled ? (config.auth || {}) : null;
  const user = config?.auth?.username || "";

  const chip = $("#user-chip");
  const card = $("#auth-card");
  const signedIn = authEnabled && Boolean(user);
  const previous = $("#user-name").textContent;
  if (previous !== user) $("#credentials-form").reset();
  $("#creds-adv").hidden = !signedIn;
  // The desktop's single-user mode has no login, so the account chip (which
  // exists to log out) stays hidden while the credential form is shown.
  chip.hidden = !signedIn || localSettings;
  $("#user-name").textContent = user;
  card.hidden = !authEnabled || signedIn;
  $("#settings-login").hidden = !authEnabled || signedIn;
  $("#settings-account").textContent = localSettings
    ? "桌面版本机设置：密钥保存在这台电脑的用户目录，不上传、不入库。"
    : signedIn ? `当前账号：${user}`
      : authEnabled ? "登录后可保存本账号的连接设置。" : "此服务未启用账号设置，请由管理员配置服务环境。";
  $("#cred-status").textContent = "";
  liveGate = Boolean(config?.live_gate);
  liveGateEnv = Boolean(config?.live_gate_env);
  // The generic "use HTTPS" advice is wrong on the desktop build: the page is
  // served over loopback and never leaves the machine.
  const railHint = $("#rail-cred-hint");
  if (railHint) {
    railHint.textContent = localSettings
      ? `此处只保存连接设置，不启动交易。文件位置：${config?.auth?.path || "本机用户目录"}`
      : config?.desktop
        ? "此处只保存连接设置，不启动交易。桌面版服务只监听本机 127.0.0.1，密钥不会离开这台电脑。"
        : "此处只保存连接设置，不启动交易。请通过 HTTPS 访问管理界面后再填写密钥。";
  }
  if (signedIn) {
    renderCredStatus(config.auth);
    $('[data-cred="llm_base_url"]').value = config.auth.llm_base_url || "";
    $('[data-cred="llm_model"]').value = config.auth.llm_model || "";
    // Show the account's saved trading persona in the prompt-tune block so a
    // previous L2 winner is visible and editable (empty = built-in persona).
    const personaField = $("#ai-persona");
    if (personaField) personaField.value = config.auth.llm_prompt || "";
    // A stored key means the model list can be fetched without the user
    // typing anything: offer the picker up front instead of an empty box.
    // Once per page load, and never while a request is already in flight.
    if (config.auth.llm_key && !modelsLoaded) {
      modelsLoaded = true;
      loadModels({ quiet: true });
    }
  }
  // The local live switch only exists on the desktop: on a networked server
  // the environment variable stays the only gate, so the checkbox is not
  // offered there at all.
  const liveSwitch = $("#local-live-switch");
  if (liveSwitch) {
    liveSwitch.hidden = !localSettings;
    $("#local-live").checked = Boolean(config?.auth?.allow_live);
  }
  localLiveHint();

  renderKeyHint();
  renderAiPage(config);
}

// renderCredStatus shows which credential slots are filled without ever
// echoing the values — the server only returns presence flags plus the LLM
// base URL, which is public state ("which AI endpoint is configured").
function renderCredStatus(auth) {
  const bits = [];
  bits.push(auth.binance_api ? "Binance Key 已配置" : "Binance Key 未配置");
  bits.push(auth.binance_secret ? "Secret 已配置" : "Secret 未配置");
  if (auth.llm_base_url) bits.push(`模型服务 ${auth.llm_base_url}`);
  if (auth.llm_model) bits.push(`模型 ${auth.llm_model}`);
  bits.push(auth.llm_key ? "Token 已配置" : "Token 未配置");
  $("#cred-status").textContent = bits.join(" · ");
}

// renderKeyHint connects the 实盘 switch to the credential state: when the box
// is on the user must know where the keys come from and whether they are in
// place. Without accounts the historical environment-variable path applies.
function renderKeyHint() {
  const host = $("#execute-key-hint");
  const execute = Boolean($('[name="execute"]')?.checked);
  if (!execute) { host.hidden = true; host.innerHTML = ""; return; }
  host.hidden = false;
  if (localSettings) {
    const auth = lastAuthState || {};
    const ready = auth.binance_api && auth.binance_secret;
    if (!ready) {
      host.className = "key-hint warn";
      host.textContent = "尚未保存 Binance 密钥：到「设置 → Binance 与 AI 服务」填写 API Key 与 Secret Key 并保存。";
      return;
    }
    host.className = liveGate ? "key-hint ok" : "key-hint warn";
    host.textContent = liveGate
      ? "Binance 密钥已保存（尚未验证连接或权限）。"
      : "密钥已保存，但实盘开关未打开：到「设置 → Binance 与 AI 服务」打开实盘开关。";
    return;
  }
  if (!authEnabled) {
    host.className = "key-hint";
    host.textContent = "实盘密钥取自环境变量 BINANCE_API_KEY / BINANCE_SECRET_KEY。";
    return;
  }
  const auth = lastAuthState || {};
  if (!auth.username) {
    host.className = "key-hint warn";
    host.textContent = "实盘需要先注册或登录账号，再在「设置 → Binance 与 AI 服务」保存 Binance 密钥。";
    return;
  }
  const ready = auth.binance_api && auth.binance_secret;
  if (!ready) {
    host.className = "key-hint warn";
    host.textContent = "本账号尚未保存 Binance 密钥：到「设置 → Binance 与 AI 服务」填写并保存后才能下单。";
    return;
  }
  // Keys are in place, but the process-level kill switch may still be off.
  // Saying so up front turns a surprising 403 at start into a one-line fix.
  host.className = liveGate ? "key-hint ok" : "key-hint warn";
  host.textContent = liveGate
    ? "Binance 密钥已保存（尚未验证连接或权限）。"
    : "密钥已保存，但服务端实盘闸门未开：启动服务时加 TA_ALLOW_LIVE=1 后才允许下单。";
}

// localLiveHint refreshes the line under the desktop live switch. The switch
// is a convenience over the environment variable, never a replacement for the
// confirmation phrase, and saying so here keeps the two from being confused.
function localLiveHint() {
  const hint = $("#local-live-hint");
  if (!hint) return;
  hint.textContent = liveGateEnv
    ? "当前由环境变量 TA_ALLOW_LIVE=1 打开；取消环境变量后此处开关才会生效。"
    : liveGate
      ? "已打开：启动实盘仍须输入「确认实盘」，且只影响本机桌面版。"
      : "默认关闭。打开后本机桌面版才能启动实盘；服务器版始终只认环境变量。";
}

function setAuthTab(mode) {
  authMode = mode;
  const isLogin = mode === "login";
  $("#auth-tab-login").classList.toggle("active", isLogin);
  $("#auth-tab-register").classList.toggle("active", !isLogin);
  $("#auth-submit").textContent = isLogin ? "登录" : "注册账号";
  $("#auth-card .panel-head h2").textContent = isLogin ? "账号登录" : "注册账号";
  $("#auth-username").placeholder = isLogin ? "用户名" : "用户名（3–32 位）";
  $("#auth-message").textContent = "";
}

async function submitAuth(event) {
  event.preventDefault();
  const body = {
    username: $("#auth-username").value.trim(),
    password: $("#auth-password").value,
  };
  const path = authMode === "login" ? "/api/auth/login" : "/api/auth/register";
  const button = $("#auth-submit");
  button.disabled = true;
  try {
    await api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    // A fresh session: re-read config so the chip and credential form appear.
    const config = await api("/api/config");
    applyAuthUi(config);
    $("#auth-password").value = "";
    refreshSession();
    refreshAccount();
  } catch (error) {
    $("#auth-message").textContent = error.message;
    $("#auth-message").className = "hint warn";
  } finally {
    button.disabled = false;
  }
}

async function logout() {
  try {
    await api("/api/auth/logout", { method: "POST" });
    window.location.reload();
  } catch (error) { setStatus(`退出失败：${error.message}`, "error"); }
}

// showAuthCard re-opens the login card after a 401: the session is gone, so
// hide the user chip and credential form and put the card back on the board.
function showAuthCard() {
  $("#credentials-form").reset();
  $("#cred-status").textContent = "";
  $("#exchange-values").hidden = true;
  $("#exchange-state").textContent = "请登录后读取自己的 Binance 合约账户。";
  $("#user-name").textContent = "";
  $("#settings-account").textContent = "登录已失效，请重新登录。";
  $("#settings-login").hidden = false;
  $("#user-chip").hidden = true;
  $("#creds-adv").hidden = true;
  $("#auth-card").hidden = false;
  setAuthTab("login");
  lastAuthState = null;
  renderKeyHint();
}

// saveCredentials posts the four credential fields the user typed. Empty
// inputs are omitted so a partial save only touches what was entered.
async function saveCredentials() {
  const fields = {};
  for (const input of $$("[data-cred]")) {
    const value = input.value.trim();
    if (value) fields[input.dataset.cred] = value;
  }
  const button = $("#cred-save");
  button.disabled = true;
  try {
    // Desktop single-user mode writes to the local file; account mode writes
    // to the logged-in user's vault. The response shape is identical.
    const path = localSettings ? "/api/local/credentials" : "/api/auth/credentials";
    const view = await api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(fields) });
    lastAuthState = view;
    renderCredStatus(view);
    renderKeyHint();
    if (localSettings) {
      liveGate = Boolean(view.allow_live) || liveGateEnv;
      const liveBox = $("#local-live");
      if (liveBox) liveBox.checked = Boolean(view.allow_live);
    }
    // Clear the secret inputs; the status line now reflects the stored state,
    // and the LLM URL stays typed so it can be read at a glance.
    $$("[data-cred]").forEach((input) => { if (input.type !== "url") input.value = ""; });
    $("#cred-status").textContent += " · 已保存";
    if (view.binance_api && view.binance_secret) refreshAccount();
  } catch (error) {
    $("#cred-status").textContent = `保存失败：${error.message}`;
  } finally {
    button.disabled = false;
  }
}

// loadModels asks the configured endpoint which models it serves and offers
// them as a datalist behind the 模型名 field. The token typed in the form is
// used when present so a model can be picked before the first save; otherwise
// the server falls back to whatever is already stored. This is the model-side
// counterpart of 全部币种: choose from what exists instead of typing a name
// from memory.
async function loadModels(options = {}) {
  const quiet = Boolean(options.quiet);
  const button = $("#load-models");
  if (button) { button.disabled = true; button.textContent = "获取中…"; }
  const hint = $("#model-status");
  if (hint) { hint.textContent = ""; hint.className = "hint"; }
  try {
    const payload = {
      llm_base_url: $('[data-cred="llm_base_url"]')?.value.trim() || "",
      llm_api_key: $('[data-cred="llm_api_key"]')?.value.trim() || "",
    };
    const data = await api("/api/models", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const models = data.models || [];
    const options = document.getElementById("model-options");
    if (options) {
      options.innerHTML = models.map((m) => `<option value="${escape(m)}"></option>`).join("");
    }
    const input = $('[data-cred="llm_model"]');
    // The list is offered, never auto-applied: silently writing the first
    // entry into an empty field would mean a later 保存密钥 (for, say, the
    // Binance key) also commits a model the user never picked. Focus moves
    // only when the user asked for the list, so the automatic call on page
    // load does not yank the cursor out of whatever they were doing.
    if (input && models.length && !quiet) input.focus();
    if (hint) {
      hint.textContent = input && input.value.trim()
        ? `已获取 ${models.length} 个模型，点模型名输入框可改选；也可以直接手写。`
        : `已获取 ${models.length} 个模型，点模型名输入框选择；也可以直接手写。`;
      hint.className = "hint";
    }
  } catch (error) {
    // The automatic call on page load stays silent on failure: a gateway that
    // simply has no /models route is not an error the user needs to see
    // before they have asked for anything. Pressing 获取模型 always reports.
    if (hint && !quiet) {
      hint.textContent = `获取失败：${error.message}`;
      hint.className = "hint warn";
    }
  } finally {
    if (button) { button.disabled = false; button.textContent = "获取模型"; }
  }
}

// toggleLocalLive arms or disarms the desktop live gate. It is deliberately a
// separate request from the credential save so a mis-click on the switch can
// never rewrite the keys, and so the page can show the gate's state without
// touching the form.
async function toggleLocalLive(allow) {
  const box = $("#local-live");
  box.disabled = true;
  try {
    const view = await api("/api/local/credentials", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ allow_live: allow }),
    });
    lastAuthState = view;
    liveGate = Boolean(view.allow_live) || liveGateEnv;
    box.checked = Boolean(view.allow_live);
    renderKeyHint();
    localLiveHint();
    $("#cred-status").textContent = allow
      ? "实盘开关已打开：仍需输入「确认实盘」才会真正下单。"
      : "实盘开关已关闭：启动实盘会被拒绝。";
  } catch (error) {
    box.checked = !allow;
    $("#cred-status").textContent = `实盘开关保存失败：${error.message}`;
  } finally {
    box.disabled = false;
  }
}

// ------------------------------------------------------------------- bootstrap

async function bootstrap() {
  const config = await api("/api/config");
  lastConfig = config;
  strategies = config.strategies || [];

  $("#strategy-select").innerHTML = strategies
    .map((s) => `<option value="${escape(s.name)}">${escape(s.title)}</option>`)
    .join("");

  // Seed the form from the effective server config.
  $('[name="symbol"]').value = config.symbol;
  $('[name="days"]').value = config.days;
  $('[name="initial_cash"]').value = config.initial_cash;
  $("#strategy-select").value = config.strategy;
  $("#config-source").textContent = config.output_dir ? `报告目录 ${config.output_dir}` : "";

  // The desktop edition can stop itself; the server edition must not expose
  // that control, so the button only appears when the backend says so.
  if (config.desktop) {
    const quit = $("#quit-button");
    quit.hidden = false;
    quit.addEventListener("click", async () => {
      if (session?.position?.open && !window.confirm("退出不会平仓，本地决策与风控轮询会停止。请在 Binance 核对持仓和保护单。确认退出？")) return;
      quit.disabled = true;
      quit.textContent = "退出中…";
      try {
        await api("/api/shutdown", { method: "POST" });
      } catch (error) {
        // The connection usually drops as the server exits, which is expected.
      }
      if (pollTimer) clearTimeout(pollTimer);
      closing = true;
      clearTimeout(marketTimer);
      document.body.innerHTML =
        '<div class="bye"><h1>已退出</h1><p>服务已停止，可以关闭此页面。</p></div>';
    });
  }

  const risk = config.risk || {};
  const toPct = (v) => (v == null ? "" : String(Number((v * 100).toFixed(4))));
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
  renderAiPage(config);
  if (config.settings) applySettings(config.settings);
  liveGate = Boolean(config.live_gate);
  applyAuthUi(config);
  renderPage();
  await refreshSession();
  refreshMarket();
  refreshAccount();
}

// ------------------------------------------------------------------- wiring

$("#strategy-select").addEventListener("change", () => {
  renderStrategyParams();
  renderAiPage(lastConfig || {});
});

// ------------------------------------------------------------- AI page (L1-L3)
// The model is the core of this project, so it owns a page instead of hiding
// behind one strategy option. Everything here only affects the NEXT session:
// the running one keeps the settings it started with.
let promptTuneReport = null; // last /api/tune-prompt response, for adopting
let tuneReport = null; // last /api/tune response, for adopting

// aiState summarizes what the model can currently do, so the page never looks
// broken when the key is simply missing.
function renderAiPage(config) {
  const auth = config?.auth || {};
  const signedIn = Boolean(auth.enabled && auth.username);
  const local = Boolean(config?.local_settings);
  const envKey = Boolean(config?.llm_env_key);
  // Accounts-mode requests never ride on the deployer's environment key, so
  // only the account's own flag counts there. The desktop is single-user, so
  // its saved key and the environment fallback are both legitimate.
  const hasKey = local
    ? Boolean(auth.llm_key) || envKey
    : signedIn ? Boolean(auth.llm_key) : envKey;
  const model = auth.llm_model || config?.llm_model || "";

  const bits = [];
  if (hasKey) bits.push(`模型已就绪${model ? `：${model}` : ""}`);
  else bits.push("模型未配置：AI 功能会退化为「只回测、不调用模型」");
  if (auth.llm_base_url) bits.push(`服务 ${auth.llm_base_url}`);
  if (hasKey) {
    if (local) bits.push(auth.llm_key ? "密钥来自本机设置" : "密钥来自环境变量 LLM_API_KEY / OPENAI_API_KEY");
    else if (signedIn) bits.push("密钥来自当前账号");
    else bits.push("密钥来自环境变量 LLM_API_KEY / OPENAI_API_KEY");
  } else if (local) {
    bits.push("到「设置 → Binance 与 AI 服务」填写 AI 服务 URL、模型名与 Token");
  } else if (auth.enabled) {
    bits.push("登录后可在设置中保存密钥");
  } else {
    bits.push("从环境变量 LLM_API_KEY / OPENAI_API_KEY 读取密钥");
  }

  const status = $("#ai-status");
  status.textContent = bits.join(" · ");
  status.className = `ai-status ${hasKey ? "ok" : "warn"}`;

  const strategy = $("#strategy-select").value;
  const isLLM = strategy === "llm";
  const running = Boolean(session?.running);
  $("#ai-strategy-state").textContent = isLLM
    ? "当前策略：LLM 目标仓位（模型正在决定仓位）"
    : `当前策略：${strategyByName(strategy)?.title || strategy}（模型只做否决与复盘）`;
  $("#ai-use-llm").disabled = isLLM || running;
  $("#ai-use-llm").textContent = isLLM ? "已是 AI 策略" : "使用 AI 策略";

  // Nothing here is hidden or disabled just because the key is missing: every
  // path degrades safely (veto is fail-open, the loops run the baseline and
  // say the model was skipped). Greying the whole page out would reproduce the
  // "where is the AI?" problem this page exists to fix.
  $("#ai-veto").disabled = running;
  $("#ai-prompt-note").textContent = hasKey
    ? "迭代会多次调用模型并回测，耗时随轮数增长。"
    : "模型未配置：迭代只会跑基线回测，不会改写人设。";
  $("#ai-prompt-run").disabled = running;
  $("#ai-tune-run").disabled = running;
  $("#ai-review-backtest").disabled = running;
}

// aiRequest is the run description the AI endpoints share with the form.
function aiRequest() {
  const request = collectRequest();
  const objective = $("#ai-objective").value;
  const rounds = Number($("#ai-rounds").value) || 1;
  const stall = Number($("#ai-stall").value) || 0;
  const cv = Number($("#ai-cv").value) || 0;
  return { request, objective, rounds, stall, cv };
}

function aiLog(text) {
  const log = $("#ai-tune-log");
  log.hidden = false;
  log.textContent = text;
}

function reviewLog(text) {
  const log = $("#ai-review-log");
  log.hidden = false;
  log.textContent = text;
}

async function runPromptTune() {
  if (session?.running || busy) return;
  const { request, objective, rounds, stall, cv } = aiRequest();
  request.strategy = "llm"; // the loop only exists for the llm strategy
  request.objective = objective;
  request.rounds = rounds;
  request.stall = stall;
  request.cv_folds = cv;
  request.prompt = $("#ai-persona").value.trim();

  const runButton = $("#ai-prompt-run");
  runButton.disabled = true;
  runButton.textContent = "迭代中（回测会多次调用模型）…";
  aiLog(`目标 ${request.objective} · ${request.rounds} 轮 · 交叉验证 ${request.cv_folds} 窗口\n正在回测基线，随后由模型改写人设…\n`);
  try {
    const report = await api("/api/tune-prompt", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    promptTuneReport = report;
    renderPromptTuneReport(report);
    if (report.best_prompt) {
      $("#ai-persona").value = report.best_prompt;
      $("#ai-prompt-adopt").hidden = false;
    }
  } catch (error) {
    $("#ai-tune-log").textContent += `\n失败：${error.message}`;
  } finally {
    runButton.disabled = false;
    runButton.textContent = "开始迭代";
  }
}

function renderPromptTuneReport(report) {
  const log = $("#ai-tune-log");
  const lines = [];
  if (!report.llm_enabled) lines.push("模型未配置（缺 LLM 密钥/URL）：只回测了基线，没有进行改写。请先在设置里保存 AI 服务。");
  for (const round of report.rounds || []) {
    const head = round.index === 0 ? "基线" : `第 ${round.index} 轮`;
    lines.push(`[${head}] ${report.objective} = ${round.objective_value}`);
    if (round.improved) lines.push(`  ↑ 改进，采纳`);
    if (round.note) lines.push(`  ${round.note}`);
    if (round.persona) lines.push(`  人设：${truncateLine(round.persona, 90)}`);
    if (round.rationale) lines.push(`  理由：${truncateLine(round.rationale, 90)}`);
  }
  const delta = (report.best_value - report.baseline).toFixed(4);
  lines.push(`\n基线 ${report.baseline} → 最佳 ${report.best_value}（Δ ${delta}）${report.early_stopped ? " · 连续无改进，已早停" : ""}`);
  const v = report.validation;
  if (v) {
    lines.push(v.accepted ? "交叉验证：通过（未过拟合训练窗口）" : "交叉验证：拒绝（在验证窗口退化，保留基线人设）");
    for (const win of v.windows || []) {
      lines.push(`  窗口 ${win.index}${win.holdout ? "（留一）" : ""} 基线 ${win.baseline} → 新 ${win.winner} ${win.passed ? "通过" : "未通过"}`);
    }
  }
  log.textContent = lines.join("\n");
}

function truncateLine(text, max) {
  text = String(text).split("\n")[0];
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

// Adopting the winner persists it as the account's trading persona, so the
// next session (and every backtest/tune after) starts from the better brain.
async function adoptPromptTune() {
  if (!promptTuneReport?.best_prompt) return;
  const persona = promptTuneReport.best_prompt;
  if (!authEnabled) {
    $("#ai-persona").value = persona;
    alert("匿名模式没有账号可保存：人设已填入上方文本框，请登录后在设置中保存。");
    return;
  }
  try {
    const view = await api("/api/auth/credentials", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ llm_prompt: persona }),
    });
    lastAuthState = view;
    renderCredStatus(view);
    $("#ai-prompt-adopt").hidden = true;
    const log = $("#ai-tune-log");
    if (!log.hidden) log.textContent += "\n\n已保存为账号人设：下次启动会话与回测都会使用它。";
  } catch (error) {
    alert(`保存失败：${error.message}`);
  }
}

$("#ai-prompt-run").addEventListener("click", runPromptTune);
$("#ai-prompt-adopt").addEventListener("click", adoptPromptTune);

// ----- AI parameter tuning (L1): the model proposes parameter sets for the
// strategy currently selected in the form and the backtest ranks them.
async function runTune() {
  if (session?.running || busy) return;
  const { request, objective, rounds, stall, cv } = aiRequest();
  request.objective = objective;
  request.rounds = rounds;
  request.stall = stall;
  request.cv_folds = cv;

  const runButton = $("#ai-tune-run");
  runButton.disabled = true;
  runButton.textContent = "调优中（每轮都会回测）…";
  aiLog(`策略 ${strategyByName(request.strategy)?.title || request.strategy} · 目标 ${objective} · ${rounds} 轮\n正在回测基线，随后由模型提出参数…\n`);
  try {
    const report = await api("/api/tune", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    tuneReport = report;
    renderTuneReport(report);
    if (report.best_params && Object.keys(report.best_params).length) {
      $("#ai-tune-adopt").hidden = false;
    }
  } catch (error) {
    $("#ai-tune-log").textContent += `\n失败：${error.message}`;
  } finally {
    runButton.disabled = false;
    runButton.textContent = "调优策略参数";
  }
}

function renderTuneReport(report) {
  const lines = [];
  if (!report.llm_enabled) lines.push("模型未配置（缺 LLM 密钥/URL）：只回测了基线，没有提出新参数。请先在设置里保存 AI 服务。");
  for (const round of report.rounds || []) {
    const head = round.index === 0 ? "基线" : `第 ${round.index} 轮`;
    lines.push(`[${head}] ${report.objective} = ${round.objective_value}`);
    if (round.improved) lines.push("  ↑ 改进，采纳");
    if (round.note) lines.push(`  ${round.note}`);
    if (round.params) lines.push(`  参数：${formatParams(round.params)}`);
    if (round.rationale) lines.push(`  理由：${truncateLine(round.rationale, 90)}`);
  }
  const delta = (report.best_value - report.baseline).toFixed(4);
  lines.push(`\n基线 ${report.baseline} → 最佳 ${report.best_value}（Δ ${delta}）${report.early_stopped ? " · 连续无改进，已早停" : ""}`);
  if (report.best_params) lines.push(`最佳参数：${formatParams(report.best_params)}`);
  $("#ai-tune-log").textContent = lines.join("\n");
}

function formatParams(params) {
  return Object.keys(params)
    .sort()
    .map((key) => `${key}=${params[key]}`)
    .join("  ");
}

// Adopting tuned parameters writes them into the strategy form, so the next
// session start (and the next tune) begins from the winning set.
function adoptTune() {
  if (!tuneReport?.best_params) return;
  const spec = strategyByName($("#strategy-select").value);
  if (!spec) return;
  const values = {};
  for (const param of spec.params) {
    const value = tuneReport.best_params[param.key];
    if (value != null) values[param.key] = value;
  }
  renderStrategyParams(values);
  $("#ai-tune-adopt").hidden = true;
  $("#ai-tune-log").textContent += "\n\n已写入上方「策略」表单：下次启动会使用这组参数。";
}

$("#ai-tune-run").addEventListener("click", runTune);
$("#ai-tune-adopt").addEventListener("click", adoptTune);

// ----- AI reviews (L3): the model explains a finished backtest or the trades
// the live session actually made.
async function reviewBacktest() {
  if (busy) return;
  const button = $("#ai-review-backtest");
  button.disabled = true;
  reviewLog("正在回测并请求模型复盘…\n");
  try {
    const request = collectRequest();
    request.review = true;
    const report = await api("/api/backtest", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    const head = `回测 ${report.symbol} · ${report.strategy} · ${report.bars} 根K线\n`;
    reviewLog(report.review
      ? `${head}\n${report.review}`
      : `${head}\n模型不可用：${report.review_unavailable || "未返回复盘"}`);
  } catch (error) {
    reviewLog(`复盘失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
}

async function reviewLive() {
  if (busy) return;
  const button = $("#ai-review-live");
  button.disabled = true;
  reviewLog("正在汇总本次会话的成交并请求模型复盘…\n");
  try {
    const report = await api("/api/session/review", { method: "POST" });
    reviewLog(report.review || `模型不可用：${report.review_unavailable || "未返回复盘"}`);
  } catch (error) {
    reviewLog(`复盘失败：${error.message}`);
  } finally {
    button.disabled = false;
  }
}

$("#ai-review-backtest").addEventListener("click", reviewBacktest);
$("#ai-review-live").addEventListener("click", reviewLive);

// 使用 AI 策略: switch the strategy picker to the model-driven strategy and
// send the user to the strategy page to confirm the rest of the parameters.
$("#ai-use-llm").addEventListener("click", () => {
  $("#strategy-select").value = "llm";
  renderStrategyParams();
  renderAiPage(lastConfig || {});
  window.location.hash = "/strategies";
});


// 全部币种: pull the whole perpetual market into the datalist.
$("#load-symbols").addEventListener("click", loadAllSymbols);
$("#account-refresh").addEventListener("click", refreshAccount);

// Arming real orders reveals the confirmation box and is deliberately noisy:
// this is the only control in the UI that can move real money.
$('[name="execute"]').addEventListener("change", (event) => {
  $("#live-confirm").hidden = !event.target.checked;
  if (!event.target.checked) $('[name="confirm"]').value = "";
  renderKeyHint();
});

$("#rail-toggle").addEventListener("click", () => setRail(!railOpen()));
$("#setup-shortcut").addEventListener("click", () => { window.location.hash = "/strategies"; });
$("#rail-close").addEventListener("click", () => setRail(false));
$("#rail-backdrop").addEventListener("click", () => setRail(false));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") setRail(false);
  if (event.key === "Tab" && railOpen()) {
    const focusable = $$("button:not(:disabled), input:not(:disabled), select:not(:disabled)", $("#rail")).filter(el => el.getClientRects().length);
    const first = focusable[0], last = focusable.at(-1);
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
    if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
  }
});

$$(".tab").forEach((tab) => {
  tab.addEventListener("click", () => { activeTab = tab.dataset.tab; renderTab(); });
});

$("#run-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (session?.running || busy) return;
  const request = collectRequest();
  busy = true;
  setRunningUi(false);
  setStatus("正在初始化交易会话：加载行情、恢复状态、校验持仓…");
  try {
    const payload = await api("/api/session/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    setRail(false);
    renderSession(payload);
    setRunningUi(payload.running);
    setStatus("");
    window.location.hash = "/market";
    schedulePoll(3000);
  } catch (error) {
    setStatus(error.message, "error");
    setRunningUi(false);
    await refreshSession();
  } finally {
    busy = false;
    setRunningUi(Boolean(session?.running));
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

async function stopSession() {
  if (busy) return;
  busy = true;
  $("#stop-button").disabled = true;
  setStatus("正在停止并保存会话状态…");
  try {
    const payload = await api("/api/session/stop", { method: "POST" });
    renderSession(payload);
    setStatus("");
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    busy = false;
    setRunningUi(Boolean(session?.running));
  }
}

async function recoverProtection() {
  if (busy) return;
  busy = true;
  const confirm = $("#recovery-confirm").value;
  $("#recovery-confirm-button").disabled = true;
  setStatus("正在只读核验 Binance 持仓与原始保护单 ID…");
  try {
    const payload = await api("/api/session/recover", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({confirm}),
    });
    $("#recovery-dialog").close();
    $("#recovery-confirm").value = "";
    renderSession(payload);
    setStatus("保护单恢复锁已解除；策略仍未启动，请重新核对参数后手动启动。");
  } catch (error) {
    setStatus(`恢复失败：${error.message}`, "error");
  } finally {
    busy = false;
    $("#recovery-confirm-button").disabled = false;
    setRunningUi(Boolean(session?.running));
  }
}

$("#stop-button").addEventListener("click", () => {
  if (session?.position?.open) $("#stop-dialog").showModal();
  else stopSession();
});
$("#stop-cancel").addEventListener("click", () => $("#stop-dialog").close());
$("#stop-confirm").addEventListener("click", () => { $("#stop-dialog").close(); stopSession(); });
$("#recovery-button").addEventListener("click", () => {
  $("#recovery-confirm").value = "";
  $("#recovery-dialog").showModal();
});
$("#recovery-cancel").addEventListener("click", () => $("#recovery-dialog").close());
$("#recovery-confirm-button").addEventListener("click", recoverProtection);
$('[name="symbol"]').addEventListener("change", () => { market = null; refreshMarket(); });
$$('[data-interval]').forEach(button => {
  button.addEventListener("click", () => {
    marketInterval = button.dataset.interval;
    $$('[data-interval]').forEach(b => b.classList.toggle("active", b === button));
    refreshMarket();
  });
});

// ------------------------------------------------------------------- auth wiring
$("#auth-tab-login").addEventListener("click", () => setAuthTab("login"));
$("#auth-tab-register").addEventListener("click", () => setAuthTab("register"));
$("#auth-form").addEventListener("submit", submitAuth);
$("#logout-button").addEventListener("click", logout);
$("#credentials-form").addEventListener("submit", event => { event.preventDefault(); saveCredentials(); });
// The desktop live switch is a separate control from the credential form: the
// change event fires on click and posts only the flag.
$("#local-live")?.addEventListener("change", (event) => toggleLocalLive(event.target.checked));
$("#load-models")?.addEventListener("click", loadModels);
$("#settings-login").addEventListener("click", () => {
  setRail(false);
  $("#auth-card").scrollIntoView({ block: "start" });
  $("#auth-username").focus();
});
document.addEventListener("invalid", event => {
  if (event.target.form !== $("#run-form")) return;
  window.location.hash = event.target.closest(".market-trade") ? "/market" : "/strategies";
  renderPage();
  let node = event.target.parentElement;
  while (node) { if (node.tagName === "DETAILS") node.open = true; node = node.parentElement; }
}, true);

// Hash routes keep one form instance: navigation never discards a draft.
function renderPage() {
  const page = window.location.hash === "#/strategies" ? "strategies"
    : window.location.hash === "#/ai" ? "ai" : "market";
  const strategiesPage = page === "strategies";
  const aiPage = page === "ai";
  $(".board").classList.toggle("strategy-page", strategiesPage || aiPage);
  $(".board").classList.toggle("ai-page-active", aiPage);
  $("#trade-config").hidden = !strategiesPage;
  $("#ai-page").hidden = !aiPage;
  $$(".board > .market, .board > .account, .board > .position, .board > .log, .board > .risk").forEach(el => { el.hidden = page !== "market"; });
  $("#setup-shortcut").hidden = page !== "market";
  $("#nav-market").setAttribute("aria-current", page === "market" ? "page" : "false");
  $("#nav-strategies").setAttribute("aria-current", strategiesPage ? "page" : "false");
  $("#nav-ai").setAttribute("aria-current", aiPage ? "page" : "false");
  $("#strategy-target").textContent = `${$('[name="symbol"]').value} · USDT 永续`;
  document.title = aiPage ? "AI 决策 · 交易工作台" : strategiesPage ? "策略与风控 · 交易工作台" : "交易总览 · 交易工作台";
}
window.addEventListener("hashchange", () => { renderPage(); window.scrollTo(0, 0); });
renderPage();

bootstrap().catch((error) => setStatus(`无法加载配置：${error.message}`, "error"));
