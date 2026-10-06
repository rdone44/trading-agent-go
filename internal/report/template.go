package report

// htmlTemplate is the self-contained report page: no CDN, no external assets,
// so it renders offline. The equity curve is drawn with inline SVG.
const htmlTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{ .Result.Symbol }} 回测报告 - {{ .Result.Strategy }}</title>
<style>
  :root {
    --bg: #f4f1ea; --panel: #ffffff; --ink: #1d232b; --muted: #6b7280;
    --line: #e3ded2; --up: #0f766e; --down: #b42318;
  }
  * { box-sizing: border-box; }
  body { margin: 0; background: var(--bg); color: var(--ink);
         font: 15px/1.55 "Segoe UI", "Helvetica Neue", sans-serif; }
  .wrap { max-width: 1040px; margin: 0 auto; padding: 40px 24px 64px; }
  header { border-bottom: 2px solid var(--ink); padding-bottom: 20px; margin-bottom: 28px; }
  h1 { font-size: 30px; margin: 0 0 6px; letter-spacing: -0.02em; }
  .sub { color: var(--muted); font-size: 14px; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; }
  .card { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; }
  .card .k { font-size: 12px; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); }
  .card .v { font-size: 22px; font-weight: 600; margin-top: 4px; }
  .pos { color: var(--up); } .neg { color: var(--down); }
  section { margin-top: 34px; }
  h2 { font-size: 18px; margin: 0 0 12px; }
  .chart { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 12px; }
  table { width: 100%; border-collapse: collapse; background: var(--panel);
          border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
  th, td { padding: 8px 12px; text-align: right; border-bottom: 1px solid var(--line);
           font-variant-numeric: tabular-nums; }
  th:first-child, td:first-child { text-align: left; }
  th { background: #faf8f3; font-size: 12px; text-transform: uppercase;
       letter-spacing: .05em; color: var(--muted); }
  tr:last-child td { border-bottom: none; }
  .note { color: var(--muted); font-size: 13px; margin-top: 26px;
          border-top: 1px solid var(--line); padding-top: 14px; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>{{ .Result.Symbol }} &middot; {{ .Result.Strategy }}</h1>
    <div class="sub">{{ .Result.DataSource }} 数据 &middot;
      {{ .Result.Start.Format "2006-01-02" }} &rarr; {{ .Result.End.Format "2006-01-02" }}
      &middot; {{ .Result.Bars }} 根K线 &middot; 生成于 {{ .Generated }}</div>
  </header>

  <div class="cards">
    <div class="card"><div class="k">总收益率</div>
      <div class="v {{ if .Positive }}pos{{ else }}neg{{ end }}">{{ .TotalReturn }}</div></div>
    <div class="card"><div class="k">期末权益</div><div class="v">{{ .FinalEquity }}</div></div>
    <div class="card"><div class="k">最大回撤</div><div class="v neg">{{ .MaxDrawdown }}</div></div>
    <div class="card"><div class="k">夏普比率</div><div class="v">{{ .Sharpe }}</div></div>
    <div class="card"><div class="k">成交笔数</div><div class="v">{{ .Metrics.NumTrades }}</div></div>
    <div class="card"><div class="k">胜率</div><div class="v">{{ .WinRate }}</div></div>
  </div>

  <section>
    <h2>资金曲线</h2>
    <div class="chart">
      <svg id="equity" viewBox="0 0 900 260" width="100%" height="260"
           role="img" aria-label="资金曲线"></svg>
    </div>
  </section>

  <section>
    <h2>成交明细</h2>
    {{ if eq .Metrics.NumTrades 0 }}
      <p>本次回测没有已平仓的交易。</p>
    {{ else }}
    <table>
      <tr><th>开仓日</th><th>平仓日</th><th>方向</th><th>数量</th><th>开仓价</th>
          <th>平仓价</th><th>盈亏</th><th>离场原因</th></tr>
      {{ range .Result.Trades }}
      <tr>
        <td>{{ .EntryTime.Format "2006-01-02" }}</td>
        <td>{{ .ExitTime.Format "2006-01-02" }}</td>
        <td>{{ sideLabel .Side }}</td>
        <td>{{ printf "%.4f" .Quantity }}</td>
        <td>{{ printf "%.2f" .EntryPrice }}</td>
        <td>{{ printf "%.2f" .ExitPrice }}</td>
        <td class="{{ if ge .PnL 0.0 }}pos{{ else }}neg{{ end }}">{{ printf "%.2f" .PnL }}</td>
        <td>{{ reasonLabel .Reason }}</td>
      </tr>
      {{ end }}
    </table>
    {{ end }}
  </section>

  <section>
    <h2>风控事件</h2>
    {{ if eq (len .Result.RiskEvents) 0 }}
      <p>本次回测没有触发任何风控限制。</p>
    {{ else }}
    <table>
      <tr><th>时间</th><th>类型</th><th>详情</th></tr>
      {{ range .Result.RiskEvents }}
      <tr><td>{{ .Time.Format "2006-01-02 15:04" }}</td><td>{{ riskTypeLabel .Type }}</td>
          <td>{{ reasonLabel .Reason }}</td></tr>
      {{ end }}
    </table>
    {{ end }}
  </section>

  <p class="note">本页为模拟结果，成本与成交假设已做简化。不构成任何投资建议。</p>
</div>

<script>
  const points = {{ .PointsJS }};
  const svg = document.getElementById('equity');
  const W = 900, H = 260, PAD = 28;
  if (points.length > 1) {
    const values = points.map(p => p.equity);
    const min = Math.min(...values), max = Math.max(...values);
    const span = (max - min) || 1;
    const x = i => PAD + (W - 2 * PAD) * i / (points.length - 1);
    const y = v => H - PAD - (H - 2 * PAD) * (v - min) / span;
    const line = points.map((p, i) =>
      (i ? 'L' : 'M') + x(i).toFixed(1) + ',' + y(p.equity).toFixed(1)).join(' ');
    const area = line + ' L' + x(points.length - 1).toFixed(1) + ',' + (H - PAD) +
                 ' L' + PAD + ',' + (H - PAD) + ' Z';
    const up = values[values.length - 1] >= values[0];
    const stroke = up ? '#0f766e' : '#b42318';
    svg.innerHTML =
      '<defs><linearGradient id="fill" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + stroke + '" stop-opacity="0.22"/>' +
      '<stop offset="100%" stop-color="' + stroke + '" stop-opacity="0.02"/>' +
      '</linearGradient></defs>' +
      '<line x1="' + PAD + '" y1="' + y(values[0]) + '" x2="' + (W - PAD) + '" y2="' + y(values[0]) +
      '" stroke="#c9c3b6" stroke-dasharray="4 4"/>' +
      '<path d="' + area + '" fill="url(#fill)"/>' +
      '<path d="' + line + '" fill="none" stroke="' + stroke + '" stroke-width="2"/>' +
      '<text x="' + PAD + '" y="' + (PAD - 8) + '" font-size="12" fill="#6b7280">期初 ' +
      values[0].toLocaleString() + '</text>' +
      '<text x="' + (W - PAD) + '" y="' + (PAD - 8) +
      '" text-anchor="end" font-size="12" fill="#6b7280">期末 ' +
      values[values.length - 1].toLocaleString() + '</text>';
  }
</script>
</body>
</html>
`
