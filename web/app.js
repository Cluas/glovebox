'use strict';
/* Glovebox web console — no build step, no dependencies. */

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

/* ---------- preferences ---------- */

const store = {
  get(k, d) { try { return localStorage.getItem('glovebox.' + k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem('glovebox.' + k, v); } catch { /* private mode */ } },
};

/* ---------- i18n ---------- */

// Strings live in /i18n/<locale>.json with English as the fallback. /i18n/index.json maps locale
// codes to native names, so adding a language is just adding a file and an index entry.
const FALLBACK_LANG = 'en';
let locales = { en: 'English' };
let lang = FALLBACK_LANG;
let dict = {};
let fallbackDict = {};

async function fetchJSON(url, timeout = 8000) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeout);
  try {
    const r = await fetch(url, { credentials: 'same-origin', signal: ctl.signal });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    return await r.json();
  } finally {
    clearTimeout(timer);
  }
}

// pickLocale prefers the saved choice, then the browser languages; exact match first, then same base language
function pickLocale(saved) {
  const codes = Object.keys(locales);
  const wanted = [saved, ...(navigator.languages || [navigator.language])].filter(Boolean);
  for (const w of wanted) {
    const exact = codes.find((c) => c.toLowerCase() === w.toLowerCase());
    if (exact) return exact;
    const base = w.split('-')[0].toLowerCase();
    const near = codes.find((c) => c.split('-')[0].toLowerCase() === base);
    if (near) return near;
  }
  return FALLBACK_LANG;
}

async function setLang(code) {
  lang = code in locales ? code : FALLBACK_LANG;
  try {
    dict = lang === FALLBACK_LANG ? fallbackDict : await fetchJSON(`/i18n/${lang}.json`);
  } catch {
    lang = FALLBACK_LANG;
    dict = fallbackDict;
  }
  applyI18n();
}

async function initI18n() {
  try { locales = await fetchJSON('/i18n/index.json'); } catch { /* English only */ }
  try { fallbackDict = await fetchJSON(`/i18n/${FALLBACK_LANG}.json`); } catch { /* keys render as is */ }
  await setLang(pickLocale(store.get('lang', '')));
}

const has = (key) => key in dict || key in fallbackDict;
function t(key, vars) {
  let s = dict[key] ?? fallbackDict[key] ?? key;
  if (vars) s = s.replace(/\{(\w+)\}/g, (_, k) => vars[k] ?? '');
  return s;
}
// tEnum translates a backend code such as sim.NO_SIM; unknown values are shown as is
const tEnum = (ns, v) => (v && has(`${ns}.${v}`) ? t(`${ns}.${v}`) : v);

function applyI18n() {
  document.documentElement.lang = lang;
  $$('[data-i18n]').forEach((el) => { el.textContent = t(el.dataset.i18n); });
  $$('[data-i18n-ph]').forEach((el) => { el.placeholder = t(el.dataset.i18nPh); });
  $('#lang-seg').innerHTML = Object.entries(locales).map(([code, name]) =>
    `<button type="button" data-lang-set="${esc(code)}" lang="${esc(code)}"${code === lang ? ' class="on"' : ''}>${esc(name)}</button>`).join('');
}

/* ---------- formatting ---------- */

function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2)) + ' ' + u[i];
}
// rates are shown in Mbps, like speed-test apps
function fmtRate(bps) {
  const mbps = (Number(bps) || 0) * 8 / 1e6;
  if (mbps < 1) return [(mbps * 1000).toFixed(0), 'Kbps'];
  return [mbps.toFixed(mbps >= 100 ? 0 : 1), 'Mbps'];
}
const rateText = (bps) => fmtRate(bps).join(' ');
function fmtDur(sec) {
  sec = Math.max(0, Math.round(Number(sec) || 0));
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m`;
  return `${sec}s`;
}
function relTime(iso) {
  const ts = Date.parse(iso);
  if (!ts || iso.startsWith('0001')) return '—';
  const s = (Date.now() - ts) / 1000;
  if (s < 60) return t('rel.now');
  if (s < 3600) return t('rel.m', { n: Math.floor(s / 60) });
  if (s < 86400) return t('rel.h', { n: Math.floor(s / 3600) });
  return t('rel.d', { n: Math.floor(s / 86400) });
}
const numOrNull = (v) => (v === '' || v == null || isNaN(Number(v)) ? null : Number(v));
function delayCls(d) {
  if (d == null || d < 0) return 'd-none';
  if (d === 0) return 'd-dead';
  if (d < 200) return 'd-fast';
  if (d < 500) return 'd-mid';
  return 'd-slow';
}
const delayText = (d) => (d > 0 ? d + ' ms' : d === 0 ? t('node.timeout') : '—');
// RSRP / SINR quality: 0 poor … 4 excellent
const rsrpQ = (v) => (v == null ? null : v >= -80 ? 4 : v >= -90 ? 3 : v >= -100 ? 2 : v >= -110 ? 1 : 0);
const sinrQ = (v) => (v == null ? null : v >= 20 ? 4 : v >= 13 ? 3 : v >= 5 ? 2 : v >= 0 ? 1 : 0);
const Q_COLOR = ['var(--bad)', '#ff8f5c', 'var(--warn)', 'var(--ok)', 'var(--ok)'];

/* ---------- API ---------- */

class AuthError extends Error {}
class NetError extends Error {}
class ApiError extends Error {
  constructor(message, code) { super(message); this.code = code; }
}

let linkFails = 0;
function setLink(ok) {
  linkFails = ok ? 0 : linkFails + 1;
  const down = linkFails >= 2;
  $('#linkdown').hidden = !down;
  document.body.classList.toggle('stale', down);
}

async function api(path, body, { timeout = 20000 } = {}) {
  const init = { headers: { 'X-Glovebox': '1' }, cache: 'no-store', credentials: 'same-origin' };
  if (body !== undefined) {
    init.method = 'POST';
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeout);
  init.signal = ctl.signal;
  let r;
  try {
    r = await fetch(path, init);
  } catch {
    setLink(false);
    throw new NetError(t('link.down'));
  } finally {
    clearTimeout(timer);
  }
  setLink(true);
  let data = null;
  try { data = await r.json(); } catch { /* not JSON */ }
  if (r.status === 401 && path !== '/api/login') {
    showLogin();
    throw new AuthError('401');
  }
  if (!r.ok) {
    // translate known error codes; otherwise show the server's English message
    const code = data && data.code;
    const msg = code && has('err.' + code) ? t('err.' + code, data.args) : (data && data.error) || `HTTP ${r.status}`;
    throw new ApiError(msg, code);
  }
  return data;
}

/* ---------- toasts and dialogs ---------- */

function toast(msg, bad) {
  const el = document.createElement('div');
  el.className = 'toast' + (bad ? ' bad' : '');
  el.innerHTML = `<span class="tk">${bad ? '[ERR]' : '[ OK]'}</span><span></span>`;
  el.lastChild.textContent = msg;
  const box = $('#toasts');
  box.appendChild(el);
  while (box.children.length > 4) box.firstChild.remove();
  setTimeout(() => el.remove(), bad ? 6000 : 2600);
}
function fail(e) { if (!(e instanceof AuthError) && !(e instanceof NetError)) toast(e.message || String(e), true); }

const modal = $('#modal');
// openModal resolves to true when confirmed; onOpen can wire up events inside the dialog body
function openModal({ title, html, ok = t('act.ok'), cancel = t('act.cancel'), wide = false, danger = false, onOpen, onClose }) {
  return new Promise((resolve) => {
    $('#modal-title').textContent = title;
    $('#modal-body').innerHTML = html;
    const okBtn = $('#modal-ok');
    okBtn.textContent = ok;
    okBtn.hidden = !ok;
    okBtn.className = 'btn ' + (danger ? 'danger' : 'pri');
    $('#modal-cancel').textContent = cancel;
    modal.classList.toggle('wide', wide);
    modal.returnValue = '';
    modal.onclose = () => { onClose && onClose(); resolve(modal.returnValue === 'ok'); };
    if (typeof modal.showModal === 'function') modal.showModal();
    else { modal.setAttribute('open', ''); }
    onOpen && onOpen($('#modal-body'));
  });
}
const confirmBox = (html, opt = {}) => openModal({ title: t('confirm.title'), html, ...opt });

async function busy(btn, label, fn) {
  const old = btn.innerHTML;
  btn.disabled = true;
  if (label) btn.textContent = label + '…';
  try { return await fn(); } finally { btn.disabled = false; btn.innerHTML = old; }
}

/* ---------- data binding ---------- */

const K = {};
$$('[data-k]').forEach((el) => { K[el.dataset.k] = el; });
function put(k, v) {
  const el = K[k];
  const s = String(v ?? '—');
  if (el && el.textContent !== s) el.textContent = s;
}

/* ---------- charts ---------- */

// spark draws values (oldest first) as an SVG polyline; null breaks the line
function spark(svg, values, { min, max, n } = {}) {
  if (!svg) return;
  const vb = svg.viewBox.baseVal;
  const w = vb.width, h = vb.height;
  const line = svg.querySelector('.line'), area = svg.querySelector('.area');
  const nums = values.filter((v) => v != null);
  if (nums.length < 2) { line.setAttribute('d', ''); area.setAttribute('d', ''); return; }
  let lo = min ?? Math.min(...nums), hi = max ?? Math.max(...nums);
  if (hi - lo < 1e-9) { hi += 1; lo -= 1; }
  const count = Math.max(n || values.length, 2);
  const off = count - values.length;
  const x = (i) => ((i + off) / (count - 1)) * w;
  const y = (v) => h - 2 - ((v - lo) / (hi - lo)) * (h - 4);
  let d = '', a = '', segStart = -1;
  const closeSeg = (end) => { if (segStart >= 0) a += `L${x(end).toFixed(1)},${h}L${x(segStart).toFixed(1)},${h}Z`; segStart = -1; };
  values.forEach((v, i) => {
    if (v == null) { closeSeg(i - 1); return; }
    const p = `${x(i).toFixed(1)},${y(v).toFixed(1)}`;
    if (segStart < 0) { d += `M${p}`; a += `M${p}`; segStart = i; } else { d += `L${p}`; a += `L${p}`; }
  });
  closeSeg(values.length - 1);
  line.setAttribute('d', d);
  area.setAttribute('d', a);
}

function niceCeil(v) {
  if (v <= 0) return 1;
  const e = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / e;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10) * e;
}

// throughput chart: DPR-aware canvas, automatic Y axis, hover readout
class Chart {
  constructor(canvas, tip, window = 180) {
    this.c = canvas;
    this.tip = tip;
    this.n = window;
    this.series = [];
    this.hover = -1;
    new ResizeObserver(() => this.resize()).observe(canvas);
    canvas.addEventListener('pointermove', (e) => this.onMove(e));
    canvas.addEventListener('pointerleave', () => { this.hover = -1; this.tip.hidden = true; this.draw(); });
  }
  resize() {
    const r = this.c.getBoundingClientRect();
    if (!r.width) return;
    const dpr = window.devicePixelRatio || 1;
    this.w = r.width;
    this.h = r.height;
    this.c.width = Math.round(r.width * dpr);
    this.c.height = Math.round(r.height * dpr);
    this.ctx = this.c.getContext('2d');
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.draw();
  }
  set(series) { this.series = series; this.draw(); }
  geom() {
    const padL = 46, padR = 6, padT = 10, padB = 20;
    return { padL, padT, W: this.w - padL - padR, H: this.h - padT - padB };
  }
  onMove(e) {
    if (!this.w) return;
    const { padL, W } = this.geom();
    const r = this.c.getBoundingClientRect();
    const i = Math.round(((e.clientX - r.left - padL) / W) * (this.n - 1));
    this.hover = clamp(i, 0, this.n - 1);
    this.draw();
  }
  draw() {
    const { ctx, w, h } = this;
    if (!ctx || !w) return;
    const css = getComputedStyle(document.documentElement);
    const v = (k) => css.getPropertyValue(k).trim();
    const { padL, padT, W, H } = this.geom();
    ctx.clearRect(0, 0, w, h);
    const all = this.series.flatMap((s) => s.values);
    const max = niceCeil(Math.max(1, ...all) * 1.08);
    const X = (i, len) => padL + ((i + this.n - len) / (this.n - 1)) * W;
    const Y = (val) => padT + H - (val / max) * H;

    ctx.font = '10px "JetBrains Mono", ui-monospace, monospace';
    ctx.textBaseline = 'middle';
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const y = Math.round(padT + (H * i) / 4) + 0.5;
      ctx.strokeStyle = v('--line');
      ctx.setLineDash(i === 4 ? [] : [2, 4]);
      ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(padL + W, y); ctx.stroke();
      ctx.fillStyle = v('--fg-2');
      ctx.textAlign = 'right';
      const val = max * (1 - i / 4);
      ctx.fillText(val >= 10 ? val.toFixed(0) : val.toFixed(1), padL - 8, y);
    }
    ctx.setLineDash([]);
    ctx.textAlign = 'left';
    ctx.fillStyle = v('--fg-3');
    ctx.fillText('Mbps', padL + 4, padT + 6);
    ctx.textBaseline = 'alphabetic';
    const marks = [[0, '-3m'], [60, '-2m'], [120, '-1m'], [this.n - 1, 'now']];
    marks.forEach(([i, label]) => {
      ctx.textAlign = i === 0 ? 'left' : i === this.n - 1 ? 'right' : 'center';
      ctx.fillStyle = v('--fg-3');
      ctx.fillText(label, padL + (i / (this.n - 1)) * W, h - 4);
    });

    for (const s of this.series) {
      const vals = s.values;
      if (vals.length < 2) continue;
      const color = v(s.color), rgb = v(s.color + '-rgb');
      ctx.beginPath();
      vals.forEach((val, i) => { const x = X(i, vals.length), y = Y(val); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      if (s.fill) {
        ctx.save();
        ctx.lineTo(X(vals.length - 1, vals.length), padT + H);
        ctx.lineTo(X(0, vals.length), padT + H);
        ctx.closePath();
        const g = ctx.createLinearGradient(0, padT, 0, padT + H);
        g.addColorStop(0, `rgba(${rgb}, .32)`);
        g.addColorStop(1, `rgba(${rgb}, 0)`);
        ctx.fillStyle = g;
        ctx.fill();
        ctx.restore();
        ctx.beginPath();
        vals.forEach((val, i) => { const x = X(i, vals.length), y = Y(val); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      }
      ctx.save();
      ctx.strokeStyle = color;
      ctx.lineWidth = 1.6;
      ctx.lineJoin = 'round';
      ctx.shadowColor = `rgba(${rgb}, .8)`;
      ctx.shadowBlur = 8;
      ctx.stroke();
      ctx.restore();
      // highlight the latest value
      const lx = X(vals.length - 1, vals.length), ly = Y(vals[vals.length - 1]);
      ctx.fillStyle = color;
      ctx.beginPath(); ctx.arc(lx, ly, 2.5, 0, Math.PI * 2); ctx.fill();
    }

    if (this.hover >= 0) {
      const x = padL + (this.hover / (this.n - 1)) * W;
      ctx.strokeStyle = v('--fg-3');
      ctx.setLineDash([3, 3]);
      ctx.beginPath(); ctx.moveTo(Math.round(x) + 0.5, padT); ctx.lineTo(Math.round(x) + 0.5, padT + H); ctx.stroke();
      ctx.setLineDash([]);
      const ago = this.n - 1 - this.hover;
      const rows = this.series.map((s) => {
        const idx = this.hover - (this.n - s.values.length);
        const val = idx >= 0 ? s.values[idx] : null;
        return `<b class="${s.key}">${s.label}</b> ${val == null ? '—' : val.toFixed(val >= 100 ? 0 : 1) + ' Mbps'}`;
      });
      this.tip.innerHTML = `<span class="dim">-${ago}s</span><br>${rows.join('<br>')}`;
      this.tip.hidden = false;
      const tw = this.tip.offsetWidth;
      this.tip.style.left = clamp(x + 12, 0, w - tw - 4) + 'px';
    }
  }
}
const chart = new Chart($('#chart'), $('#chart-tip'));

function gauge(k, pct, text, color) {
  const fig = K[k];
  if (!fig) return;
  const C = 207.35;
  fig.querySelector('.fg').style.strokeDasharray = `${(C * clamp(pct, 0, 1)).toFixed(1)} ${C}`;
  fig.style.setProperty('--gc', color);
  fig.querySelector('b').textContent = text;
}

/* ---------- state ---------- */

const S = { status: null, info: null, proxies: null };
let page = 'overview';
let loggedIn = false;
const PAGES = ['overview', 'proxy', 'device', 'logs'];

function renderChips(s) {
  const c = s.cell || {}, p = s.proxy || {}, ts = s.tailscale || {};
  const chip = (k, state, text) => {
    const el = K[k];
    el.querySelector('.dot').className = 'dot ' + state;
    el.querySelector('b').textContent = text;
  };
  const cellText = c.connected ? tEnum('net', c.label) || 'ONLINE'
    : c.sim && c.sim !== 'READY' ? tEnum('sim', c.sim) : tEnum('net', c.label) || 'OFFLINE';
  chip('chip.cell', c.connected ? 'ok' : 'bad', cellText);
  chip('chip.proxy', p.running ? 'ok' : 'bad', p.running ? String(p.mode || '').toUpperCase() : 'DOWN');
  K['chip.ts'].hidden = !ts.enabled;
  if (ts.enabled) chip('chip.ts', ts.online ? 'ok' : ts.checked ? 'bad' : '', ts.online ? ts.delay + 'ms' : 'OFF');
  put('px.conns', p.running ? p.connections ?? 0 : '—');
}

function renderStatusLine(s) {
  const c = s.cell || {}, p = s.proxy || {}, ts = s.tailscale || {}, r = s.rates || {}, b = s.battery || {};
  const seg = [
    `${c.connected ? c.label : 'NO WAN'}${c.rsrp ? ' ' + c.rsrp + 'dBm' : ''}`,
    p.running ? `MIHOMO ${String(p.mode || '').toUpperCase()}` : 'MIHOMO DOWN',
    ts.enabled ? `TS ${ts.online ? ts.delay + 'ms' : 'OFF'}` : null,
    `↓${fmtRate(r.wan_down)[0]} ↑${fmtRate(r.wan_up)[0]} ${fmtRate(Math.max(r.wan_down || 0, r.wan_up || 0))[1]}`,
    b.present ? `BAT ${b.capacity}%${b.external ? '⚡' : ''}` : null,
    `CPU ${Math.round(r.cpu || 0)}%`,
    s.uptime ? `UP ${fmtDur(s.uptime.seconds)}` : null,
  ].filter(Boolean);
  $('#tm-right').innerHTML = seg.map((x) => `<span>${esc(x)}</span>`).join('');
  put('sys.fw', s.firmware || '—');
  put('sys.up', s.uptime ? fmtDur(s.uptime.seconds) : '—');
}

function renderTmuxWins() {
  $('#tm-wins').innerHTML = PAGES.map((p, i) => `<span class="${p === page ? 'on' : ''}">${i}:${p}${p === page ? '*' : ''}</span>`).join('');
}

/* ---------- overview ---------- */

function kpiRate(key, cur, hist = []) {
  const [v, u] = fmtRate(cur);
  put(`kpi.${key}`, v);
  put(`kpi.${key}.u`, u);
  const peak = hist.length ? Math.max(...hist) : 0;
  const avg = hist.length ? hist.reduce((a, b) => a + b, 0) / hist.length : 0;
  put(`kpi.${key}.peak`, rateText(peak));
  put(`kpi.${key}.avg`, rateText(avg));
  spark(K[`kpi.${key}.spark`], hist.slice(-90), { min: 0, n: 90 });
}

function qualityBar(k, q) {
  const el = K[k];
  if (!el) return;
  el.style.color = q == null ? 'var(--fg-3)' : Q_COLOR[q];
  el.firstElementChild.style.width = q == null ? '0' : `${(q + 1) * 20}%`;
}

let routeSig = '';
function renderOverview(s) {
  const r = s.rates || {}, h = s.history || {}, c = s.cell || {}, p = s.proxy || {}, ts = s.tailscale || {};
  const b = s.battery || {}, m = s.mem || {}, err = s.errors || {};

  // clock skew
  const skew = s.unix ? Date.now() / 1000 - s.unix : 0;
  $('#skew').hidden = Math.abs(skew) <= 300;
  if (Math.abs(skew) > 300) $('#skew-text').textContent = t('skew.text', { dev: s.time, diff: fmtDur(Math.abs(skew)) });

  // KPI
  kpiRate('down', r.wan_down, h.wan_down || []);
  kpiRate('up', r.wan_up, h.wan_up || []);
  const rsrp = numOrNull(c.rsrp), sinr = numOrNull(c.sinr);
  put('kpi.rsrp', rsrp ?? '—');
  const bars = K['kpi.bars'];
  bars.style.color = rsrp == null ? 'var(--fg-3)' : Q_COLOR[rsrpQ(rsrp)];
  [...bars.children].forEach((el, i) => el.classList.toggle('on', i < (c.signalbar || 0)));
  put('kpi.net', [tEnum('net', c.label), c.band].filter(Boolean).join(' · ') || '—');
  put('kpi.sinr', sinr == null ? '—' : sinr + ' dB');
  const sig = (s.signal_history || []).map((x) => x.rsrp);
  const sn = sig.filter((x) => x != null);
  spark(K['kpi.sig.spark'], sig.slice(-60), { min: sn.length ? Math.min(...sn) - 3 : -120, max: sn.length ? Math.max(...sn) + 3 : -60, n: 60 });

  const chain = p.now || [];
  put('kpi.node', p.running ? (chain[chain.length - 1] || p.group || '—') : 'OFFLINE');
  K['kpi.node'].classList.toggle('bad-t', !p.running);
  put('kpi.chain', p.group ? [p.group, ...chain].join(' → ') : (p.running ? '—' : t('proxy.offline')));
  put('kpi.mode', p.running ? String(p.mode || '—').toUpperCase() : 'DOWN');
  K['kpi.mode'].className = 'tag ' + (p.running ? 'acc' : 'bad');
  put('kpi.conns', p.connections ?? '—');

  // throughput
  const toM = (a) => (a || []).map((x) => (x * 8) / 1e6);
  chart.set([
    { key: 'a', label: 'WAN ↓', color: '--a', fill: true, values: toM(h.wan_down) },
    { key: 'b', label: 'WAN ↑', color: '--b', fill: false, values: toM(h.wan_up) },
  ]);

  // cellular
  K['cell.err'].hidden = !err.zte;
  put('cell.err', err.zte ? '[ERR] goform: ' + err.zte : '');
  put('cell.state', c.connected ? t('state.connected') : t('state.disconnected'));
  K['cell.state'].className = 'tag ' + (c.connected ? 'ok' : 'bad');
  put('cell.operator', c.operator || '—');
  put('cell.network', [tEnum('net', c.label), c.band, c.roaming && c.roaming !== 'Home' ? 'ROAMING' : ''].filter(Boolean).join(' · ') || '—');
  put('cell.sim', tEnum('sim', c.sim) || '—');
  put('cell.rsrp', rsrp == null ? '—' : rsrp + ' dBm');
  put('cell.sinr', sinr == null ? '—' : sinr + ' dB');
  qualityBar('cell.rsrp.q', rsrpQ(rsrp));
  qualityBar('cell.sinr.q', sinrQ(sinr));
  put('cell.ip', c.wan_ip || '—');
  put('cell.month', s.month ? `↓ ${fmtBytes(s.month.rx)}  ↑ ${fmtBytes(s.month.tx)}` : '—');
  spark(K['cell.spark'], sig, { min: sn.length ? Math.min(...sn) - 3 : -120, max: sn.length ? Math.max(...sn) + 3 : -60, n: 120 });

  // route
  K['route.err'].hidden = p.running || !err.mihomo;
  put('route.err', !p.running && err.mihomo ? '[ERR] mihomo: ' + err.mihomo : '');
  const hops = [[t('route.clients'), String(s.clients ?? 0)], ['mihomo', p.running ? String(p.mode || '').toUpperCase() : 'DOWN']];
  if (p.running && p.mode !== 'direct' && p.group) {
    hops.push(['group', p.group]);
    chain.forEach((n, i) => hops.push([i === chain.length - 1 ? 'node' : 'group', n]));
  }
  hops.push(['wan', p.running && p.mode === 'direct' ? 'DIRECT' : t('route.internet')]);
  const rs = JSON.stringify(hops) + lang;
  if (rs !== routeSig) {
    routeSig = rs;
    const endIdx = hops.findIndex((x) => x[0] === 'node');
    K['route.chain'].innerHTML = hops.map(([k, v], i) =>
      `<li><span class="hop${i === endIdx ? ' end' : ''}"><small>${esc(k.toUpperCase())}</small><span title="${esc(v)}">${esc(v)}</span></span></li>`).join('');
  }
  put('route.conns', p.running ? p.connections ?? 0 : '—');
  put('route.down', p.running ? fmtBytes(p.download_total) : '—');
  put('route.up', p.running ? fmtBytes(p.upload_total) : '—');
  put('route.core', p.version || '—');

  // Tailscale
  K['ts.off'].hidden = !!ts.enabled;
  K['ts.on'].hidden = !ts.enabled;
  if (ts.enabled) {
    const st = K['ts.state'];
    st.className = 'big-state ' + (ts.online ? 'ok' : ts.checked ? 'bad' : '');
    st.innerHTML = `<i class="dot ${ts.online ? 'ok' : ts.checked ? 'bad' : ''}"></i>${ts.online ? 'ONLINE' : ts.checked ? 'OFFLINE' : '…'}`;
    put('ts.delay', ts.online ? ts.delay + ' ms' : '—');
    put('ts.checked', ts.checked || t('ts.never'));
    put('ts.target', `${ts.name || ''} → ${ts.target || ''}`);
    K['ts.target'].title = ts.online ? '' : ts.error || '';
  }

  // usage
  const rx = s.month?.rx || 0, tx = s.month?.tx || 0;
  put('use.month', s.month ? fmtBytes(rx + tx) : '—');
  put('use.rx', fmtBytes(rx));
  put('use.tx', fmtBytes(tx));
  K['use.rxbar'].style.width = rx + tx ? `${(rx / (rx + tx)) * 100}%` : '50%';
  put('use.lanDown', rateText(r.lan_down));
  put('use.lanUp', rateText(r.lan_up));

  // system
  put('sys.power', b.external ? '⚡ ' + t('power.ext') : t('power.bat'));
  K['sys.power'].className = 'tag ' + (b.external ? 'ok' : 'warn');
  if (b.present) gauge('g.bat', b.capacity / 100, b.capacity + '%', b.capacity < 15 ? 'var(--bad)' : b.capacity < 35 ? 'var(--warn)' : 'var(--ok)');
  const cpu = r.cpu || 0;
  gauge('g.cpu', cpu / 100, Math.round(cpu) + '%', cpu > 85 ? 'var(--bad)' : cpu > 60 ? 'var(--warn)' : 'var(--a)');
  const memUsed = m.total_mb ? 1 - m.available_mb / m.total_mb : 0;
  gauge('g.mem', memUsed, Math.round(memUsed * 100) + '%', memUsed > 0.9 ? 'var(--bad)' : memUsed > 0.75 ? 'var(--warn)' : 'var(--a)');
  const hot = (s.thermals || [])[0];
  if (hot) gauge('g.temp', (hot.temp_c - 20) / 70, hot.temp_c.toFixed(0) + '°', hot.temp_c > 70 ? 'var(--bad)' : hot.temp_c > 55 ? 'var(--warn)' : 'var(--ok)');
  put('sys.uptime', s.uptime ? fmtDur(s.uptime.seconds) : '—');
  put('sys.load', s.load || '—');
  put('sys.wifi', s.wifi ? (s.wifi.on ? s.wifi.ssid : t('state.off')) : '—');
  put('sys.battery', b.present ? `${b.capacity}% · ${tEnum('bat', b.status)} · ${b.temp_c.toFixed(1)}°C` : '—');
  put('sys.hottest', hot ? `${hot.temp_c.toFixed(1)}°C ${hot.name}` : '—');
  put('sys.firmware', s.firmware || '—');

  renderClients(s.client_list, 'clients.table', 'clients.count');
}

function renderClients(list, tableKey, countKey) {
  list = list || [];
  put(countKey, list.length);
  const el = K[tableKey];
  const sig = JSON.stringify(list) + lang;
  if (!el || el.dataset.sig === sig) return;
  el.dataset.sig = sig;
  el.innerHTML = list.length
    ? `<table><thead><tr><th>${t('th.device')}</th><th>${t('th.ip')}</th><th class="num">${t('th.via')}</th></tr></thead><tbody>${
      list.map((c) => `<tr><td>${esc(c.name || '—')}<span class="sub">${esc(c.mac)}</span></td><td>${esc(c.ip)}</td><td class="num"><span class="tag">${esc(c.via)}</span></td></tr>`).join('')}</tbody></table>`
    : `<p class="empty">${t('clients.none')}</p>`;
}

/* ---------- proxy ---------- */

const openGroups = new Set(JSON.parse(store.get('open', '[]')));
const testing = new Set();
let proxySig = '';

async function loadProxies(first) {
  try {
    const d = await api('/api/proxies');
    $('#proxy-err').hidden = true;
    if (first && !openGroups.size && d.groups && d.groups.length) openGroups.add(d.groups[0].name);
    S.proxies = d;
    renderProxies();
  } catch (e) {
    if (e instanceof AuthError || e instanceof NetError) return;
    $('#proxy-err').hidden = false;
    $('#proxy-err').textContent = '[ERR] ' + e.message;
  }
}

function renderProxies(force) {
  const d = S.proxies;
  if (!d) return;
  $$('#mode button').forEach((b) => b.classList.toggle('on', b.dataset.mode === d.mode));
  const q = $('#node-filter').value.trim().toLowerCase();
  const sig = JSON.stringify([d.groups, [...openGroups], [...testing], q, lang]);
  if (sig === proxySig && !force) return;
  proxySig = sig;
  const html = (d.groups || []).map((g) => {
    const nodes = q ? g.nodes.filter((n) => n.name.toLowerCase().includes(q)) : g.nodes;
    if (q && !nodes.length && !g.name.toLowerCase().includes(q)) return '';
    const open = openGroups.has(g.name) || !!q;
    const selectable = g.type === 'Selector';
    const chain = g.chain && g.chain.length ? g.chain : [g.now];
    const cards = open ? `<div class="nodes">${nodes.map((n) => {
      const dc = n.group ? 'd-none' : delayCls(n.delay);
      const width = n.group || n.delay < 0 ? 0 : n.delay === 0 ? 100 : clamp(n.delay / 10, 4, 100);
      return `<button class="node${n.name === g.now ? ' sel' : ''}" data-g="${esc(g.name)}" data-n="${esc(n.name)}" ${selectable ? '' : 'disabled'} title="${esc(n.name)}">
        <span class="nn">${esc(n.name)}</span>
        <span class="nm"><span>${esc(n.group ? t('node.group') : (n.type || '').toLowerCase())}</span><span class="nd ${dc}">${n.group ? '' : delayText(n.delay)}</span></span>
        <i class="nbar ${dc}" style="width:${width}%"></i></button>`;
    }).join('')}</div>` : '';
    return `<article class="panel group${open ? ' open' : ''}">
      <header class="ph" data-toggle="${esc(g.name)}">
        <svg class="ic chev"><use href="#i-chevron"/></svg>
        <h2 class="gname" title="${esc(g.name)}">${esc(g.name)}</h2>
        <span class="tag${selectable ? ' acc' : ''}">${esc(tEnum('gt', g.type))}</span>
        <span class="gnow">${chain.map((n) => `<span class="arrow">→</span> ${esc(n)}`).join(' ')}</span>
        <div class="pa"><span class="dim small">${g.nodes.length}</span>
          <button class="btn sm" data-test="${esc(g.name)}" ${testing.has(g.name) ? 'disabled' : ''}>${testing.has(g.name) ? t('act.testing') + '…' : t('act.test')}</button></div>
      </header>${cards}</article>`;
  }).join('');
  $('#groups').innerHTML = html || `<p class="panel pad dim">${t('proxy.noMatch')}</p>`;
  $('#providers').innerHTML = (d.providers || []).length
    ? `<table><thead><tr><th>${t('th.name')}</th><th>${t('th.kind')}</th><th class="num">${t('th.count')}</th><th class="num">${t('th.updated')}</th><th></th></tr></thead><tbody>${
      d.providers.map((p) => `<tr><td>${esc(p.name)}</td><td><span class="tag">${esc(tEnum('kind', p.kind))}</span></td><td class="num">${p.count.toLocaleString()}</td>
        <td class="num" title="${esc(p.updated_at)}">${relTime(p.updated_at)}</td>
        <td class="num"><button class="btn sm" data-update="${esc(p.name)}" data-kind="${esc(p.kind)}">${t('act.update')}</button></td></tr>`).join('')}</tbody></table>`
    : '<p class="empty">—</p>';
}

$('#groups').addEventListener('click', async (ev) => {
  const testBtn = ev.target.closest('[data-test]');
  if (testBtn) {
    ev.stopPropagation();
    const group = testBtn.dataset.test;
    testing.add(group);
    openGroups.add(group);
    renderProxies();
    try {
      const r = await api('/api/proxies/delay', { group }, { timeout: 30000 });
      if (r.error) toast(t('toast.testFailed', { err: r.error }), true);
      const g = S.proxies.groups.find((x) => x.name === group);
      const tested = new Set(g ? g.nodes.filter((n) => !n.group).map((n) => n.name) : []);
      // a node can appear in several groups; update all of them
      S.proxies.groups.forEach((gg) => gg.nodes.forEach((n) => { if (tested.has(n.name)) n.delay = r.delays[n.name] ?? 0; }));
    } catch (e) { fail(e); }
    testing.delete(group);
    renderProxies(true);
    return;
  }
  const node = ev.target.closest('.node');
  if (node) {
    if (node.disabled) return;
    try {
      await api('/api/proxies/select', { group: node.dataset.g, name: node.dataset.n });
      toast(t('toast.switched', { name: node.dataset.n }));
      await loadProxies();
      statusTick();
    } catch (e) { fail(e); }
    return;
  }
  const head = ev.target.closest('[data-toggle]');
  if (head) {
    const name = head.dataset.toggle;
    openGroups.has(name) ? openGroups.delete(name) : openGroups.add(name);
    store.set('open', JSON.stringify([...openGroups]));
    renderProxies(true);
  }
});

$('#mode').addEventListener('click', async (ev) => {
  const b = ev.target.closest('button[data-mode]');
  if (!b) return;
  try {
    await api('/api/proxies/mode', { mode: b.dataset.mode });
    toast(t('toast.mode', { mode: tEnum('mode', b.dataset.mode) }));
    await loadProxies();
    statusTick();
  } catch (e) { fail(e); }
});

$('#node-filter').addEventListener('input', () => renderProxies());

async function updateProviders(btn, body) {
  await busy(btn, t('act.updating'), async () => {
    const r = await api('/api/proxies/update', body, { timeout: 100000 });
    if (r.failed) {
      const bad = r.results.filter((x) => x.error).map((x) => `${x.name}: ${x.error}`).join('\n');
      toast(`${t('toast.updateFailed', { n: r.failed })}\n${bad}`, true);
    } else {
      toast(t('toast.updated', { n: r.results.length }));
    }
    await loadProxies();
  }).catch(fail);
}
$('#btn-update').addEventListener('click', (ev) => updateProviders(ev.currentTarget, {}));
$('#providers').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-update]');
  if (b) updateProviders(b, { kind: b.dataset.kind, name: b.dataset.update });
});

$('#btn-conns').addEventListener('click', async () => {
  let list = [];
  let timer = null;
  const render = (body) => {
    const q = (body.querySelector('#conn-filter')?.value || '').toLowerCase();
    const rows = list.filter((c) => !q || `${c.host} ${c.source} ${c.rule} ${c.chain}`.toLowerCase().includes(q)).slice(0, 300);
    body.querySelector('#conn-table').innerHTML = rows.length
      ? `<table><thead><tr><th>${t('th.target')}</th><th>${t('th.chain')}</th><th class="num">${t('th.traffic')}</th></tr></thead><tbody>${rows.map((c) =>
        `<tr><td>${esc(c.host)}<span class="sub">${esc(c.network)} · ${esc(c.source)} · ${esc(c.rule)}</span></td><td>${esc(c.chain)}</td>
          <td class="num">↓ ${fmtBytes(c.download)}<span class="sub">↑ ${fmtBytes(c.upload)}</span></td></tr>`).join('')}</tbody></table>`
      : `<p class="empty">${t('conns.none')}</p>`;
    $('#modal-title').textContent = t('conns.title', { n: list.length });
  };
  const refresh = async (body) => {
    try { list = (await api('/api/connections')).connections || []; render(body); } catch (e) { fail(e); }
  };
  const ok = await openModal({
    title: t('conns.title', { n: '…' }), wide: true, danger: true, ok: t('conns.closeAll'), cancel: t('act.close'),
    html: `<label class="search" style="margin-bottom:12px"><svg class="ic"><use href="#i-search"/></svg><input id="conn-filter" type="search" placeholder="${esc(t('conns.filter'))}" autocomplete="off"></label><div class="table-wrap" id="conn-table"><p class="empty">${t('logs.loading')}</p></div>`,
    onOpen: (body) => {
      body.querySelector('#conn-filter').addEventListener('input', () => render(body));
      refresh(body);
      timer = setInterval(() => refresh(body), 3000);
    },
    onClose: () => clearInterval(timer),
  });
  if (ok) {
    try { await api('/api/proxies/close-connections', {}); toast(t('conns.closed')); } catch (e) { fail(e); }
  }
});

/* ---------- device ---------- */

let wifiLoaded = false;

async function loadDevice(first) {
  if (S.status) renderClients(S.status.client_list, 'clients.table2', 'clients.count2');
  if (!S.info) { try { S.info = await api('/api/info'); } catch { /* shown without HTTPS details */ } }
  renderSecure();
  if (first || !wifiLoaded) {
    try { renderWiFi(await api('/api/wifi')); } catch (e) {
      if (e instanceof AuthError || e instanceof NetError) return;
      $('#wifi').innerHTML = `<article class="panel"><div class="pb err">[ERR] ${esc(e.message)}</div></article>`;
    }
  }
}

function renderWiFi(d) {
  wifiLoaded = true;
  const max = d.max_supported || 10;
  $('#wifi').innerHTML = (d.aps || []).map((ap) => {
    const cur = Number(ap.max_clients) || 0;
    const opts = [];
    for (let i = 1; i <= Math.max(max, cur); i++) opts.push(`<option value="${i}"${i === cur ? ' selected' : ''}>${i}</option>`);
    return `<form class="panel wifi-form" data-chip="${esc(ap.chip)}" data-enabled="${ap.enabled ? 1 : 0}" data-band="${esc(ap.band)}" data-max="${cur}">
      <header class="ph"><span class="pi">${ap.chip === '1' ? '5G' : '2.4'}</span><h2>WLAN · ${esc(ap.band)}</h2><div class="pa"><span class="tag ${ap.enabled ? 'ok' : ''}">${ap.enabled ? 'ON' : 'OFF'}</span></div></header>
      <div class="pb">
        <label class="field switch"><span class="lbl">${t('wifi.enabled')}</span><input type="checkbox" name="enabled" ${ap.enabled ? 'checked' : ''}><span class="sw"></span></label>
        <label class="field"><span class="lbl">${t('wifi.ssid')}</span><input type="text" name="ssid" value="${esc(ap.ssid)}" maxlength="32" autocomplete="off" spellcheck="false"></label>
        <div class="field"><span class="lbl">${t('wifi.password')}</span><div class="pw-row">
          <input type="password" name="password" value="${esc(ap.password)}" autocomplete="off" spellcheck="false" aria-label="${esc(t('wifi.password'))}">
          <button class="btn" type="button" data-showpw>${t('act.show')}</button></div></div>
        <label class="field"><span class="lbl">${t('wifi.max')} · <span class="dim">${t('wifi.maxNote', { n: max })}</span></span><select name="max">${opts.join('')}</select></label>
        <label class="field switch"><span class="lbl">${t('wifi.hidden')}</span><input type="checkbox" name="hidden" ${ap.hidden ? 'checked' : ''}><span class="sw"></span></label>
        <p class="note">${esc(t('wifi.note', { auth: ap.auth_mode || '—' }))}</p>
        <button class="btn pri" type="submit">${t('act.save')}</button>
      </div></form>`;
  }).join('');
}

$('#wifi').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-showpw]');
  if (!b) return;
  const inp = b.parentElement.querySelector('input');
  inp.type = inp.type === 'password' ? 'text' : 'password';
  b.textContent = inp.type === 'password' ? t('act.show') : t('act.hide');
});

$('#wifi').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const band = f.dataset.band;
  const maxSel = Number(f.max.value);
  const body = {
    chip: f.dataset.chip,
    enabled: f.enabled.checked,
    ssid: f.ssid.value.trim(),
    password: f.password.value,
    hidden: f.hidden.checked,
    max_clients: maxSel !== Number(f.dataset.max) ? maxSel : 0,
  };
  const switching = (f.dataset.enabled === '1') !== body.enabled;
  const msg = switching
    ? t(body.enabled ? 'wifi.confirmOn' : 'wifi.confirmOff', { band: esc(band) })
    : t('wifi.confirmSave', { ssid: esc(body.ssid) });
  if (!(await confirmBox(msg, { danger: switching && !body.enabled }))) return;
  const btn = f.querySelector('button[type=submit]');
  await busy(btn, t('act.save'), async () => {
    await api('/api/wifi', body);
    toast(switching ? t('wifi.switched', { state: t(body.enabled ? 'wifi.on' : 'wifi.off') }) : t('wifi.saved'));
    wifiLoaded = false;
    setTimeout(() => page === 'device' && loadDevice(true), 1500);
  }).catch(fail);
});

const isStandalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
const isIOS = () => /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
let installEvt = null;

function renderSecure() {
  const info = S.info || {};
  const hs = info.https;
  const parts = [];
  if (isStandalone()) parts.push(`<p><span class="tag ok">PWA</span> ${t('pwa.installed')}</p>`);
  if (!hs) {
    parts.push(`<p class="dim">${t('https.off')}</p>`);
  } else if (location.protocol === 'https:') {
    parts.push(`<p><span class="tag ok">TLS</span> ${t('https.on')}</p>`);
    if (!isStandalone()) {
      if (installEvt) parts.push(`<p><button class="btn pri" data-install><svg class="ic"><use href="#i-install"/></svg>${t('pwa.install')}</button></p>`);
      else if (isIOS()) parts.push(`<p class="dim">${t('pwa.iosHint')}</p>`);
    }
    if (hs.ca_sha256) parts.push(`<p class="fp">${esc(hs.ca_name)} · SHA-256 ${esc(hs.ca_sha256)}</p>`);
  } else {
    parts.push(`<p>${t('https.why')}</p>`);
    parts.push(`<div class="actions">${hs.custom ? '' : `<a class="btn" href="/ca.crt" download="glovebox-ca.crt"><svg class="ic"><use href="#i-install"/></svg>${t('https.download')}</a>`}
      <a class="btn pri" href="${esc(hs.url)}"><svg class="ic"><use href="#i-lock"/></svg>${t('https.open')}</a></div>`);
    if (hs.custom) {
      parts.push(`<p class="dim">${t('https.custom')}</p>`);
    } else {
      parts.push(`<p class="dim small">${esc(t('https.constraint', { hosts: (info.hostnames || []).join(', ') }))}</p>`);
      parts.push(`<p class="fp">${t('https.fp')}: ${esc(hs.ca_sha256 || '')}</p>`);
      const steps = (k) => `<details${(k === 'ios' && isIOS()) || (k === 'android' && /Android/.test(navigator.userAgent)) ? ' open' : ''}><summary>${t(`https.${k}`)}</summary><ol>${[1, 2, 3].map((i) => `<li>${esc(t(`https.${k}.${i}`))}</li>`).join('')}</ol></details>`;
      parts.push(`<div class="secure-steps">${steps('ios')}${steps('android')}${steps('desktop')}</div>`);
    }
  }
  const html = parts.join('');
  const el = $('#secure-body');
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

function updateInstallUI() {
  $$('.side [data-install]').forEach((b) => { b.hidden = !installEvt || isStandalone(); });
  if (page === 'device') renderSecure();
}
addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); installEvt = e; updateInstallUI(); });
addEventListener('appinstalled', () => { installEvt = null; toast(t('pwa.done')); updateInstallUI(); });
document.addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-install]');
  if (!b || !installEvt) return;
  installEvt.prompt();
  try { await installEvt.userChoice; } catch { /* dismissed */ }
  installEvt = null;
  updateInstallUI();
});

$('#btn-box-restart').addEventListener('click', async (ev) => {
  const btn = ev.currentTarget;
  if (!(await confirmBox(t('confirm.restartProxy')))) return;
  await busy(btn, null, async () => {
    await api('/api/box/restart', {});
    toast(t('toast.restartingProxy'));
  }).catch(fail);
});

$('#btn-reboot').addEventListener('click', async () => {
  if (!(await confirmBox(t('confirm.reboot'), { ok: t('act.reboot'), danger: true }))) return;
  try {
    await api('/api/device/reboot', {});
    toast(t('toast.rebooting'));
  } catch (e) { fail(e); }
});

document.addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-sync-time]');
  if (!b) return;
  await busy(b, null, async () => {
    await api('/api/device/time', { unix: Math.round(Date.now() / 1000) });
    toast(t('toast.timeSynced'));
    statusTick();
  }).catch(fail);
});

document.addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-ts-check]');
  if (!b) return;
  await busy(b, t('act.testing'), async () => {
    await api('/api/tailscale/check', {}, { timeout: 30000 });
    toast(t('ts.probed'));
    await statusTick();
  }).catch(fail);
});

document.addEventListener('click', async (ev) => {
  if (!ev.target.closest('[data-logout]')) return;
  try { await api('/api/logout', {}); } catch { /* ignore */ }
  showLogin();
});

$('#pw-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  if ($('#pw-new').value !== $('#pw-again').value) { toast(t('pw.mismatch'), true); return; }
  try {
    await api('/api/console/password', { old: $('#pw-old').value, new: $('#pw-new').value });
    ev.target.reset();
    toast(t('pw.changed'));
  } catch (e) { fail(e); }
});

/* interface preferences */
function applyTheme(name) {
  if (!['ice', 'phosphor', 'amber'].includes(name)) name = 'ice';
  document.documentElement.dataset.theme = name;
  store.set('theme', name);
  $$('#theme-seg button').forEach((b) => b.classList.toggle('on', b.dataset.themeSet === name));
  chart.draw();
}
$('#theme-seg').addEventListener('click', (ev) => { const b = ev.target.closest('[data-theme-set]'); if (b) applyTheme(b.dataset.themeSet); });
$('#lang-seg').addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-lang-set]');
  if (!b) return;
  store.set('lang', b.dataset.langSet);
  await setLang(b.dataset.langSet);
  renderAll();
});
$('#pref-crt').addEventListener('change', (ev) => {
  document.documentElement.classList.toggle('crt', ev.target.checked);
  store.set('crt', ev.target.checked ? '1' : '0');
});

/* ---------- logs ---------- */

const LOG_TITLES = { mihomo: 'tail -f /data/adb/box/run/clash.log', box: 'tail -f /data/adb/box/run/runs.log', glovebox: 'tail -f /data/adb/glovebox/glovebox.log', system: 'logcat -d *:W' };
let logSrc = store.get('logsrc', 'mihomo');
if (!LOG_TITLES[logSrc]) logSrc = 'mihomo';
let logLevel = 'all';
let logText = '';
let logMissing = false;
let follow = true;
const LV_RANK = { debug: 0, info: 1, warn: 2, error: 3 };

function parseLine(line) {
  let m = line.match(/^time="([^"]+)" level=(\w+) msg="(.*)"$/);
  if (m) {
    const lv = { warning: 'warn', fatal: 'error', panic: 'error' }[m[2]] || m[2];
    return { lv, ts: (m[1].match(/T(\d\d:\d\d:\d\d)/) || [])[1] || '', msg: m[3].replace(/\\"/g, '"') };
  }
  m = line.match(/^(\d\d-\d\d \d\d:\d\d:\d\d)\.\d+ ([VDIWEF])\/(.*)$/);
  if (m) return { lv: { V: 'debug', D: 'debug', I: 'info', W: 'warn', E: 'error', F: 'error' }[m[2]], ts: m[1], msg: m[2] + '/' + m[3] };
  m = line.match(/^(\d\d:\d\d:\d\d) \[(\w+)\]:? ?(.*)$/);
  if (m) return { lv: { Debug: 'debug', Info: 'info', Warning: 'warn', Warn: 'warn', Error: 'error' }[m[2]] || 'info', ts: m[1], msg: m[3] };
  m = line.match(/^\d{4}\/\d\d\/\d\d (\d\d:\d\d:\d\d) (.*)$/);
  if (m) return { lv: /panic|fatal/i.test(m[2]) ? 'error' : /error|fail|exited|unavailable|refused/i.test(m[2]) ? 'warn' : 'info', ts: m[1], msg: m[2] };
  return { lv: 'info', ts: '', msg: line };
}

function renderLog() {
  const el = $('#log');
  const q = $('#log-filter').value.trim().toLowerCase();
  const min = logLevel === 'all' ? -1 : LV_RANK[logLevel];
  const hl = (s) => {
    const e = esc(s);
    if (!q) return e;
    const qe = esc(q).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return e.replace(new RegExp(qe, 'gi'), (x) => `<mark>${x}</mark>`);
  };
  const lines = logText.split('\n').filter(Boolean).slice(-2000).map(parseLine)
    .filter((l) => (LV_RANK[l.lv] ?? 1) >= min && (!q || (l.ts + ' ' + l.msg).toLowerCase().includes(q)));
  const tag = { debug: 'DBG', info: 'INF', warn: 'WRN', error: 'ERR' };
  el.innerHTML = lines.length
    ? lines.map((l) => `<div class="l ${l.lv}"><span class="ts">${esc(l.ts)}</span> <span class="lv">${tag[l.lv] || 'INF'}</span> <span class="msg">${hl(l.msg)}</span></div>`).join('')
    : `<div class="l debug">${t(logMissing ? 'logs.missing' : 'logs.empty')}</div>`;
  if (follow) el.scrollTop = el.scrollHeight;
}

async function loadLogs() {
  try {
    const r = await api('/api/logs?src=' + encodeURIComponent(logSrc));
    const text = r.text || '';
    if (text !== logText || !!r.missing !== logMissing) { logText = text; logMissing = !!r.missing; renderLog(); }
  } catch (e) {
    if (e instanceof AuthError || e instanceof NetError) return;
    logText = '';
    $('#log').innerHTML = `<div class="l error"><span class="lv">ERR</span> ${esc(e.message)}</div>`;
  }
}

function setLogSrc(src) {
  logSrc = src;
  store.set('logsrc', src);
  $$('#log-src button').forEach((b) => b.classList.toggle('on', b.dataset.src === src));
  $('#log-title').textContent = LOG_TITLES[src];
  logText = '';
  $('#log').innerHTML = `<div class="l debug">${t('logs.loading')}</div>`;
}
function setFollow(on) {
  follow = on;
  $('#log-follow').setAttribute('aria-pressed', String(on));
  $('#log-follow span').textContent = on ? 'FOLLOW' : 'PAUSED';
  if (on) { $('#log').scrollTop = $('#log').scrollHeight; pageTick(); }
}
$('#log-src').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-src]');
  if (!b) return;
  setLogSrc(b.dataset.src);
  setFollow(true);
  loadLogs();
});
$('#log-level').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-level]');
  if (!b) return;
  logLevel = b.dataset.level;
  $$('#log-level button').forEach((x) => x.classList.toggle('on', x === b));
  renderLog();
});
$('#log-filter').addEventListener('input', renderLog);
$('#log-follow').addEventListener('click', () => setFollow(!follow));
$('#log-wrap').addEventListener('click', (ev) => {
  const on = !$('#log').classList.contains('wrap');
  $('#log').classList.toggle('wrap', on);
  ev.currentTarget.setAttribute('aria-pressed', String(on));
});
// scrolling up pauses follow mode
$('#log').addEventListener('scroll', () => {
  const el = $('#log');
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 30;
  if (follow && !atBottom && el.dataset.user === '1') setFollow(false);
}, { passive: true });
['wheel', 'touchstart', 'keydown'].forEach((e) => $('#log').addEventListener(e, () => { $('#log').dataset.user = '1'; }, { passive: true }));

/* ---------- routing and polling ---------- */

let statusTimer = null, pageTimer = null;
const PAGE_EVERY = { overview: 0, proxy: 10000, device: 0, logs: 2500 };
const visible = () => document.visibilityState === 'visible';

async function statusTick() {
  clearTimeout(statusTimer);
  if (!loggedIn) return;
  if (visible()) {
    try {
      const s = await api('/api/status');
      S.status = s;
      renderChips(s);
      renderStatusLine(s);
      if (page === 'overview') renderOverview(s);
      if (page === 'device') renderClients(s.client_list, 'clients.table2', 'clients.count2');
      $('#crumb').parentElement.title = s.time || '';
    } catch (e) { if (!(e instanceof AuthError) && !(e instanceof NetError)) console.warn(e); }
  }
  if (loggedIn) statusTimer = setTimeout(statusTick, 2000);
}

const loaders = { overview: async () => { if (S.status) renderOverview(S.status); }, proxy: loadProxies, device: loadDevice, logs: loadLogs };

async function pageTick(first) {
  clearTimeout(pageTimer);
  if (!loggedIn) return;
  const p = page;
  if (visible() && (p !== 'logs' || follow || first)) {
    try { await loaders[p](first); } catch (e) { fail(e); }
  }
  if (loggedIn && p === page && PAGE_EVERY[p]) pageTimer = setTimeout(() => pageTick(false), PAGE_EVERY[p]);
}

function setPage(p) {
  if (!PAGES.includes(p)) p = 'overview';
  page = p;
  store.set('page', p);
  $$('[data-page]').forEach((a) => {
    const on = a.dataset.page === p;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  $$('.page').forEach((s) => s.classList.toggle('on', s.id === 'page-' + p));
  $('#crumb').textContent = p;
  document.title = `${p} · Glovebox`;
  renderTmuxWins();
  if (p === 'logs') { $('#log').dataset.user = '0'; }
  pageTick(true);
  if (p === 'overview') requestAnimationFrame(() => chart.resize());
}

function route() { setPage(location.hash.slice(1)); }
addEventListener('hashchange', route);

document.addEventListener('visibilitychange', () => {
  if (visible() && loggedIn) { statusTick(); pageTick(); }
});
addEventListener('online', () => { if (loggedIn) statusTick(); });

// keyboard: 1-4 switch pages, / focuses the filter
document.addEventListener('keydown', (ev) => {
  if (!loggedIn || ev.metaKey || ev.ctrlKey || ev.altKey || modal.open) return;
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName);
  if (typing) {
    if (ev.key === 'Escape') document.activeElement.blur();
    return;
  }
  if (/^[1-4]$/.test(ev.key)) { location.hash = '#' + PAGES[Number(ev.key) - 1]; ev.preventDefault(); }
  if (ev.key === '/') {
    const f = page === 'proxy' ? $('#node-filter') : page === 'logs' ? $('#log-filter') : null;
    if (f) { f.focus(); ev.preventDefault(); }
  }
});

function renderAll() {
  routeSig = '';
  proxySig = '';
  $$('[data-sig]').forEach((el) => { delete el.dataset.sig; });
  if (S.status) { renderChips(S.status); renderStatusLine(S.status); renderOverview(S.status); renderClients(S.status.client_list, 'clients.table2', 'clients.count2'); }
  if (S.proxies) renderProxies(true);
  if (logText || logMissing) renderLog();
  if (wifiLoaded) { wifiLoaded = false; if (page === 'device') loadDevice(true); }
  renderSecure();
}

/* ---------- login and boot ---------- */

function showGate(mode) {
  $('#app').hidden = true;
  $('#gate').hidden = false;
  $('#gate-boot').hidden = mode !== 'boot';
  $('#login-form').hidden = mode !== 'login';
}

function showLogin() {
  loggedIn = false;
  clearTimeout(statusTimer);
  clearTimeout(pageTimer);
  showGate('login');
  $('#login-pw').focus();
}

$('#login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  $('#login-err').textContent = '';
  const btn = ev.submitter || $('#login-form .btn');
  await busy(btn, null, async () => {
    try {
      await api('/api/login', { password: $('#login-pw').value });
      $('#login-pw').value = '';
      enterApp();
    } catch (e) {
      $('#login-err').textContent = '[ERR] ' + e.message;
    }
  });
});

async function enterApp() {
  loggedIn = true;
  $('#gate').hidden = true;
  $('#app').hidden = false;
  if (!location.hash) history.replaceState(null, '', '#' + store.get('page', 'overview'));
  route();
  statusTick();
  try {
    S.info = await api('/api/info');
    put('info.version', S.info.version + (S.info.demo ? ' · demo' : ''));
    renderSecure();
  } catch { /* retried when the device page opens */ }
}

async function boot() {
  const host = location.host || 'glovebox';
  $('#gate-host').textContent = host;
  $('#side-host').textContent = 'root@' + host.replace(/:\d+$/, '');
  showGate('boot');
  try {
    const r = await api('/api/session');
    r.logged_in ? enterApp() : showLogin();
  } catch {
    $('#gate-boot').innerHTML = `<span class="bad-t">[FAIL]</span> ${esc(t('gate.unreachable'))}<span class="caret"></span>`;
    setTimeout(boot, 3000);
  }
}

setInterval(() => {
  const d = new Date();
  $('#clock').textContent = d.toTimeString().slice(0, 8);
}, 1000);

applyTheme(store.get('theme', 'ice'));
$('#pref-crt').checked = store.get('crt', '0') === '1';
document.documentElement.classList.toggle('crt', $('#pref-crt').checked);
setLogSrc(logSrc);
$$('#log-level button')[0].classList.add('on');
renderTmuxWins();

if ('serviceWorker' in navigator && window.isSecureContext) {
  const hadController = !!navigator.serviceWorker.controller;
  navigator.serviceWorker.register('/sw.js').catch(() => { /* fails while the local CA is not trusted */ });
  // a new version took over: the running page still has the old code
  navigator.serviceWorker.addEventListener('controllerchange', () => { if (hadController) toast(t('pwa.updated')); });
}

initI18n().finally(boot);
