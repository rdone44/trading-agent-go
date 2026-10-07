"use strict";

// ------------------------------------------------------------------ helpers

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

const fmt = {
  money: (v) => (v == null ? "n/a" : v.toLocaleString(undefined, { maximumFractionDigits: 2, minimumFractionDigits: 2 })),
  num: (v, digits = 2) => (v == null ? "n/a" : v.toFixed(digits)),
  pct: (v, digits = 2) => (v == null ? "n/a" : `${v >= 0 ? "" : ""}${v.toFixed(digits)}%`),
  signedPct: (v, digits = 2) => (v == null ? "n/a" : `${v >= 0 ? "+" : ""}${v.toFixed(digits)}%`),
  qty: (v) => (v == null ? "n/a" : v.toLocaleString(undefined, { maximumFractionDigits: 4 })),
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
};

const riskTypeLabel = {
  halt: "熔断停止交易",
  daily_loss_limit: "单日亏损超限",
};

const metricLabel = {
  initial_cash: "初始资金",
  final_equity: "期末权益",
  total_return_pct: "总收益率",
  annual_return_pct: "年化收益率",
  annual_volatility_pct: "年化波动率",
  sharpe: "夏普比率",
  sortino: "索提诺比率",
  max_drawdown_pct: "最大回撤",
  calmar: "卡玛比率",
  exposure_pct: "持仓暴露",
  num_trades: "成交笔数",
  win_rate_pct: "胜率",
  profit_factor: "盈亏比",
  avg_win: "平均盈利",
  avg_loss: "平均亏损",
  expectancy: "单笔期望",
  total_fees: "总费用",
};

// translateReason covers the parameterised codes ("risk halt: ...") as well as
// the fixed ones, and falls back to the raw string so nothing is ever hidden.
function translateReason(reason) {
  if (!reason) return "";
  if (reason.startsWith("risk halt: ")) return `风控熔断：${translateReason(reason.slice("risk halt: ".length))}`;
  if (reasonLabel[reason]) return reasonLabel[reason];
  if (reason.startsWith("max drawdown breached")) return "触发最大回撤限制";
  if (reason.startsWith("daily loss ")) return "单日亏损超限，暂停开仓";
  return reason;
}

const signClass = (v) => (v == null ? "" : v >= 0 ? "pos" : "neg");

async function api(path, options) {
  const response = await fetch(path, options);
  const payload = await response.json().catch(() => ({ error: `HTTP ${response.status}` }));
  if (!response.ok) throw new Error(payload.error || `HTTP ${response.status}`);
  return payload;
}

// ------------------------------------------------------------------- charts

// lineChart renders a responsive SVG line chart with an optional filled area
// and a baseline marker. Kept deliberately small: no charting library.
function lineChart(container, points, options = {}) {
  const {
    value = (p) => p.equity,
    color = "#1f5f5b",
    fill = true,
    zeroLine = false,
    label = (v) => v.toFixed(0),
    height: H = 250,
  } = options;
  // The viewBox matches the rendered aspect ratio, so the SVG scales
  // uniformly and axis text is never stretched.
  const W = 900, PAD = { top: 14, right: 14, bottom: 22, left: 54 };

  if (!points || points.length < 2) {
    container.innerHTML = '<p class="muted">数据点不足，无法绘制曲线。</p>';
    return;
  }

  const values = points.map(value);
  let min = Math.min(...values);
  let max = Math.max(...values);
  if (zeroLine) { min = Math.min(min, 0); max = Math.max(max, 0); }
  if (min === max) { max = min + 1; }
  const pad = (max - min) * 0.08;
  min -= pad; max += pad;

  const x = (i) => PAD.left + (W - PAD.left - PAD.right) * (i / (points.length - 1));
  const y = (v) => PAD.top + (H - PAD.top - PAD.bottom) * (1 - (v - min) / (max - min));

  const line = points.map((p, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(value(p)).toFixed(1)}`).join(" ");
  const area = `${line} L${x(points.length - 1).toFixed(1)},${(H - PAD.bottom).toFixed(1)} L${PAD.left},${(H - PAD.bottom).toFixed(1)} Z`;

  // Four horizontal gridlines with value labels.
  const ticks = [];
  for (let i = 0; i <= 3; i++) {
    const v = min + ((max - min) * i) / 3;
    const yy = y(v);
    ticks.push(`<line x1="${PAD.left}" y1="${yy.toFixed(1)}" x2="${W - PAD.right}" y2="${yy.toFixed(1)}" stroke="#eceae4" stroke-width="1"/>`);
    ticks.push(`<text x="${PAD.left - 8}" y="${(yy + 3.5).toFixed(1)}" text-anchor="end" font-size="10.5" fill="#858b93" font-family="monospace">${label(v)}</text>`);
  }

  // First / last date labels.
  const first = points[0].t, last = points[points.length - 1].t;
  const axis = `
    <text x="${PAD.left}" y="${H - 4}" font-size="10.5" fill="#858b93" font-family="monospace">${first}</text>
    <text x="${W - PAD.right}" y="${H - 4}" text-anchor="end" font-size="10.5" fill="#858b93" font-family="monospace">${last}</text>`;

  const baseline = zeroLine && min < 0 && max > 0
    ? `<line x1="${PAD.left}" y1="${y(0).toFixed(1)}" x2="${W - PAD.right}" y2="${y(0).toFixed(1)}" stroke="#cfccc3" stroke-dasharray="4 4"/>`
    : "";

  const gradientId = `fill-${Math.random().toString(36).slice(2, 9)}`;
  container.innerHTML = `
    <svg viewBox="0 0 ${W} ${H}" role="img">
      <defs>
        <linearGradient id="${gradientId}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${color}" stop-opacity="0.20"/>
          <stop offset="100%" stop-color="${color}" stop-opacity="0.02"/>
        </linearGradient>
      </defs>
      ${ticks.join("")}
      ${baseline}
      ${fill ? `<path d="${area}" fill="url(#${gradientId})"/>` : ""}
      <path d="${line}" fill="none" stroke="${color}" stroke-width="1.8" stroke-linejoin="round"/>
      ${axis}
    </svg>`;
}

// --------------------------------------------------------------- form logic

let strategies = [];
let current = null;
let activeTab = "trades";

function strategyByName(name) {
  return strategies.find((s) => s.name === name);
}

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

function collectRequest() {
  const form = $("#run-form");
  const params = {};
  $$("[data-param]", form).forEach((input) => {
    const value = Number(input.value);
    if (Number.isFinite(value)) params[input.dataset.param] = value;
  });

  // The UI works in percent; the engine takes fractions.
  const pct = (name) => Number($(`[name="${name}"]`).value) / 100;

  return {
    symbol: $('[name="symbol"]').value.trim().toUpperCase(),
    strategy: $('[name="strategy"]').value,
    params,
    days: Number($('[name="days"]').value) || 0,
    initial_cash: Number($('[name="initial_cash"]').value) || 0,
    warmup_bars: 60,
    review: $('[name="review"]').checked,
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

// ------------------------------------------------------------------ render

function renderCards(m) {
  const cards = [
    { k: "总收益率", v: fmt.signedPct(m.total_return_pct), c: signClass(m.total_return_pct), d: `期末权益 ${fmt.money(m.final_equity)}` },
    { k: "年化收益", v: fmt.signedPct(m.annual_return_pct), c: signClass(m.annual_return_pct), d: `波动率 ${fmt.pct(m.annual_volatility_pct)}` },
    { k: "最大回撤", v: fmt.pct(m.max_drawdown_pct), c: "neg", d: `卡玛 ${fmt.num(m.calmar)}` },
    { k: "夏普比率", v: fmt.num(m.sharpe), c: signClass(m.sharpe), d: `索提诺 ${fmt.num(m.sortino)}` },
    { k: "成交笔数", v: String(m.num_trades ?? 0), d: `胜率 ${fmt.pct(m.win_rate_pct, 1)}` },
    { k: "盈亏比", v: fmt.num(m.profit_factor), c: signClass((m.profit_factor ?? 0) - 1), d: `单笔期望 ${fmt.money(m.expectancy)}` },
    { k: "持仓暴露", v: fmt.pct(m.exposure_pct, 1), d: "持有仓位的K线占比" },
    { k: "总费用", v: fmt.money(m.total_fees), d: "手续费 + 滑点" },
  ];
  $("#cards").innerHTML = cards
    .map((c) => `<div class="card"><div class="k">${c.k}</div><div class="v ${c.c || ""}">${c.v}</div><div class="d">${c.d}</div></div>`)
    .join("");
}

function table(headers, rows, emptyMessage) {
  if (!rows.length) return `<p class="muted">${emptyMessage}</p>`;
  const head = headers.map((h) => `<th class="${h.num ? "num" : ""}">${h.label}</th>`).join("");
  const body = rows.map((cells) => `<tr>${cells.join("")}</tr>`).join("");
  return `<div class="scroll"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
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
    `<td class="tag"><span class="pill ${t.reason.includes("stop") ? "stop" : ""}">${translateReason(t.reason)}</span></td>`,
  ]);
  return table(
    [{ label: "开仓日" }, { label: "平仓日" }, { label: "方向" }, { label: "数量", num: true },
     { label: "开仓价", num: true }, { label: "平仓价", num: true }, { label: "盈亏", num: true },
     { label: "收益率", num: true }, { label: "离场原因" }],
    rows,
    "没有已平仓的交易：该策略在这段区间内没有开过仓。"
  );
}

function renderOrders(orders) {
  const rows = orders.map((o) => [
    `<td>${o.time}</td>`,
    `<td class="tag"><span class="pill ${o.side === "buy" ? "long" : "short"}">${sideLabel[o.side] || o.side}</span></td>`,
    `<td class="num">${fmt.qty(o.quantity)}</td>`,
    `<td class="num">${fmt.money(o.price)}</td>`,
    `<td class="num">${fmt.money(o.quantity * o.price)}</td>`,
    `<td class="tag">${translateReason(o.reason)}${o.rejected ? ' <span class="pill stop">已拒单</span>' : ""}</td>`,
  ]);
  return table(
    [{ label: "日期" }, { label: "方向" }, { label: "数量", num: true }, { label: "成交价", num: true },
     { label: "成交额", num: true }, { label: "说明" }],
    rows,
    "本次回测没有发出任何委托。"
  );
}

function renderRisk(events) {
  const rows = events.map((e) => [
    `<td>${e.time}</td>`,
    `<td class="tag"><span class="pill ${e.type === "halt" ? "halt" : ""}">${riskTypeLabel[e.type] || e.type}</span></td>`,
    `<td class="tag">${translateReason(e.reason)}</td>`,
  ]);
  return table(
    [{ label: "日期" }, { label: "类型" }, { label: "详情" }],
    rows,
    "本次回测没有触发任何风控限制。"
  );
}

function renderMetrics(m) {
  const rows = Object.entries(m).map(([key, value]) => [
    `<td>${metricLabel[key] || key.replaceAll("_", " ")}<span class="muted"> · ${key}</span></td>`,
    `<td class="num">${value == null ? "n/a" : typeof value === "number" ? fmt.num(value, 6) : value}</td>`,
  ]);
  return table([{ label: "指标" }, { label: "数值", num: true }], rows, "没有指标数据。");
}

function renderTab() {
  if (!current) return;
  const body = $("#tab-body");
  if (activeTab === "trades") body.innerHTML = renderTrades(current.trades || []);
  if (activeTab === "orders") body.innerHTML = renderOrders(current.orders || []);
  if (activeTab === "risk") body.innerHTML = renderRisk(current.risk_events || []);
  if (activeTab === "metrics") body.innerHTML = renderMetrics(current.metrics || {});
  $$(".tab").forEach((tab) => tab.classList.toggle("active", tab.dataset.tab === activeTab));
}

function renderResult(payload) {
  current = payload;
  $("#empty-state").hidden = true;
  $("#results").hidden = false;

  $("#result-title").textContent = `${payload.symbol} · ${payload.strategy}`;
  $("#result-sub").textContent =
    `${payload.data_source} 数据 · ${payload.start} → ${payload.end} · ${payload.bars} 根K线 · 任务 ${payload.run_name}`;
  $("#report-link").href = payload.report_url;

  $("#trades-count").textContent = (payload.trades || []).length;
  $("#orders-count").textContent = (payload.orders || []).length;
  $("#risk-count").textContent = (payload.risk_events || []).length;

  renderCards(payload.metrics);

  const equity = payload.equity || [];
  if (equity.length > 1) {
    $("#equity-range").textContent =
      `${fmt.money(equity[0].equity)} → ${fmt.money(equity[equity.length - 1].equity)}`;
  }
  lineChart($("#equity-chart"), equity, {
    value: (p) => p.equity,
    color: payload.metrics.total_return_pct >= 0 ? "#0f766e" : "#b42318",
    label: (v) => v >= 1000 ? `${(v / 1000).toFixed(0)}k` : v.toFixed(0),
  });
  lineChart($("#drawdown-chart"), equity, {
    value: (p) => p.drawdown * 100,
    color: "#b42318",
    zeroLine: true,
    height: 150,
    label: (v) => `${v.toFixed(0)}%`,
  });

  renderReview(payload);
  renderTab();
}

// renderReview surfaces the LLM post-mortem if the run asked for one. A
// backtest that requested a review but could not reach a model shows the
// unavailability note instead of pretending nothing happened.
function renderReview(payload) {
  const panel = $("#review-panel");
  const text = $("#review-text");
  if (payload.review) {
    text.textContent = payload.review;
    text.classList.remove("unavailable");
    panel.hidden = false;
  } else if (payload.review_unavailable) {
    text.textContent = `复盘不可用：${payload.review_unavailable}`;
    text.classList.add("unavailable");
    panel.hidden = false;
  } else {
    panel.hidden = true;
    text.classList.remove("unavailable");
  }
}

async function loadRuns() {
  const host = $("#runs-list");
  try {
    const runs = await api("/api/runs");
    if (!runs.length) {
      host.innerHTML = '<p class="muted">还没有保存的回测记录，运行一次就会出现在这里。</p>';
      return;
    }
    host.innerHTML = runs.slice(0, 12).map((run) => {
      const ret = run.metrics.total_return_pct;
      return `
        <button class="run" data-run="${run.name}">
          <div class="top">
            <span class="sym">${run.symbol}</span>
            <span class="ret ${signClass(ret)}">${fmt.signedPct(ret)}</span>
          </div>
          <div class="meta">${run.strategy} · ${run.data_source}</div>
          <div class="meta">${run.start} → ${run.end} · ${run.bars} 根K线</div>
          <div class="meta">${run.metrics.num_trades} 笔成交 · 回撤 ${fmt.pct(run.metrics.max_drawdown_pct)}</div>
        </button>`;
    }).join("");
    $$(".run", host).forEach((button) => {
      button.addEventListener("click", () => openRun(button.dataset.run));
    });
  } catch (error) {
    host.innerHTML = `<p class="muted">无法读取历史记录：${error.message}</p>`;
  }
}

async function openRun(name) {
  setStatus(`正在载入 ${name}…`, "running");
  try {
    const payload = await api(`/api/run?name=${encodeURIComponent(name)}`);
    renderResult(payload);
    setStatus("");
    window.scrollTo({ top: 0, behavior: "smooth" });
  } catch (error) {
    setStatus(error.message, "error");
  }
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
  await loadRuns();
}

$("#strategy-select").addEventListener("change", renderStrategyParams);
$("#refresh-runs").addEventListener("click", loadRuns);
$$(".tab").forEach((tab) => {
  tab.addEventListener("click", () => { activeTab = tab.dataset.tab; renderTab(); });
});

$("#run-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#run-button");
  button.disabled = true;
  button.textContent = "回测中…";
  setStatus("正在加载行情、生成信号并回放撮合…", "running");
  try {
    const payload = await api("/api/backtest", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(collectRequest()),
    });
    renderResult(payload);
    setStatus("");
    await loadRuns();
    window.scrollTo({ top: 0, behavior: "smooth" });
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    button.disabled = false;
    button.textContent = "开始回测";
  }
});

$("#tune-button").addEventListener("click", async () => {
  const button = $("#tune-button");
  button.disabled = true;
  button.textContent = "调参中…";
  setStatus("LLM 正在提出并回测新的参数组合…", "running");
  try {
    const report = await api("/api/tune", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...collectRequest(), rounds: 4, objective: "sharpe", stall: 3 }),
    });
    renderTune(report);
    setStatus("");
    window.scrollTo({ top: 0, behavior: "smooth" });
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    button.disabled = false;
    button.textContent = "LLM 调参";
  }
});

// renderTune shows the tuning outcome: the best round vs. baseline up top,
// then the round-by-round log. When the model is not configured the loop
// reports the baseline and a "skipped" note, which we surface plainly.
function renderTune(report) {
  $("#results").hidden = true;
  $("#empty-state").hidden = true;
  $("#tune-results").hidden = false;

  $("#tune-sub").textContent =
    `${report.symbol} · ${report.strategy} · 目标 ${report.objective} · ` +
    `${report.rounds.length} 轮${report.early_stopped ? "（早停）" : ""}`;

  const improved = report.best_value > report.baseline;
  const cards = [
    { k: "基线 " + report.objective, v: fmt.num(report.baseline), c: signClass(report.baseline), d: "调参起点" },
    { k: "最佳 " + report.objective, v: fmt.num(report.best_value), c: signClass(report.best_value - report.baseline), d: improved ? `优于基线 ${fmt.num(report.best_value - report.baseline, 4)}` : "未超过基线" },
  ];
  $("#tune-cards").innerHTML = cards
    .map((c) => `<div class="card"><div class="k">${c.k}</div><div class="v ${c.c || ""}">${c.v}</div><div class="d">${c.d}</div></div>`)
    .join("");

  const rounds = report.rounds
    .map((r) => {
      const star = r.improved ? "★ " : "";
      const params = Object.entries(r.params || {})
        .sort((a, b) => a[0].localeCompare(b[0]))
        .map(([k, v]) => `${k}=${v}`)
        .join(" ");
      const note = r.note ? ` <span class="muted">· ${r.note}</span>` : "";
      const rationale = r.rationale ? `<div class="meta">${r.rationale}</div>` : "";
      return `<div class="tune-round ${r.improved ? "win" : ""}">
        <div class="top"><span class="sym">${r.index === 0 ? "基线" : "第 " + r.index + " 轮"}</span>
          <span class="ret ${signClass(r.objective_value)}">${fmt.num(r.objective_value)}</span></div>
        <div class="meta">${star}${params || "（默认参数）"}</div>${rationale}${note}
      </div>`;
    })
    .join("");
  $("#tune-rounds").innerHTML = rounds;

  const note = $("#tune-note");
  if (!report.llm_enabled) {
    note.textContent = "未配置 LLM：本轮只跑了基线，没有生成新参数。设置 LLM_API_KEY 后重新调参。";
    note.hidden = false;
  } else {
    note.hidden = true;
  }
}

bootstrap().catch((error) => setStatus(`无法加载配置：${error.message}`, "error"));
