'use strict';

// Palette chosen to stay distinguishable in both themes and for the common
// forms of colour blindness; hue order is deliberate, not incidental.
const COLORS = ['#2f6f4e', '#2b6ca3', '#a5561b', '#7a4fa3', '#a32c2c'];

let rangeSec = 21600;
let rttPlot = null;
let lossPlot = null;
let wifiPlot = null;
let throughputPlot = null;

const $ = (id) => document.getElementById(id);

function fmtMs(us) {
  if (!us || us <= 0) return '–';
  const ms = us / 1000;
  return ms < 10 ? ms.toFixed(1) + 'ms' : Math.round(ms) + 'ms';
}

function fmtPct(x) {
  return (x * 100).toFixed(x < 0.01 ? 2 : 1) + '%';
}

function themeColors() {
  const cs = getComputedStyle(document.body);
  return {
    ink: cs.getPropertyValue('--ink').trim(),
    muted: cs.getPropertyValue('--muted').trim(),
    line: cs.getPropertyValue('--line').trim(),
    gap: cs.getPropertyValue('--gap').trim(),
  };
}

// Gaps are drawn as shaded regions rather than being interpolated across. A
// line joined over an eight-hour suspend would claim measurements that were
// never taken.
function gapPlugin(gaps) {
  return {
    hooks: {
      draw: (u) => {
        if (!gaps || !gaps.length) return;
        const { ctx } = u;
        const t = themeColors();
        ctx.save();
        ctx.fillStyle = t.gap;
        for (const g of gaps) {
          const x0 = u.valToPos(g.from, 'x', true);
          const x1 = u.valToPos(g.to, 'x', true);
          if (x1 < u.bbox.left || x0 > u.bbox.left + u.bbox.width) continue;
          const a = Math.max(x0, u.bbox.left);
          const b = Math.min(x1, u.bbox.left + u.bbox.width);
          ctx.fillRect(a, u.bbox.top, Math.max(b - a, 1), u.bbox.height);
        }
        ctx.restore();
      },
    },
  };
}

// Labels only decades and their 2x/5x subdivisions. A log axis otherwise
// emits every minor tick and the labels overprint into an unreadable smear.
function isNiceLogTick(v) {
  if (!(v > 0)) return false;
  const e = Math.log10(v);
  if (Math.abs(e - Math.round(e)) < 1e-9) return true;
  const m = v / Math.pow(10, Math.floor(e + 1e-9));
  return Math.abs(m - 2) < 0.01 || Math.abs(m - 5) < 0.01;
}

// Wilson score interval, matching the server's implementation so that
// re-bucketed client-side values agree with the API's.
function wilson(lost, sent, z = 1.96) {
  if (sent === 0) return [0, 0];
  const p = lost / sent;
  const z2 = z * z;
  const denom = 1 + z2 / sent;
  const centre = p + z2 / (2 * sent);
  const spread = z * Math.sqrt((p * (1 - p)) / sent + z2 / (4 * sent * sent));
  return [Math.max(0, (centre - spread) / denom), Math.min(1, (centre + spread) / denom)];
}

// Widens loss buckets until each holds at least this many packets.
//
// Loss at fine resolution is mostly binomial noise: twenty packets a minute
// puts the 95% upper bound near 20% even on a flawless link, which paints the
// whole chart as a solid block of alarming shading. Widening the bucket until
// there are enough samples to say anything is the honest fix - the chart
// loses time resolution it never really had.
const LOSS_MIN_SAMPLES = 100;

// Coalesces every target onto ONE shared, evenly spaced time grid.
//
// Merging each target independently until it reached the sample threshold put
// every series on slightly different timestamps. uPlot needs a single x array,
// so the union of those timestamps left most series null at most positions and
// the chart degenerated into scattered bars with nothing joining them.
function coalesceLossAligned(series, minSamples) {
  const stamps = new Set();
  for (const s of series) for (const b of s.buckets || []) stamps.add(b.ts);
  const xs = [...stamps].sort((a, b) => a - b);
  if (!xs.length) return { xs: [], los: [], his: [] };

  const index = new Map(xs.map((t, i) => [t, i]));
  const sent = series.map(() => new Array(xs.length).fill(0));
  const lost = series.map(() => new Array(xs.length).fill(0));

  series.forEach((s, si) => {
    for (const b of s.buckets || []) {
      const i = index.get(b.ts);
      sent[si][i] += b.sent;
      lost[si][i] += b.lost;
    }
  });

  // One group size for every series, so the grid stays uniform and
  // comparable across targets. Size it from the *typical* per-bucket rate,
  // not the busiest bucket: sizing off the peak leaves the average series
  // short of the threshold the caller asked for.
  const rates = [];
  for (const row of sent) for (const v of row) if (v > 0) rates.push(v);
  rates.sort((a, b) => a - b);
  const typical = rates.length ? rates[Math.floor(rates.length / 2)] : 1;
  const groupSize = Math.max(1, Math.ceil(minSamples / Math.max(typical, 1)));

  const gxs = [];
  const los = series.map(() => []);
  const his = series.map(() => []);

  for (let start = 0; start < xs.length; start += groupSize) {
    const end = Math.min(start + groupSize, xs.length);
    gxs.push(xs[start]);
    series.forEach((_, si) => {
      let ns = 0;
      let nl = 0;
      for (let i = start; i < end; i++) {
        ns += sent[si][i];
        nl += lost[si][i];
      }
      if (ns === 0) {
        los[si].push(null);
        his[si].push(null);
        return;
      }
      const [lo, hi] = wilson(nl, ns);
      los[si].push(lo * 100);
      his[si].push(hi * 100);
    });
  }
  return { xs: gxs, los, his, groupSize };
}

function rgba(hex, a) {
  const n = parseInt(hex.slice(1), 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${a})`;
}

// Renders our own legend: one entry per target, rather than uPlot's, which
// would list the hidden upper-bound series too and double the entries.
function renderLegend(el, series, colors = COLORS) {
  el.innerHTML = series
    .map((s, i) => `<span><i style="background:${colors[i % colors.length]}"></i>${s.label}</span>`)
    .join('');
}

function tooltipPlugin(fmt) {
  let el;
  return {
    hooks: {
      init: (u) => {
        el = document.createElement('div');
        el.className = 'u-tooltip';
        el.style.display = 'none';
        u.over.appendChild(el);
      },
      setCursor: (u) => {
        const { idx, left, top } = u.cursor;
        if (idx == null || left < 0) {
          el.style.display = 'none';
          return;
        }
        const rows = [];
        for (let s = 1; s < u.series.length; s++) {
          const ser = u.series[s];
          if (ser.show === false || ser._hideTip) continue;
          const v = u.data[s][idx];
          if (v == null) continue;
          rows.push(`<div><span style="color:${ser.stroke()}">■</span> ${ser.label}: ${fmt(v)}</div>`);
        }
        if (!rows.length) {
          el.style.display = 'none';
          return;
        }
        const when = new Date(u.data[0][idx] * 1000).toLocaleString();
        el.innerHTML = `<div style="color:var(--muted);margin-bottom:.2rem">${when}</div>` + rows.join('');
        el.style.display = 'block';
        const width = u.over.clientWidth || u.bbox.width / devicePixelRatio;
        const height = u.over.clientHeight || u.bbox.height / devicePixelRatio;
        const gap = 12;
        const x = left + el.offsetWidth + gap <= width
          ? left + gap
          : left - el.offsetWidth - gap;
        const y = top + el.offsetHeight + gap <= height
          ? top + gap
          : top - el.offsetHeight - gap;
        el.style.left = Math.max(0, Math.min(x, width - el.offsetWidth)) + 'px';
        el.style.top = Math.max(0, Math.min(y, height - el.offsetHeight)) + 'px';
      },
    },
  };
}

function baseOpts(el, fmtY, plugins) {
  const t = themeColors();
  return {
    width: el.clientWidth || 900,
    height: 260,
    // Left padding is explicit so the widest y-axis label is not clipped.
    padding: [12, 12, 0, 8],
    plugins,
    cursor: { y: false, drag: { x: true, y: false } },
    axes: [
      { stroke: t.muted, grid: { stroke: t.line, width: 1 }, ticks: { stroke: t.line } },
      {
        stroke: t.muted,
        size: 58,
        grid: { stroke: t.line, width: 1 },
        ticks: { stroke: t.line },
        values: (u, vals) => vals.map(fmtY),
      },
    ],
    legend: { show: false },
  };
}

async function load() {
  const to = Math.floor(Date.now() / 1000);
  const from = to - rangeSec;

  const [seriesRes, eventsRes, summaryRes, layersRes] = await Promise.all([
    fetch(`/api/series?from=${from}&to=${to}&points=1000`).then((r) => r.json()),
    fetch(`/api/events?from=${from}&to=${to}`).then((r) => r.json()),
    fetch(`/api/summary?window=${rangeSec}s`).then((r) => r.json()),
    fetch(`/api/layers?from=${from}&to=${to}`).then((r) => r.json()),
  ]);

  renderHeader(summaryRes, seriesRes);
  renderTiles(summaryRes);
  renderRTT(seriesRes);
  renderLoss(seriesRes);
  renderWiFi(layersRes, seriesRes);
  renderThroughput(layersRes.throughput || [], from, to);
  renderBufferbloat(layersRes.throughput || []);
  renderLayers(layersRes);
  renderEvents(eventsRes);
}

// SNR bands, in dB. These are link-quality thresholds rather than opinions:
// below about 15 dB a Wi-Fi link starts dropping to slower modulations and
// retransmitting, which shows up as jitter long before it shows up as loss.
function snrQuality(snr) {
  if (snr >= 40) return { label: 'excellent', color: COLORS[0] };
  if (snr >= 25) return { label: 'good', color: COLORS[0] };
  if (snr >= 15) return { label: 'fair', color: COLORS[2] };
  return { label: 'poor', color: COLORS[4] };
}

function renderWiFi(layers, seriesRes) {
  const card = document.getElementById('wifi-card');
  const pts = layers.wifi || [];
  const now = layers.wifiNow;

  if (!now && !pts.length) {
    // Ethernet, radio off, or a build without cgo: say nothing rather than
    // showing an empty chart that implies a missing measurement.
    card.hidden = true;
    return;
  }
  card.hidden = false;

  if (now) {
    const q = snrQuality(now.snr);
    $('wifi-now').innerHTML =
      `<div class="bloat-row"><span class="name">now</span>` +
      `<span class="val" style="color:${q.color}">${now.rssi} dBm · ` +
      `SNR ${now.snr} dB (${q.label}) · ${Math.round(now.txRate)} Mbit/s</span></div>`;
  }

  const el = $('chart-wifi');
  if (pts.length < 2) {
    el.innerHTML = '<p class="note">Not enough radio samples yet — collected every 30 seconds.</p>';
    return;
  }

  const xs = pts.map((p) => p.ts);
  const snr = pts.map((p) => p.snr);
  const rssi = pts.map((p) => p.rssi);

  const opts = baseOpts(el, (v) => v + ' dB', [gapPlugin(seriesRes.gaps),
    tooltipPlugin((v) => v.toFixed(0))]);
  opts.height = 180;
  opts.scales = { x: { time: true, range: () => [seriesRes.from, seriesRes.to] } };
  opts.series = [
    {},
    { label: 'SNR (dB)', stroke: COLORS[0], width: 1.7, spanGaps: false },
    { label: 'RSSI (dBm)', stroke: COLORS[1], width: 1.2, dash: [3, 3], scale: 'dbm', spanGaps: false },
  ];
  // The right-hand axis needs an explicit size, or "-50 dBm" is clipped to
  // "-50 dBrr" against the plot edge.
  opts.axes.push({
    scale: 'dbm',
    side: 1,
    size: 66,
    stroke: themeColors().muted,
    grid: { show: false },
    values: (u, vals) => vals.map((v) => v + ' dBm'),
  });
  opts.padding = [12, 4, 0, 8];

  el.innerHTML = '';
  wifiPlot = new uPlot(opts, [xs, snr, rssi], el);
}

function renderThroughput(rows, from, to) {
  const card = document.getElementById('throughput-card');
  const host = $('throughput');
  const points = rows
    .filter((r) => r.downMbps > 0 || r.upMbps > 0)
    .sort((a, b) => a.ts - b.ts);
  if (!points.length) {
    card.hidden = true;
    return;
  }
  card.hidden = false;
  const latest = points[points.length - 1];
  const xs = points.map((r) => r.ts);
  const down = points.map((r) => (r.downMbps > 0 ? r.downMbps : null));
  const up = points.map((r) => (r.upMbps > 0 ? r.upMbps : null));
  const chart = $('chart-throughput');
  const opts = baseOpts(chart, (v) => v.toFixed(0) + ' Mbit/s', [
    tooltipPlugin((v) => v.toFixed(1) + ' Mbit/s'),
  ]);
  opts.height = 220;
  opts.scales = { x: { time: true, range: () => [from, to] } };
  opts.axes[1].size = 88;
  opts.series = [
    {},
    { label: 'Download', stroke: COLORS[1], width: 1.7, points: { show: points.length < 3 } },
    { label: 'Upload', stroke: COLORS[2], width: 1.7, points: { show: points.length < 3 } },
  ];
  renderLegend($('legend-throughput'), [
    { label: 'Download' },
    { label: 'Upload' },
  ], [COLORS[1], COLORS[2]]);
  chart.innerHTML = '';
  throughputPlot = new uPlot(opts, [xs, down, up], chart);
  host.innerHTML =
    `<div class="throughput-values">` +
    `<div><span class="label">download</span><strong>${latest.downMbps.toFixed(1)} Mbit/s</strong></div>` +
    `<div><span class="label">upload</span><strong>${latest.upMbps.toFixed(1)} Mbit/s</strong></div>` +
    `</div>` +
    `<p class="note">${new Date(latest.ts * 1000).toLocaleString()}</p>`;
}

// Draws idle and loaded latency as bars on one shared scale.
//
// "20ms idle, 2105ms loaded" is a pair of numbers most people skim past. The
// same pair drawn to scale is a sliver next to a bar crossing the page, and
// needs no explanation at all.
function renderBufferbloat(rows) {
  const card = document.getElementById('bloat-card');
  const host = $('bloat');
  const latest = rows.find((r) => r.baseRttUs > 0 && r.loadedP95Us > 0);
  if (!latest) {
    card.hidden = true;
    return;
  }
  card.hidden = false;

  const idle = latest.baseRttUs / 1000;
  const loaded = latest.loadedP95Us / 1000;
  const added = loaded - idle;
  const max = Math.max(loaded, idle, 1);
  const pct = (v) => Math.max((v / max) * 100, 0.4);
  const verdict = bloatVerdict(added);

  host.innerHTML =
    `<div class="bloat-row"><span class="name">idle</span>` +
    `<span class="bar bloat-idle" style="width:${pct(idle)}%"></span>` +
    `<span class="val">${fmtDur(idle)}</span></div>` +
    `<div class="bloat-row"><span class="name">under load (p95)</span>` +
    `<span class="bar bloat-loaded" style="width:${pct(loaded)}%"></span>` +
    `<span class="val">${fmtDur(loaded)}</span></div>` +
    `<p class="note">${latest.downMbps.toFixed(1)} Mbit/s down · ` +
    `${latest.upMbps.toFixed(1)} Mbit/s up` +
    (latest.downRpm > 0 ? ` · ${latest.downRpm} RPM` : '') +
    ` · ${new Date(latest.ts * 1000).toLocaleString()}</p>` +
    `<div class="verdict ${verdict.ok ? 'ok' : ''}">` +
    `<strong>${verdict.title}</strong>${verdict.body}</div>`;
}

function fmtDur(ms) {
  return ms >= 1000 ? (ms / 1000).toFixed(2) + 's' : Math.round(ms) + 'ms';
}

function bloatVerdict(addedMs) {
  if (addedMs < 30) {
    return { ok: true, title: 'Excellent', body: 'The link stays responsive while saturated.' };
  }
  if (addedMs < 100) {
    return { ok: true, title: 'Good', body: 'Calls should hold up during large transfers.' };
  }
  if (addedMs < 250) {
    return {
      ok: false,
      title: `Noticeable — ${Math.round(addedMs)}ms added under load`,
      body: 'Video calls will stutter during big uploads or downloads.',
    };
  }
  return {
    ok: false,
    title: `Severe — ${fmtDur(addedMs)} added under load`,
    body:
      'Any large transfer will make calls and browsing feel broken. This is a router ' +
      'queueing problem rather than a line speed problem: enabling SQM / fq_codel on ' +
      'the router fixes it, and needs nothing from your ISP.',
  };
}

// Renders DNS and HTTP timings side by side.
//
// This is the attribution table: when everything "feels slow", it says which
// layer is actually slow, and comparing the router's resolver against public
// ones is often the whole answer.
function renderLayers(res) {
  const host = $('layers');
  const parts = [];

  const dns = res.dns || [];
  if (dns.length) {
    const rows = dns
      .map(
        (d) =>
          `<tr><td>${d.resolver}</td><td>${d.cold ? 'cold' : 'cached'}</td>` +
          `<td>${fmtDur(d.meanUs / 1000)}</td><td>${d.n}</td>` +
          `<td>${d.failures || '–'}</td></tr>`,
      )
      .join('');
    parts.push(
      `<h3>DNS resolution</h3><table class="layers-t">` +
        `<tr><th>Resolver</th><th>Kind</th><th>Mean</th><th>n</th><th>Failures</th></tr>` +
        rows +
        `</table>` +
        `<p class="note">"cold" uses a random name so it cannot be answered from cache; ` +
        `"cached" repeats a popular one. Your router being much slower than 1.1.1.1 or ` +
        `8.8.8.8 is a router problem, not a line problem.</p>`,
    );
  }

  const http = res.http || [];
  if (http.length) {
    const rows = http
      .map(
        (h) =>
          `<tr><td>${shortURL(h.url)}</td><td>${fmtDur(h.dnsUs / 1000)}</td>` +
          `<td>${fmtDur(h.connectUs / 1000)}</td><td>${fmtDur(h.tlsUs / 1000)}</td>` +
          `<td>${fmtDur(h.ttfbUs / 1000)}</td><td>${h.n}</td></tr>`,
      )
      .join('');
    parts.push(
      `<h3>HTTPS request phases</h3><table class="layers-t">` +
        `<tr><th>Endpoint</th><th>DNS</th><th>Connect</th><th>TLS</th><th>TTFB</th><th>n</th></tr>` +
        rows +
        `</table>` +
        `<p class="note">Every request opens a fresh connection, so these measure real setup ` +
        `cost. Note this reaches a CDN edge, not your ISP.</p>`,
    );
  }

  host.innerHTML = parts.length
    ? parts.join('')
    : '<p class="note">No DNS or HTTP samples yet — these are collected every 30 to 60 seconds.</p>';
}

function shortURL(u) {
  try {
    return new URL(u).host;
  } catch {
    return u;
  }
}

function renderHeader(summary, series) {
  $('net').textContent = summary.network
    ? `${summary.network} · ${summary.fingerprint}`
    : 'no network recorded yet';

  const grain = { 60: '1 minute', 3600: '1 hour', 86400: '1 day' }[series.grain] || series.grain + 's';
  $('meta').textContent = `Buckets of ${grain}. Latency figures are histogram quantiles (upper bounds), never averages.`;
}

function renderTiles(summary) {
  const host = $('tiles');
  host.innerHTML = '';
  if (!summary.targets || !summary.targets.length) {
    host.innerHTML = '<div class="tile"><div class="label">No data</div>' +
      '<div class="detail">The daemon aggregates once a minute.</div></div>';
    return;
  }
  for (const t of summary.targets) {
    const div = document.createElement('div');
    div.className = 'tile';
    // Thresholds are intentionally coarse. They flag "look here", not "this
    // is broken", and a tile should never be redder than the data warrants.
    if (t.LossLo > 0.02) div.classList.add('bad');
    else if (t.P99 > 250000) div.classList.add('warn');

    div.innerHTML =
      `<div class="label">${t.Label}</div>` +
      `<div class="value">${fmtMs(t.P50)}</div>` +
      `<div class="detail">p99 ${fmtMs(t.P99)} · jitter ${fmtMs(t.JitterM)}</div>` +
      `<div class="detail">loss ${fmtPct(t.LossLo)}–${fmtPct(t.LossHi)} · n=${t.Sent}</div>`;
    host.appendChild(div);
  }
}

// Aligns every target's buckets onto one shared timestamp axis, which uPlot
// requires and which the API does not guarantee (a target added mid-window
// has fewer buckets).
function alignSeries(series, pick) {
  const stamps = new Set();
  for (const s of series) for (const b of s.buckets || []) stamps.add(b.ts);
  const xs = [...stamps].sort((a, b) => a - b);
  const index = new Map(xs.map((t, i) => [t, i]));

  const cols = series.map((s) => {
    const col = new Array(xs.length).fill(null);
    for (const b of s.buckets || []) col[index.get(b.ts)] = pick(b);
    return col;
  });
  return { xs, cols };
}

// Builds a chart where each target contributes a visible line plus a shaded
// region reaching some upper bound. uPlot's `bands` fill between two series,
// so the upper series is added but drawn invisibly - it exists only to give
// the band an edge, which is also why the legend is rendered separately.
function bandedChart(el, res, series, xs, lowCols, highCols, fmtY, fmtTip, extraOpts) {
  const n = series.length;

  const opts = baseOpts(el, fmtY, [gapPlugin(res.gaps), tooltipPlugin(fmtTip)]);

  // Pin the x-axis to the window that was requested rather than letting it
  // auto-range to the data. A freshly started daemon has a single bucket, and
  // uPlot given one point invents a span of several years. It is also simply
  // more honest: the chart shows the period asked for, so a partly-collected
  // window reads as mostly empty instead of silently zooming in.
  opts.scales = { x: { time: true, range: () => [res.from, res.to] } };
  opts.series = [{}];

  series.forEach((s, i) => {
    opts.series.push({
      label: s.label,
      stroke: COLORS[i % COLORS.length],
      width: 1.7,
      spanGaps: false,
    });
  });
  series.forEach((s, i) => {
    opts.series.push({
      label: s.label + ' (upper)',
      stroke: 'transparent',
      width: 0,
      points: { show: false },
      spanGaps: false,
      _hideTip: true, // the tooltip names the bound itself, not this series
    });
  });

  opts.bands = series.map((s, i) => ({
    series: [1 + n + i, 1 + i],
    fill: rgba(COLORS[i % COLORS.length], 0.14),
  }));

  // extraOpts may add a y scale. Merge the two scale objects rather than
  // letting one replace the other, and pull `scales` out of the copy first:
  // assigning `undefined` to it would still copy the key and wipe what was
  // just merged.
  const { scales: extraScales, ...extra } = extraOpts || {};
  if (extraScales) {
    opts.scales = { ...opts.scales, ...extraScales };
  }
  Object.assign(opts, extra);

  // A single point draws nothing with lines alone.
  if (xs.length < 3) {
    for (let i = 1; i <= n; i++) opts.series[i].points = { show: true, size: 6 };
  }

  el.innerHTML = '';
  return new uPlot(opts, [xs, ...lowCols, ...highCols], el);
}

function renderRTT(res) {
  const el = $('chart-rtt');
  const series = res.series || [];
  renderLegend($('legend-rtt'), series);
  if (!series.length) {
    el.innerHTML = '<p class="note">No latency data in this range.</p>';
    return;
  }

  const p50 = alignSeries(series, (b) => (b.p50 > 0 ? b.p50 / 1000 : null));
  const p99 = alignSeries(series, (b) => (b.p99 > 0 ? b.p99 / 1000 : null));

  rttPlot = bandedChart(
    el, res, series, p50.xs, p50.cols, p99.cols,
    (v) => (isNiceLogTick(v) ? (v >= 1 ? v + 'ms' : Math.round(v * 1000) + 'µs') : ''),
    (v) => v.toFixed(1) + 'ms',
    {
      // Log scale: a router at 4ms and a stalled anycast probe at 900ms have
      // to share this axis, and on a linear scale the router is a flat line
      // pinned to zero - which is exactly the part worth seeing.
      scales: { y: { distr: 3 } },
    },
  );
}

function renderLoss(res) {
  const el = $('chart-loss');
  const series = res.series || [];
  renderLegend($('legend-loss'), series);
  if (!series.length) {
    el.innerHTML = '<p class="note">No loss data in this range.</p>';
    $('loss-note').textContent = '';
    return;
  }

  const g = coalesceLossAligned(series, LOSS_MIN_SAMPLES);

  lossPlot = bandedChart(
    el, res, series, g.xs, g.los, g.his,
    (v) => v.toFixed(v < 1 ? 2 : 1) + '%',
    (v) => v.toFixed(2) + '%',
    {
      // The scale has to contain the upper bound, not just the point
      // estimate, or the shading spills over the whole plot area.
      scales: { y: { range: (u, min, max) => [0, Math.max(max * 1.1, 0.5)] } },
    },
  );

  const total = series.reduce(
    (n, s) => n + (s.buckets || []).reduce((m, b) => m + b.sent, 0), 0);
  const perBucket = g.xs.length
    ? Math.round(total / series.length / g.xs.length)
    : 0;

  $('loss-note').textContent =
    `${total.toLocaleString()} packets in this window, about ${perBucket} per target ` +
    `per bucket. Buckets are widened to hold at least ${LOSS_MIN_SAMPLES} packets — at ` +
    `finer resolution a loss chart shows binomial noise rather than the connection. ` +
    `Shading is each target's 95% confidence interval, so a band that never reaches zero ` +
    `means the samples cannot rule out a little loss, not that loss was observed.`;
}

function renderEvents(events) {
  const ul = $('events');
  ul.innerHTML = '';
  if (!events || !events.length) {
    ul.innerHTML = '<li class="empty">Nothing recorded in this range.</li>';
    return;
  }
  for (const e of events) {
    const li = document.createElement('li');
    li.innerHTML =
      `<time>${new Date(e.ts * 1000).toLocaleString()}</time>` +
      `<span class="kind">${e.kind}</span>` +
      `<span>${e.detail || ''}</span>`;
    ul.appendChild(li);
  }
}

$('ranges').addEventListener('click', (ev) => {
  const btn = ev.target.closest('button');
  if (!btn) return;
  document.querySelectorAll('#ranges button').forEach((b) => b.classList.remove('on'));
  btn.classList.add('on');
  rangeSec = +btn.dataset.range;
  load();
});

addEventListener('resize', () => {
  for (const p of [rttPlot, lossPlot, wifiPlot, throughputPlot]) {
    if (p) {
      p.setSize({
        width: p.root.parentElement.clientWidth,
        height: p === wifiPlot ? 180 : 260,
      });
    }
  }
});

load();
setInterval(load, 30000);
