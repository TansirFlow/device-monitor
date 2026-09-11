'use strict';

/* ============================================================
   服务器监控 · 只读看板
   纯原生 JS，无任何外部依赖。所有渲染数据都经过 HTML 转义——
   节点上报的内容属于不可信输入，即使节点被入侵也不能在这里执行脚本。
   ============================================================ */

const $ = (sel, root) => (root || document).querySelector(sel);
const esc = (v) => String(v === null || v === undefined ? '' : v)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;').replace(/'/g, '&#39;');

const COLORS = {
  cpu: '#3b82f6', mem: '#8b5cf6', disk: '#f59e0b',
  gpu: '#10b981', rx: '#06b6d4', tx: '#f97316',
  temp: '#f43f5e',
};

/* 温度传感器类别：与 agent 的 kind 字段一一对应。
   顺序即看板里的展示优先级（CPU 最关心，other 最后）。 */
const TEMP_KINDS = {
  cpu:   { label: 'CPU',  color: '#3b82f6' },
  gpu:   { label: 'GPU',  color: '#10b981' },
  disk:  { label: '磁盘', color: '#f59e0b' },
  board: { label: '主板', color: '#8b5cf6' },
  nic:   { label: '网卡', color: '#06b6d4' },
  other: { label: '其他', color: '#94a3b8' },
};
const TEMP_ORDER = ['cpu', 'gpu', 'disk', 'board', 'nic', 'other'];

const tempMeta = (k) => TEMP_KINDS[k] || TEMP_KINDS.other;

/* 温度阈值是"通用硬件"口径：电子元件长期 70℃ 以上就算偏高，
   85℃ 以上基本需要立刻介入。真正的临界值由 agent 的 crit_c 给出，
   卡片上的颜色只是快速视觉分级。 */
const tempLevel = (c) => (c >= 85 ? 'crit' : c >= 70 ? 'warn' : 'ok');
const fmtTemp = (c) => (Number(c) || 0).toFixed(0) + '°C';

const state = {
  token: localStorage.getItem('mon_token') || '',
  nodes: [],
  selected: null,
  minutes: 60,
  tokenPrompted: false,
  // 上一次画进抽屉的图表面板集合，用来判断节点是否上报了新的维度
  panelKeys: [],
};

/* ---------------- 主题 ---------------- */

function currentThemePref() { return localStorage.getItem('mon_theme') || 'auto'; }

function applyTheme() {
  const pref = currentThemePref();
  const dark = pref === 'dark' ||
    (pref === 'auto' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  const btn = $('#themeBtn');
  if (btn) btn.textContent = pref === 'auto' ? '主题·自动' : (pref === 'dark' ? '主题·暗色' : '主题·亮色');
}

function cycleTheme() {
  const order = ['auto', 'light', 'dark'];
  const next = order[(order.indexOf(currentThemePref()) + 1) % order.length];
  localStorage.setItem('mon_theme', next);
  applyTheme();
}

window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (currentThemePref() === 'auto') applyTheme();
});

/* ---------------- 格式化 ---------------- */

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ' + units[i];
}

function fmtPct(v) { return (Number(v) || 0).toFixed(1) + '%'; }

// cpuTopoText 把「物理核 / 逻辑线程」拼成一句人话。
//
// 三条规则，顺序很重要：
//   1. 两个数都有且不等 -> "8 核 16 线程"（典型的超线程机器）
//   2. 两个数相等       -> "8 核"（没有超线程，或线程数没上报）
//   3. 只有线程数       -> "16 线程"
// 规则 3 不能退化成 "16 核"：agent 识别不出物理核心时报的就是这种情况，
// 拿线程数冒充核心数正好会让 8 核 16 线程的机器看起来是 16 核。
function cpuTopoText(cpu) {
  const cores = Number((cpu || {}).cores) || 0;
  const threads = Number((cpu || {}).threads) || 0;
  if (cores <= 0 && threads <= 0) return '';
  if (cores <= 0) return threads + ' 线程';
  if (threads <= 0 || threads === cores) return cores + ' 核';
  return cores + ' 核 ' + threads + ' 线程';
}

function fmtUptime(sec) {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d > 0) return d + ' 天 ' + h + ' 小时';
  if (h > 0) return h + ' 小时 ' + m + ' 分';
  return m + ' 分钟';
}

function fmtAgo(sec) {
  sec = Number(sec) || 0;
  if (sec < 60) return sec + ' 秒前';
  if (sec < 3600) return Math.floor(sec / 60) + ' 分钟前';
  if (sec < 86400) return Math.floor(sec / 3600) + ' 小时前';
  return Math.floor(sec / 86400) + ' 天前';
}

function fmtTime(ts, spanSec) {
  const d = new Date(ts * 1000), p = (n) => String(n).padStart(2, '0');
  if (spanSec > 172800) return (d.getMonth() + 1) + '/' + d.getDate() + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  if (spanSec > 86400) return p(d.getHours()) + ':' + p(d.getMinutes());
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

const level = (p) => (p >= 90 ? 'crit' : p >= 75 ? 'warn' : 'ok');

/* ---------------- API ---------------- */

async function api(path) {
  const headers = {};
  if (state.token) headers['Authorization'] = 'Bearer ' + state.token;
  const res = await fetch(path, { headers, cache: 'no-store' });
  if (res.status === 401) {
    openTokenModal('访问令牌缺失或无效，请输入主控的 admin_token。');
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    let msg = 'HTTP ' + res.status;
    try { const j = await res.json(); if (j && j.error) msg = j.error; } catch (e) { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

/* ---------------- 节点卡片 ---------------- */

function barRow(label, valueHTML, pct, extraClass) {
  const p = Math.max(0, Math.min(100, Number(pct) || 0));
  return '<div class="metric' + (extraClass ? ' ' + extraClass : '') + '">' +
    '<div class="metric-top"><span class="k">' + esc(label) + '</span>' +
    '<span class="v">' + valueHTML + '</span></div>' +
    '<div class="bar lv-' + level(p) + '"><i style="width:' + p.toFixed(1) + '%"></i></div>' +
    '</div>';
}

function cardHTML(n) {
  const cpu = n.cpu || {}, mem = n.mem || {}, disk = n.disk || {}, net = n.net || {};
  const gpus = Array.isArray(n.gpu) ? n.gpu : [];
  const host = n.host || {};

  let body = '';
  const topo = cpuTopoText(cpu);
  body += barRow('CPU', esc(fmtPct(cpu.usage_pct)) +
    (topo ? '<span class="sub">' + esc(topo) + '</span>' : ''), cpu.usage_pct);
  body += barRow('内存', esc(fmtPct(mem.used_pct)) +
    (mem.total_bytes ? '<span class="sub">' + esc(fmtBytes(mem.used_bytes)) + ' / ' + esc(fmtBytes(mem.total_bytes)) + '</span>' : ''),
    mem.used_pct);
  body += barRow('磁盘', esc(fmtPct(disk.max_used_pct)) +
    (disk.total_bytes ? '<span class="sub">' + esc(fmtBytes(disk.total_bytes)) + '</span>' : ''), disk.max_used_pct);

  gpus.slice(0, 2).forEach((g) => {
    body += barRow('GPU' + esc(g.index) + ' ' + esc((g.name || '').replace(/NVIDIA\s*/i, '')),
      esc(fmtPct(g.util_pct)) + '<span class="sub">显存 ' + esc(fmtPct(g.mem_used_pct)) + '</span>', g.util_pct);
  });

  body += cardTempRows(n);

  // 任何一类温度越线都在卡片上直接标出来，不用点进详情才发现
  const tmax = (n.temp && n.temp.max_c) || 0;
  if (tmax >= 85) {
    body += '<div class="card-warn">温度告警：' + esc(n.temp.max_name || '传感器') +
      ' 已达 ' + esc(fmtTemp(tmax)) + '</div>';
  }

  const tags = Object.entries(n.labels || {})
    .slice(0, 4)
    .map(([k, v]) => '<span class="tag">' + esc(k) + '=' + esc(v) + '</span>').join('');

  let foot = '';
  if (cpu.load1) foot += '<span>负载 ' + esc(cpu.load1.toFixed(2)) + '</span>';
  foot += '<span>↑ ' + esc(fmtBytes(net.tx_bytes_sec)) + '/s</span>';
  foot += '<span>↓ ' + esc(fmtBytes(net.rx_bytes_sec)) + '/s</span>';
  if (host.uptime_sec) foot += '<span>已运行 ' + esc(fmtUptime(host.uptime_sec)) + '</span>';
  foot += '<span>' + esc(fmtAgo(n.age_sec)) + '</span>';

  let warn = '';
  if (Array.isArray(n.errors) && n.errors.length) {
    warn = '<div class="card-warn">采集降级：' + esc(n.errors[0]) +
      (n.errors.length > 1 ? '（等 ' + n.errors.length + ' 项）' : '') + '</div>';
  }
  if (n.buffered) {
    warn += '<div class="card-warn">链路抖动：本条为补发数据（前面积压 ' + esc(n.buffered) + ' 条）</div>';
  }

  return '<article class="card' + (n.online ? '' : ' offline') + '" data-id="' + esc(n.node_id) + '">' +
    '<div class="card-head"><h3>' + esc(n.node_id) + '</h3>' +
    '<span class="status ' + (n.online ? 'on' : 'off') + '">' + (n.online ? '在线' : '离线') + '</span></div>' +
    '<div class="card-sub">' + esc(host.hostname || '-') + ' · ' +
    esc(host.os || '') + '/' + esc(host.arch || '') +
    (host.container ? ' · ' + esc(host.container) : '') + '</div>' +
    (tags ? '<div class="tags">' + tags + '</div>' : '') +
    body + warn +
    '<div class="card-foot">' + foot + '</div>' +
    '</article>';
}

function chip(label, value, cls) {
  return '<span class="chip' + (cls ? ' ' + cls : '') + '">' + esc(label) + ' <b>' + esc(value) + '</b></span>';
}

/* 温度条：和 barRow 长得一样，但阈值用 tempLevel（70/85），
   而不是使用率的 75/90——温度 75℃ 已经很烫了。 */
function tempRow(label, c, sub) {
  const p = Math.max(0, Math.min(100, Number(c) || 0));
  return '<div class="metric">' +
    '<div class="metric-top"><span class="k">' + esc(label) + '</span>' +
    '<span class="v">' + esc(fmtTemp(c)) +
    (sub ? '<span class="sub">' + esc(sub) + '</span>' : '') + '</span></div>' +
    '<div class="bar lv-' + tempLevel(c) + '"><i style="width:' + p.toFixed(1) + '%"></i></div>' +
    '</div>';
}

/* 卡片上最多展开几个类别，其余折叠成一行提示——
   一台机器可能有十几个温度传感器，全铺开会把卡片撑得没法看。 */
const CARD_TEMP_ROWS = 3;

function cardTempRows(n) {
  const t = n.temp || null;
  if (!t || !(t.max_c > 0)) return '';

  const byKind = t.by_kind || {};
  let kinds = Object.keys(byKind).sort((a, b) => {
    const d = byKind[b] - byKind[a];
    return d !== 0 ? d : TEMP_ORDER.indexOf(a) - TEMP_ORDER.indexOf(b);
  });
  // 兜底：master 没给 by_kind（老版本）时，只用整体最高温渲染一行
  if (!kinds.length) return tempRow('温度', t.max_c, t.max_name || '');

  let html = '';
  kinds.slice(0, CARD_TEMP_ROWS).forEach((k, i) => {
    const meta = tempMeta(k);
    const sub = i === 0 && t.max_name && t.max_kind === k ? t.max_name : '';
    html += tempRow(meta.label + ' 温度', byKind[k], sub);
  });
  if (kinds.length > CARD_TEMP_ROWS) {
    html += '<p class="muted small" style="margin:-4px 0 9px">另有 ' +
      esc(kinds.length - CARD_TEMP_ROWS) + ' 类传感器，点击卡片查看全部</p>';
  }
  return html;
}

/* 详情里的全量传感器表。agent v1.0.x 只上报 cpu.temp_c / gpu[].temp_c，
   这里做一次兼容补齐，避免升级期看板直接显示"无数据"。 */
function tempSensorList(n) {
  const list = Array.isArray(n.temps) ? n.temps.slice() : [];
  if (list.length) return list;
  if (n.cpu && n.cpu.temp_c > 0) {
    list.push({ name: 'CPU', kind: 'cpu', temp_c: n.cpu.temp_c, source: 'agent<1.1' });
  }
  (n.gpu || []).forEach((g) => {
    if (g.temp_c > 0) {
      list.push({ name: g.name || ('GPU' + g.index), kind: 'gpu', temp_c: g.temp_c, source: 'nvidia-smi' });
    }
  });
  return list;
}

function tempTable(n) {
  const list = tempSensorList(n);
  if (!list.length) {
    return '<p class="muted small" style="margin:0">该节点没有可读的温度传感器。' +
      '（Windows 上读磁盘温度需要以管理员身份运行 agent）</p>';
  }
  const sorted = list.slice().sort((a, b) => {
    const ka = TEMP_ORDER.indexOf(a.kind), kb = TEMP_ORDER.indexOf(b.kind);
    if (ka !== kb) return (ka < 0 ? 99 : ka) - (kb < 0 ? 99 : kb);
    return (b.temp_c || 0) - (a.temp_c || 0);
  });
  return '<table><thead><tr><th>传感器</th><th>类别</th><th class="num">当前</th>' +
    '<th class="num">上限</th><th class="num">临界</th><th>来源</th></tr></thead><tbody>' +
    sorted.map((t) => {
      const meta = tempMeta(t.kind);
      return '<tr><td>' + esc(t.name || '-') + '</td>' +
        '<td><span class="tkind" style="color:' + meta.color + '">' + esc(meta.label) + '</span></td>' +
        '<td class="num" style="color:var(--' + tempLevel(t.temp_c) + ')">' + esc(fmtTemp(t.temp_c)) + '</td>' +
        '<td class="num muted">' + (t.max_c > 0 ? esc(fmtTemp(t.max_c)) : '-') + '</td>' +
        '<td class="num muted">' + (t.crit_c > 0 ? esc(fmtTemp(t.crit_c)) : '-') + '</td>' +
        '<td class="muted">' + esc(t.source || '-') + '</td></tr>';
    }).join('') + '</tbody></table>';
}

function renderGrid() {
  const nodes = state.nodes;
  const online = nodes.filter((n) => n.online).length;
  const gpuTotal = nodes.reduce((a, n) => a + (Array.isArray(n.gpu) ? n.gpu.length : 0), 0);
  // 只看在线节点：离线节点的温度是"最后一次上报的历史值"，拿来告警会误导
  const hottest = nodes.reduce((a, n) => Math.max(a, n.online ? ((n.temp && n.temp.max_c) || 0) : 0), 0);

  let summary = chip('节点', nodes.length) + chip('在线', online, 'ok');
  if (nodes.length - online > 0) summary += chip('离线', nodes.length - online, 'bad');
  if (gpuTotal > 0) summary += chip('GPU', gpuTotal);
  if (hottest > 0) {
    const lv = tempLevel(hottest);
    summary += chip('最高温', fmtTemp(hottest), lv === 'ok' ? '' : (lv === 'warn' ? 'warn' : 'bad'));
  }
  $('#summary').innerHTML = summary;
  $('#pulse').className = 'pulse' + (nodes.length > 0 && online < nodes.length ? ' bad' : '');

  const grid = $('#grid');
  if (!nodes.length) {
    grid.innerHTML = '';
    $('#empty').classList.remove('hidden');
    return;
  }
  $('#empty').classList.add('hidden');
  grid.innerHTML = nodes.map(cardHTML).join('');
  Array.from(grid.querySelectorAll('.card')).forEach((el) => {
    el.addEventListener('click', () => openDrawer(el.dataset.id));
  });
}

/* ---------------- 折线图 ---------------- */

function renderChart(container, series, opts) {
  opts = opts || {};
  const W = 720, H = opts.height || 132;
  const padL = 48, padR = 12, padT = 10, padB = 22;
  const iw = W - padL - padR, ih = H - padT - padB;

  let all = [];
  series.forEach((s) => { all = all.concat(s.points || []); });
  if (!all.length) {
    container.innerHTML = '<p class="muted small" style="margin:6px 0">该时间范围内没有数据</p>';
    return;
  }

  const ts0 = Math.min.apply(null, all.map((p) => p.ts));
  const ts1 = Math.max.apply(null, all.map((p) => p.ts));
  let vmax = Number(opts.max) || 0;
  if (!vmax) {
    vmax = Math.max.apply(null, all.map((p) => p.v)) * 1.15;
    if (!(vmax > 0)) vmax = 1;
  }
  const vmin = 0;
  const sx = (ts) => padL + (ts1 === ts0 ? iw / 2 : (ts - ts0) / (ts1 - ts0) * iw);
  const sy = (v) => padT + ih - (Math.max(vmin, Math.min(vmax, v)) - vmin) / (vmax - vmin) * ih;

  let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img">';

  // 水平网格 + y 轴刻度
  const lines = 4;
  for (let i = 0; i <= lines; i++) {
    const v = vmin + (vmax - vmin) * i / lines;
    const y = sy(v);
    svg += '<line x1="' + padL + '" y1="' + y.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + y.toFixed(1) +
      '" stroke="currentColor" stroke-opacity="0.10" stroke-width="1"/>';
    svg += '<text x="' + (padL - 7) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end" ' +
      'font-size="10" fill="currentColor" fill-opacity="0.5">' + esc(v >= 100 ? v.toFixed(0) : v.toFixed(1)) + '</text>';
  }

  const span = ts1 - ts0;
  for (let i = 0; i <= 4; i++) {
    const ts = ts0 + span * i / 4;
    svg += '<text x="' + sx(ts).toFixed(1) + '" y="' + (H - 6) + '" text-anchor="' +
      (i === 0 ? 'start' : i === 4 ? 'end' : 'middle') + '" font-size="10" fill="currentColor" fill-opacity="0.5">' +
      esc(fmtTime(Math.round(ts), span)) + '</text>';
  }

  series.forEach((s) => {
    const pts = (s.points || []).filter((p) => typeof p.v === 'number');
    if (!pts.length) return;
    let d = '';
    pts.forEach((p, i) => { d += (i ? 'L' : 'M') + sx(p.ts).toFixed(1) + ' ' + sy(p.v).toFixed(1) + ' '; });

    if (series.length === 1 && opts.area !== false) {
      svg += '<defs><linearGradient id="g_' + esc(opts.gid || 'x') + '" x1="0" y1="0" x2="0" y2="1">' +
        '<stop offset="0%" stop-color="' + s.color + '" stop-opacity="0.28"/>' +
        '<stop offset="100%" stop-color="' + s.color + '" stop-opacity="0.02"/></linearGradient></defs>';
      svg += '<path d="' + d + 'L' + sx(pts[pts.length - 1].ts).toFixed(1) + ' ' + (padT + ih) +
        ' L' + sx(pts[0].ts).toFixed(1) + ' ' + (padT + ih) + ' Z" fill="url(#g_' + esc(opts.gid || 'x') + ')"/>';
    }
    svg += '<path d="' + d + '" fill="none" stroke="' + s.color + '" stroke-width="1.6" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>';
  });

  svg += '<line class="hv" x1="0" y1="' + padT + '" x2="0" y2="' + (padT + ih) +
    '" stroke="currentColor" stroke-opacity="0.35" stroke-width="1" stroke-dasharray="3 3" style="display:none"/>';
  svg += '</svg>';

  container.innerHTML = '<div class="chart-wrap">' + svg + '<div class="tip hidden"></div></div>';

  const wrap = container.querySelector('.chart-wrap');
  const hv = container.querySelector('.hv');
  const tip = container.querySelector('.tip');
  const svgEl = container.querySelector('svg');

  wrap.addEventListener('mousemove', (ev) => {
    const rect = svgEl.getBoundingClientRect();
    const vx = (ev.clientX - rect.left) * (W / rect.width);
    if (vx < padL || vx > W - padR) { hv.style.display = 'none'; tip.classList.add('hidden'); return; }
    const ts = ts0 + (vx - padL) / iw * span;

    let html = '<b>' + esc(fmtTime(Math.round(ts), span)) + '</b>';
    let nearest = null;
    series.forEach((s) => {
      const pts = s.points || [];
      if (!pts.length) return;
      let best = pts[0], bestD = Math.abs(pts[0].ts - ts);
      for (let i = 1; i < pts.length; i++) {
        const dd = Math.abs(pts[i].ts - ts);
        if (dd < bestD) { bestD = dd; best = pts[i]; }
      }
      if (!nearest || bestD < nearest.d) nearest = { d: bestD, ts: best.ts, x: sx(best.ts) };
      html += '<br><span style="color:' + s.color + '">■</span> ' + esc(s.name) + ' ' +
        esc(s.val(best.v)) + esc(opts.unit || '');
    });

    if (nearest) {
      hv.setAttribute('x1', nearest.x.toFixed(1));
      hv.setAttribute('x2', nearest.x.toFixed(1));
      hv.style.display = '';
    }
    tip.innerHTML = html;
    tip.classList.remove('hidden');
    const wrapRect = wrap.getBoundingClientRect();
    let left = ((nearest ? nearest.x : padL) / W) * wrapRect.width + 10;
    if (left > wrapRect.width - 150) left = wrapRect.width - 150;
    tip.style.left = Math.max(0, left) + 'px';
    tip.style.top = '4px';
  });

  wrap.addEventListener('mouseleave', () => {
    hv.style.display = 'none';
    tip.classList.add('hidden');
  });
}

/* ---------------- 详情抽屉 ---------------- */

function drawerHeaderHTML(n) {
  const host = n.host || {};
  const parts = [];
  if (host.hostname) parts.push(host.hostname);
  parts.push((host.os || '') + '/' + (host.arch || ''));
  if (host.kernel) parts.push('内核 ' + host.kernel);
  if (host.container) parts.push(host.container);
  if (host.uptime_sec) parts.push('已运行 ' + fmtUptime(host.uptime_sec));
  parts.push('agent ' + (n.agent_version || '-'));
  parts.push('最后上报 ' + fmtAgo(n.age_sec));
  return esc(parts.join(' · '));
}

function infoTable(n) {
  const rows = [];
  const add = (k, v) => { if (v !== undefined && v !== null && v !== '') rows.push([k, v]); };
  const cpu = n.cpu || {}, mem = n.mem || {}, host = n.host || {};
  add('主机名', host.hostname);
  add('系统', (host.os || '') + ' ' + (host.kernel || ''));
  add('运行环境', host.container || '裸机');
  // 核心与线程分两行显示，避免读者把线程数当成核心数。
  add('CPU 物理核心', cpu.cores ? cpu.cores + ' 核' : '');
  add('CPU 逻辑线程', cpu.threads ? cpu.threads + ' 线程' : '');
  // 只有多路（≥2）才值得占一行：单路是绝大多数情况，多路才是需要一眼看出的信息。
  add('CPU 路数', cpu.sockets >= 2 ? cpu.sockets + ' 路' : '');
  add('CPU 主频', cpu.freq_mhz ? cpu.freq_mhz + ' MHz' : '');
  add('CPU 温度', cpu.temp_c ? cpu.temp_c + ' °C' : '');
  add('最高温度', n.temp && n.temp.max_c
    ? fmtTemp(n.temp.max_c) + (n.temp.max_name ? '（' + n.temp.max_name + '）' : '')
    : '');
  add('负载 (1/5/15)', cpu.load1 ? cpu.load1.toFixed(2) + ' / ' + cpu.load5.toFixed(2) + ' / ' + cpu.load15.toFixed(2) : '');
  add('内存', mem.total_bytes ? fmtBytes(mem.used_bytes) + ' / ' + fmtBytes(mem.total_bytes) : '');
  add('Swap', mem.swap_total_bytes ? fmtBytes(mem.swap_used_bytes) + ' / ' + fmtBytes(mem.swap_total_bytes) : '');
  add('采样耗时', n.collect_ms + ' ms');
  add('采样序号', n.seq);
  return '<table><tbody>' + rows.map((r) =>
    '<tr><th style="width:38%">' + esc(r[0]) + '</th><td>' + esc(r[1]) + '</td></tr>').join('') + '</tbody></table>';
}

function diskTable(n) {
  const mounts = (n.disk && n.disk.mounts) || [];
  if (!mounts.length) return '<p class="muted small">无磁盘数据</p>';
  return '<table><thead><tr><th>挂载点</th><th>文件系统</th><th class="num">已用</th>' +
    '<th class="num">可用</th><th class="num">使用率</th><th class="num">inode</th></tr></thead><tbody>' +
    mounts.map((m) => '<tr><td>' + esc(m.mount) + '</td><td class="muted">' + esc(m.fs || '-') + '</td>' +
      '<td class="num">' + esc(fmtBytes(m.used_bytes)) + '</td>' +
      '<td class="num">' + esc(fmtBytes(m.avail_bytes)) + '</td>' +
      '<td class="num" style="color:var(--' +
      (level(m.used_pct) === 'ok' ? 'ok' : level(m.used_pct)) + ')">' + esc(fmtPct(m.used_pct)) + '</td>' +
      '<td class="num muted">' + (m.inode_pct ? esc(fmtPct(m.inode_pct)) : '-') + '</td></tr>').join('') +
    '</tbody></table>';
}

function gpuTable(n) {
  const gpus = n.gpu || [];
  if (!gpus.length) return '';
  return '<div class="panel"><h4>GPU</h4><table><thead><tr><th>#</th><th>型号</th>' +
    '<th class="num">利用率</th><th class="num">显存</th><th class="num">温度</th><th class="num">功耗</th></tr></thead><tbody>' +
    gpus.map((g) => '<tr><td>' + esc(g.index) + '</td><td>' + esc(g.name) + '</td>' +
      '<td class="num">' + esc(fmtPct(g.util_pct)) + '</td>' +
      '<td class="num">' + esc(fmtBytes(g.mem_used_bytes)) + ' / ' + esc(fmtBytes(g.mem_total_bytes)) + '</td>' +
      '<td class="num">' + (g.temp_c ? esc(g.temp_c.toFixed(0)) + ' °C' : '-') + '</td>' +
      '<td class="num">' + (g.power_w ? esc(g.power_w.toFixed(0)) + ' W' : '-') + '</td></tr>').join('') +
    '</tbody></table></div>';
}

/* 该节点实际有数据的温度类别，按 CPU→GPU→磁盘→… 的固定顺序排列。
   未知类别排在最后，保证新传感器不会让已有顺序跳来跳去。 */
function tempKindsOf(n) {
  if (!n) return [];
  const set = new Set();
  tempSensorList(n).forEach((t) => { if (t.kind) set.add(t.kind); });
  const known = TEMP_ORDER.filter((k) => set.has(k));
  const unknown = Array.from(set).filter((k) => TEMP_ORDER.indexOf(k) < 0).sort();
  return known.concat(unknown);
}

/* 抽屉里的图表面板定义。抽成独立函数是为了让轮询逻辑能对比
   "当前应该有哪几张图" 与 "已经画了哪几张图"。 */
function drawerPanels(n) {
  const panels = [
    { key: 'cpu', title: 'CPU 使用率', unit: '%', max: 100, gid: 'cpu', color: COLORS.cpu, val: (v) => v.toFixed(1) },
    { key: 'mem', title: '内存使用率', unit: '%', max: 100, gid: 'mem', color: COLORS.mem, val: (v) => v.toFixed(1) },
    { key: 'disk', title: '磁盘使用率（最高挂载点）', unit: '%', max: 100, gid: 'disk', color: COLORS.disk, val: (v) => v.toFixed(1) },
  ];

  if (n && n.gpu && n.gpu.length) {
    panels.push({ key: 'gpu', title: 'GPU 利用率', unit: '%', max: 100, gid: 'gpu', color: COLORS.gpu, val: (v) => v.toFixed(1) });
  }

  // 温度：先给一条"全部传感器的最高温"，再按类别各给一条。
  // 类别面板是排查用的（哪块盘、哪张卡热），全局面板是看趋势用的。
  const kinds = tempKindsOf(n);
  if (kinds.length) {
    panels.push({
      key: 'temp', id: 'temp_all', title: '最高温度（全部传感器）',
      unit: '°C', gid: 'temp', color: COLORS.temp, val: (v) => v.toFixed(1),
    });
    kinds.forEach((k) => {
      const meta = tempMeta(k);
      panels.push({
        key: 'temp:' + k, id: 'temp_' + k, title: meta.label + ' 温度',
        unit: '°C', gid: 'temp_' + k, color: meta.color, val: (v) => v.toFixed(1),
      });
    });
  }

  panels.push({ key: '__net', title: '网络吞吐', unit: ' KiB/s', gid: 'net', multi: true, val: (v) => v.toFixed(1) });
  return panels;
}

async function loadCharts() {
  const id = state.selected;
  if (!id) return;
  const n = state.nodes.find((x) => x.node_id === id);
  const panels = drawerPanels(n);
  state.panelKeys = panels.map((p) => p.key);

  const sensors = tempSensorList(n || {});

  const body = $('#drawerBody');
  body.innerHTML =
    '<div class="panel"><h4>节点信息</h4>' + infoTable(n || {}) + '</div>' +
    gpuTable(n || {}) +
    '<div class="panel"><h4>温度传感器' +
      (sensors.length ? '<span class="legend">共 ' + esc(sensors.length) + ' 个</span>' : '') +
      '</h4>' + tempTable(n || {}) + '</div>' +
    panels.map((p) => '<div class="panel" id="panel_' + esc(p.id || p.key) + '"><h4>' + esc(p.title) +
      '<span class="legend">' + (p.multi
        ? '<span><i style="background:' + COLORS.rx + '"></i>接收</span><span><i style="background:' + COLORS.tx + '"></i>发送</span>'
        : '<span>' + esc(p.unit || '') + '</span>') + '</span></h4>' +
      '<div class="chart-slot muted small">加载中…</div></div>').join('') +
    '<div class="panel"><h4>磁盘挂载点</h4>' + diskTable(n || {}) + '</div>' +
    (n && n.errors && n.errors.length
      ? '<div class="panel"><h4>采集降级</h4><p class="small muted" style="margin:0">' +
        n.errors.map(esc).join('<br>') + '</p></div>'
      : '');

  for (const p of panels) {
    const slot = $('#panel_' + (p.id || p.key) + ' .chart-slot');
    if (!slot) continue;
    const metric = encodeURIComponent(p.key);
    try {
      if (p.multi) {
        const [rx, tx] = await Promise.all([
          api('/api/v1/history?node=' + encodeURIComponent(id) + '&metric=net_rx&minutes=' + state.minutes),
          api('/api/v1/history?node=' + encodeURIComponent(id) + '&metric=net_tx&minutes=' + state.minutes),
        ]);
        renderChart(slot, [
          { name: '接收', color: COLORS.rx, points: rx.points || [] },
          { name: '发送', color: COLORS.tx, points: tx.points || [] },
        ], { unit: ' KiB/s', gid: 'net', val: p.val });
      } else {
        const h = await api('/api/v1/history?node=' + encodeURIComponent(id) +
          '&metric=' + metric + '&minutes=' + state.minutes);
        renderChart(slot, [{ name: p.title, color: p.color, points: h.points || [] }],
          { unit: h.unit || p.unit, max: p.max, gid: p.gid, val: p.val });
      }
      slot.classList.remove('muted', 'small');
    } catch (e) {
      slot.innerHTML = '<p class="muted small">加载失败：' + esc(e.message) + '</p>';
    }
  }
}

function openDrawer(id) {
  state.selected = id;
  const n = state.nodes.find((x) => x.node_id === id);
  $('#drawerTitle').textContent = id;
  $('#drawerSub').innerHTML = drawerHeaderHTML(n || {});
  $('#drawer').classList.remove('hidden');
  $('#drawer').setAttribute('aria-hidden', 'false');
  $('#scrim').classList.remove('hidden');
  loadCharts();
}

function closeDrawer() {
  state.selected = null;
  $('#drawer').classList.add('hidden');
  $('#drawer').setAttribute('aria-hidden', 'true');
  $('#scrim').classList.add('hidden');
}

/* ---------------- 令牌弹窗 ---------------- */

function openTokenModal(hint) {
  if (state.tokenPrompted && !hint) return;
  state.tokenPrompted = true;
  $('#tokenInput').value = state.token;
  $('#tokenHint').textContent = hint || '';
  $('#tokenModal').classList.remove('hidden');
  $('#tokenInput').focus();
}

function closeTokenModal() { $('#tokenModal').classList.add('hidden'); }

/* ---------------- 轮询 ---------------- */

let chartTick = 0;

async function refreshNodes() {
  try {
    const data = await api('/api/v1/nodes');
    state.nodes = Array.isArray(data.nodes) ? data.nodes : [];
    if (data.title) { $('#title').textContent = data.title; document.title = data.title; }
    $('#error').classList.add('hidden');
    renderGrid();
    if (state.selected) {
      const n = state.nodes.find((x) => x.node_id === state.selected);
      if (n) {
        $('#drawerSub').innerHTML = drawerHeaderHTML(n);
        // 节点可能刚刚才上报出新维度（第一次读到 GPU / 温度传感器），
        // 这时抽屉里还没有对应的面板，需要重新渲染一次。
        const keys = drawerPanels(n).map((p) => p.key).join(',');
        if (keys !== state.panelKeys.join(',')) loadCharts();
      }
    }
  } catch (e) {
    if (e.message === 'unauthorized') return;
    $('#error').textContent = '获取数据失败：' + e.message;
    $('#error').classList.remove('hidden');
  }
}

function boot() {
  applyTheme();
  $('#themeBtn').addEventListener('click', cycleTheme);
  $('#tokenBtn').addEventListener('click', () => openTokenModal(''));
  $('#tokenSave').addEventListener('click', () => {
    state.token = $('#tokenInput').value.trim();
    localStorage.setItem('mon_token', state.token);
    closeTokenModal();
    refreshNodes();
    if (state.selected) loadCharts();
  });
  $('#tokenClear').addEventListener('click', () => {
    state.token = '';
    localStorage.removeItem('mon_token');
    closeTokenModal();
    refreshNodes();
  });
  $('#tokenCancel').addEventListener('click', closeTokenModal);

  $('#range').addEventListener('change', (ev) => {
    state.minutes = Number(ev.target.value) || 60;
    if (state.selected) loadCharts();
  });

  $('#drawerClose').addEventListener('click', closeDrawer);
  $('#scrim').addEventListener('click', closeDrawer);
  document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') closeDrawer(); });

  refreshNodes();
  setInterval(refreshNodes, 5000);
  setInterval(() => {
    if (!state.selected) return;
    chartTick++;
    if (chartTick % 3 === 0) loadCharts(); // 图表 15 秒刷新一次，避免频繁重绘
  }, 5000);
}

document.addEventListener('DOMContentLoaded', boot);
