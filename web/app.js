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
// dual renders a terminal label (ONLINE) next to its translated text; the skin shows one of them
const dual = (term, key) => `<span class="tt">${term}</span><span class="tn">${esc(t(key))}</span>`;

function applyI18n() {
  document.documentElement.lang = lang;
  $$('[data-i18n]').forEach((el) => { el.textContent = t(el.dataset.i18n); });
  $$('[data-i18n-ph]').forEach((el) => { el.placeholder = t(el.dataset.i18nPh); });
  $('#lang-seg').innerHTML = Object.entries(locales).map(([code, name]) =>
    `<button type="button" data-lang-set="${esc(code)}" lang="${esc(code)}"${code === lang ? ' class="on"' : ''}>${esc(name)}</button>`).join('');
  renderThemes();
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
const Q_COLOR = ['var(--bad)', 'var(--orange)', 'var(--warn)', 'var(--ok)', 'var(--ok)'];

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
  // quick action tiles keep their icon and name and show the progress in their status line
  const el = btn.querySelector('.qa-st') || btn;
  const old = el.innerHTML;
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  if (label) el.textContent = label + '…';
  try { return await fn(); } finally { btn.disabled = false; btn.removeAttribute('aria-busy'); el.innerHTML = old; }
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

    const glow = parseFloat(v('--glow')) || 0;
    ctx.font = '10px ' + (v('--font-num') || 'monospace');
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
      ctx.shadowBlur = 8 * glow;
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
  const bad = s.health?.bad || [];
  K['chip.alert'].hidden = !bad.length;
  if (bad.length) {
    chip('chip.alert', 'bad', String(bad.length));
    K['chip.alert'].title = bad.map((n) => tEnum('hc', n)).join(', ');
  }
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
  K['kpi.egress'].dataset.go = p.group ? groupLink(p.group) : '#proxy';
  renderQuick(s);

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
  // each hop links to where it is managed; a node opens the group it was picked in
  const hops = [[t('route.clients'), String(s.clients ?? 0), '#device/clients'], ['mihomo', p.running ? String(p.mode || '').toUpperCase() : 'DOWN', '#proxy']];
  if (p.running && p.mode !== 'direct' && p.group) {
    hops.push(['group', p.group, groupLink(p.group)]);
    chain.forEach((n, i) => hops.push(i === chain.length - 1 ? ['node', n, groupLink(i ? chain[i - 1] : p.group)] : ['group', n, groupLink(n)]));
  }
  hops.push(['wan', p.running && p.mode === 'direct' ? 'DIRECT' : t('route.internet'), '#overview/cell']);
  const rs = JSON.stringify(hops) + lang;
  if (rs !== routeSig) {
    routeSig = rs;
    const endIdx = hops.findIndex((x) => x[0] === 'node');
    K['route.chain'].innerHTML = hops.map(([k, v, href], i) =>
      `<li><a class="hop${i === endIdx ? ' end' : ''}" href="${esc(href)}"><small>${esc(k.toUpperCase())}</small><span title="${esc(v)}">${esc(v)}</span></a></li>`).join('');
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
    st.innerHTML = `<i class="dot ${ts.online ? 'ok' : ts.checked ? 'bad' : ''}"></i>${ts.online ? dual('ONLINE', 'ts.online') : ts.checked ? dual('OFFLINE', 'ts.offline') : '…'}`;
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
  renderHealth();
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

/* ---------- health ---------- */

const H = { data: null, seq: -1, at: 0, busy: false };

// loadHealth refetches /api/health when the status reports a new event, otherwise every 30 s
async function loadHealth(force) {
  const seq = S.status?.health?.seq ?? 0;
  if (H.busy || (!force && H.data && seq === H.seq && Date.now() - H.at < 30000)) return;
  H.busy = true;
  try {
    H.data = await api('/api/health');
    H.seq = H.data.seq;
    H.at = Date.now();
  } catch (e) {
    if (!(e instanceof AuthError) && !(e instanceof NetError)) console.warn(e);
  } finally { H.busy = false; }
  if (page === 'overview') renderHealth();
  if (page === 'device') renderReport();
}

const pad2 = (n) => String(n).padStart(2, '0');
function fmtClock(iso) {
  const d = new Date(iso);
  return isNaN(d) ? '—' : `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}
// devAgo is relTime for a time taken from the device clock, which may differ from this one
function devAgo(iso) {
  const ts = Date.parse(iso);
  if (!ts || iso.startsWith('0001')) return '—';
  const skew = S.status?.unix ? Date.now() - S.status.unix * 1000 : 0;
  return relTime(new Date(ts + skew).toISOString());
}

function evText(e) {
  if (e.check === 'update') return t('ev.update.' + e.state, { msg: e.msg || '' });
  const check = tEnum('hc', e.check);
  if (e.state === 'fail') return t('alert.fail', { check });
  if (e.state === 'ok') return t('alert.ok', { check, dur: fmtDur(e.dur) });
  return t('ev.' + e.check, { msg: e.msg || '', dur: fmtDur(e.dur) });
}

const HC_LOG = { mihomo: 'box', dns: 'mihomo', proxy: 'mihomo', tailscale: 'mihomo' };

function renderHealth() {
  const d = H.data;
  if (!d) return;
  const checks = (d.checks || []).filter((c) => c.state !== 'off');
  const bad = checks.filter((c) => c.state === 'fail');
  put('hc.summary', bad.length ? t('hc.failing', { n: bad.length }) : t('hc.allOk'));
  K['hc.summary'].className = 'tag ' + (bad.length ? 'bad' : 'ok');
  const el = K['hc.events'];
  // the relative times change every minute
  const sig = JSON.stringify([checks, d.seq, lang, Math.floor(Date.now() / 60000)]);
  if (el.dataset.sig === sig) return;
  el.dataset.sig = sig;
  const dot = { ok: 'ok', fail: 'bad', skip: 'warn' };
  K['hc.checks'].innerHTML = checks.map((c) => {
    const tip = c.state === 'fail' ? c.msg : c.state === 'skip' ? t('hc.skipped') : c.state ? '' : t('hc.waiting');
    const label = `<i class="dot ${dot[c.state] || ''}"></i>${esc(tEnum('hc', c.name))}`;
    // a failing check links to the log most likely to say why
    return `<li class="${esc(c.state || 'wait')}" title="${esc(tip)}">${c.state === 'fail' ? `<a href="#logs/${HC_LOG[c.name] || 'system'}">${label}</a>` : label}</li>`;
  }).join('');
  K['hc.errs'].innerHTML = bad.map((c) =>
    `<p class="err small"><b>${esc(tEnum('hc', c.name))}</b> · ${esc(devAgo(c.since))} · ${esc(c.msg || '')}</p>`).join('');
  const evs = (d.events || []).slice(0, 20);
  const tag = { fail: 'bad', ok: 'ok' };
  el.innerHTML = evs.length
    ? `<table class="events"><thead><tr><th>${t('th.time')}</th><th>${t('th.event')}</th></tr></thead><tbody>${evs.map((e) =>
      `<tr><td>${esc(fmtClock(e.time))}<span class="sub">${esc(devAgo(e.time))}</span></td><td><span class="tag ${tag[e.state] || ''}">${esc(String(e.state).toUpperCase())}</span> ${esc(evText(e))}${
        e.state === 'fail' && e.msg ? `<span class="sub">${esc(e.msg)}</span>` : ''}</td></tr>`).join('')}</tbody></table>`
    : `<p class="empty">${t('hc.none')}</p>`;
}

function renderReport() {
  const rep = S.info?.report || {};
  const on = rep.telegram || rep.webhook || rep.whatsapp || rep.logs;
  $('[data-report-test]').hidden = !on;
  let html = `<p class="dim">${esc(t('rep.off'))}</p>`;
  if (on) {
    const ch = H.data?.channels || {};
    const cell = (k, label) => {
      if (!rep[k]) return `<div class="off"><dt>${label}</dt><dd>${t('rep.disabled')}</dd><dd class="sub">&nbsp;</dd></div>`;
      const c = ch[k] || {};
      const pend = !c.pending ? '' : k === 'logs' ? t('rep.behind', { size: fmtBytes(c.pending) }) : t('rep.queued', { n: c.pending });
      const extra = [pend, c.dropped ? t('rep.dropped', { n: c.dropped }) : ''].filter(Boolean).join(' · ');
      return `<div><dt>${label}</dt><dd><i class="dot ${c.error ? 'bad' : c.last_ok ? 'ok' : ''}"></i> ${
        esc(c.last_ok ? t('rep.last', { t: devAgo(c.last_ok) }) : t('rep.never'))}</dd><dd class="sub">${esc(extra) || '&nbsp;'}</dd></div>`;
    };
    const errs = ['telegram', 'whatsapp', 'webhook', 'logs'].filter((k) => rep[k] && ch[k]?.error)
      .map((k) => `<p class="err small">${k}: ${esc(ch[k].error)}</p>`).join('');
    html = `<dl class="stats c4">${cell('telegram', dual('TELEGRAM', 'rep.telegram'))}${cell('whatsapp', dual('WHATSAPP', 'rep.whatsapp'))}${
      cell('webhook', dual('WEBHOOK', 'rep.webhook'))}${cell('logs', dual('LOGS', 'rep.logs'))}</dl>${errs}`;
  }
  const el = $('#report-body');
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

document.addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-report-test]');
  if (!b) return;
  await busy(b, t('act.testing'), async () => {
    const r = await api('/api/report/test', {}, { timeout: 40000 });
    toast(t('rep.sent', { list: (r.sent || []).join(', ') }));
  }).catch(fail);
  loadHealth(true);
});

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
    S.proxiesAt = Date.now();
    renderProxies();
    if (S.status) renderQuick(S.status);
  } catch (e) {
    if (e instanceof AuthError || e instanceof NetError) return;
    $('#proxy-err').hidden = false;
    $('#proxy-err').textContent = '[ERR] ' + e.message;
  }
}

const groupLink = (name) => '#proxy/' + encodeURIComponent(name);

// nodeCard renders one member of a group; clicking it selects that member
function nodeCard(g, n) {
  const dc = n.group ? 'd-none' : delayCls(n.delay);
  const width = n.group || n.delay < 0 ? 0 : n.delay === 0 ? 100 : clamp(n.delay / 10, 4, 100);
  return `<button type="button" class="node${n.name === g.now ? ' sel' : ''}" data-g="${esc(g.name)}" data-n="${esc(n.name)}" ${g.type === 'Selector' ? '' : 'disabled'} title="${esc(n.name)}">
    <span class="nn">${esc(n.name)}</span>
    <span class="nm"><span>${esc(n.group ? t('node.group') : (n.type || '').toLowerCase())}</span><span class="nd ${dc}">${n.group ? '' : delayText(n.delay)}</span></span>
    <i class="nbar ${dc}" style="width:${width}%"></i></button>`;
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
    const cards = open ? `<div class="nodes">${nodes.map((n) => nodeCard(g, n)).join('')}</div>` : '';
    return `<article class="panel group${open ? ' open' : ''}" data-group="${esc(g.name)}">
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

// testGroup runs a delay test on a group's nodes and resolves to the result, or null when it failed
async function testGroup(group) {
  testing.add(group);
  renderProxies();
  if (S.status) renderQuick(S.status);
  let r = null;
  try {
    r = await api('/api/proxies/delay', { group }, { timeout: 30000 });
    if (r.error) toast(t('toast.testFailed', { err: r.error }), true);
    const groups = S.proxies?.groups || [];
    const g = groups.find((x) => x.name === group);
    const tested = new Set(g ? g.nodes.filter((n) => !n.group).map((n) => n.name) : []);
    // a node can appear in several groups; update all of them
    if (r.delays) groups.forEach((gg) => gg.nodes.forEach((n) => { if (tested.has(n.name)) n.delay = r.delays[n.name] ?? 0; }));
  } catch (e) { fail(e); }
  testing.delete(group);
  renderProxies(true);
  if (S.status) renderQuick(S.status);
  return r;
}

async function selectNode(group, name) {
  try {
    await api('/api/proxies/select', { group, name });
    toast(t('toast.switched', { name }));
    await loadProxies();
    statusTick();
  } catch (e) { fail(e); }
}

async function setMode(mode) {
  try {
    await api('/api/proxies/mode', { mode });
    toast(t('toast.mode', { mode: tEnum('mode', mode) }));
    await loadProxies();
    statusTick();
  } catch (e) { fail(e); }
}

$('#groups').addEventListener('click', (ev) => {
  const testBtn = ev.target.closest('[data-test]');
  if (testBtn) {
    ev.stopPropagation();
    openGroups.add(testBtn.dataset.test);
    testGroup(testBtn.dataset.test);
    return;
  }
  const node = ev.target.closest('.node');
  if (node) {
    if (!node.disabled) selectNode(node.dataset.g, node.dataset.n);
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

// the proxy page toolbar and the overview's quick actions both switch the mode
$$('#mode, #qa-mode').forEach((seg) => seg.addEventListener('click', (ev) => {
  const b = ev.target.closest('button[data-mode]');
  if (b && !b.disabled && !b.classList.contains('on')) setMode(b.dataset.mode);
}));

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

async function openConnections() {
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
    try { await api('/api/proxies/close-connections', {}); toast(t('conns.closed')); statusTick(); } catch (e) { fail(e); }
  }
}
$('#btn-conns').addEventListener('click', () => openConnections());

/* ---------- device ---------- */

let wifiLoaded = false;

async function loadDevice(first) {
  if (S.status) renderClients(S.status.client_list, 'clients.table2', 'clients.count2');
  if (!S.info) { try { S.info = await api('/api/info'); } catch { /* shown without HTTPS details */ } }
  renderSecure();
  renderReport();
  loadHealth(first);
  if (first || !upd) loadUpdate();
  loadSMS(first);
  loadDiag(first);
  if (first || !wifiLoaded) {
    try { renderWiFi(await api('/api/wifi')); } catch (e) {
      if (e instanceof AuthError || e instanceof NetError) return;
      $('#wifi').innerHTML = `<article class="panel"><div class="pb err">[ERR] ${esc(e.message)}</div></article>`;
    }
  }
}

let wifiData = null;

// the F50 runs one band at a time, so the bands are a choice rather than two switches
function bandSeg(d) {
  const bandBtn = (chip, label, sub) => `<button type="button" data-wifi-band="${esc(chip)}" class="${d.active === chip ? 'on' : ''}" aria-pressed="${d.active === chip}"><b>${esc(label)}</b>${sub ? `<small>${esc(sub)}</small>` : ''}</button>`;
  return `<div class="seg">${(d.aps || []).map((ap) => bandBtn(ap.chip, ap.band, ap.ssid)).join('')}${bandBtn('', 'OFF', '')}</div>`;
}

function renderWiFi(d) {
  wifiLoaded = true;
  wifiData = d;
  const max = d.max_supported || 10;
  const aps = d.aps || [];
  const band = `<article class="panel s12 wifi-band">
    <header class="ph"><span class="pi">≋</span><h2><span class="tt">WLAN</span><span class="tn">${esc(t('ttl.wlan'))}</span></h2><span class="sub">${t('wifi.bandSub')}</span><div class="pa"><span class="tag ${d.active ? 'ok' : ''}">${d.active ? 'ON' : 'OFF'}</span></div></header>
    <div class="pb">${bandSeg(d)}
      <p class="note">${t('wifi.bandNote')}</p></div></article>`;
  $('#wifi').innerHTML = band + aps.map((ap) => {
    const cur = Number(ap.max_clients) || 0;
    const opts = [];
    for (let i = 1; i <= Math.max(max, cur); i++) opts.push(`<option value="${i}"${i === cur ? ' selected' : ''}>${i}</option>`);
    return `<form class="panel wifi-form" data-chip="${esc(ap.chip)}" data-on="${ap.enabled ? 1 : 0}" data-max="${cur}">
      <header class="ph"><span class="pi">${ap.chip === '1' ? '5G' : '2.4'}</span><h2><span class="tt">WLAN · ${esc(ap.band)}</span><span class="tn">${esc(t('ttl.wlan'))} · ${esc(ap.band)}</span></h2><div class="pa"><span class="tag ${ap.enabled ? 'ok' : ''}">${ap.enabled ? 'ON' : 'OFF'}</span></div></header>
      <div class="pb">
        <label class="field"><span class="lbl">${t('wifi.ssid')}</span><input type="text" name="ssid" value="${esc(ap.ssid)}" maxlength="32" autocomplete="off" spellcheck="false"></label>
        <div class="field"><span class="lbl">${t('wifi.password')}</span><div class="pw-row">
          <input type="password" name="password" value="${esc(ap.password)}" autocomplete="off" spellcheck="false" aria-label="${esc(t('wifi.password'))}">
          <button class="btn" type="button" data-showpw>${t('act.show')}</button></div></div>
        <label class="field"><span class="lbl">${t('wifi.max')} · <span class="dim">${t('wifi.maxNote', { n: max })}</span></span><select name="max">${opts.join('')}</select></label>
        <label class="field switch"><span class="lbl">${t('wifi.hidden')}</span><input type="checkbox" name="hidden" ${ap.hidden ? 'checked' : ''}><span class="sw"></span></label>
        <p class="note">${esc(t(ap.enabled ? 'wifi.note' : 'wifi.noteIdle', { auth: ap.auth_mode || '—' }))}</p>
        <button class="btn pri" type="submit">${t('act.save')}</button>
      </div></form>`;
  }).join('');
}

// switchBand turns on one band (chip) or, with chip "", Wi-Fi off; both drop every connected device
async function switchBand(chip, btn) {
  const ap = (wifiData?.aps || []).find((a) => a.chip === chip);
  const msg = ap ? t('wifi.confirmBand', { band: esc(ap.band), ssid: esc(ap.ssid) }) : t('wifi.confirmOff');
  if (!(await confirmBox(msg, { danger: true }))) return;
  const run = async () => {
    await api('/api/wifi/band', { chip }, { timeout: 40000 });
    toast(ap ? t('wifi.bandSet', { band: ap.band }) : t('wifi.offDone'));
    wifiLoaded = false;
    wifiData = { ...wifiData, active: chip };
    setTimeout(() => { if (page === 'device') loadDevice(true); statusTick(); }, 3000);
  };
  await (btn ? busy(btn, '', run) : run()).catch(fail);
}

$('#wifi').addEventListener('click', async (ev) => {
  const bb = ev.target.closest('[data-wifi-band]');
  if (bb) {
    if (!bb.classList.contains('on')) switchBand(bb.dataset.wifiBand, bb);
    return;
  }
  const b = ev.target.closest('[data-showpw]');
  if (!b) return;
  const inp = b.parentElement.querySelector('input');
  inp.type = inp.type === 'password' ? 'text' : 'password';
  b.textContent = inp.type === 'password' ? t('act.show') : t('act.hide');
});

$('#wifi').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const maxSel = Number(f.max.value);
  const body = {
    chip: f.dataset.chip,
    ssid: f.ssid.value.trim(),
    password: f.password.value,
    hidden: f.hidden.checked,
    max_clients: maxSel !== Number(f.dataset.max) ? maxSel : 0,
  };
  const ap = (wifiData?.aps || []).find((a) => a.chip === body.chip);
  const msg = f.dataset.on === '1'
    ? t('wifi.confirmSave', { ssid: esc(body.ssid) })
    : t('wifi.confirmSaveIdle', { band: esc(ap ? ap.band : '') });
  if (!(await confirmBox(msg))) return;
  const btn = f.querySelector('button[type=submit]');
  await busy(btn, t('act.save'), async () => {
    await api('/api/wifi', body);
    toast(t('wifi.saved'));
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

async function restartProxy(btn) {
  if (!(await confirmBox(t('confirm.restartProxy')))) return;
  await busy(btn, null, async () => {
    await api('/api/box/restart', {});
    toast(t('toast.restartingProxy'));
    statusTick();
  }).catch(fail);
}

async function rebootDevice() {
  if (!(await confirmBox(t('confirm.reboot'), { ok: t('act.reboot'), danger: true }))) return;
  try {
    await api('/api/device/reboot', {});
    toast(t('toast.rebooting'));
  } catch (e) { fail(e); }
}

$('#btn-box-restart').addEventListener('click', (ev) => restartProxy(ev.currentTarget));
$('#btn-reboot').addEventListener('click', () => rebootDevice());

document.addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-sync-time]');
  if (!b) return;
  await busy(b, null, async () => {
    await api('/api/device/time', { unix: Math.round(Date.now() / 1000) });
    toast(t('toast.timeSynced'));
    statusTick();
  }).catch(fail);
});

/* ---------- software update ---------- */

let upd = null;
let updTimer = 0;
// updFrom is the build that started an install; the page reloads once a different build answers
let updFrom = 0;

async function loadUpdate() {
  clearTimeout(updTimer);
  try {
    upd = await api('/api/update');
  } catch (e) {
    if (e instanceof AuthError) return;
    // the console is down while it restarts into the new version
    if (!updFrom) {
      if (!(e instanceof NetError)) $('#update-body').innerHTML = `<p class="err">[ERR] ${esc(e.message)}</p>`;
      return;
    }
  }
  if (updFrom && upd.code !== updFrom) { location.reload(); return; }
  if (updFrom && !upd.phase) {
    // the install stopped before the restart, or the new version was rolled back
    updFrom = 0;
    if (upd.skipped) toast(t('upd.skipped', { version: upd.skipped }));
  }
  renderUpdate();
  if (updFrom || (upd.phase && page === 'device')) updTimer = setTimeout(loadUpdate, 1500);
}

function renderUpdate() {
  const u = upd;
  if (!u) return;
  const off = u.mode === 'off';
  const kv = (k, v) => `<dt>${esc(t(k))}</dt><dd>${v}</dd>`;
  const rows = [kv('upd.current', `${esc(u.version)}${u.built ? ` <span class="dim small">${esc(relTime(u.built))}</span>` : ''}`)];
  if (!off) {
    if (u.source) rows.push(kv('upd.source', esc(u.source)));
    rows.push(kv('upd.mode', esc(t('upd.mode.' + u.mode) + (u.interval ? ' · ' + t('upd.every', { d: u.interval }) : ''))));
    rows.push(kv('upd.checked', esc(u.checked_at ? devAgo(u.checked_at) : t('ts.never'))));
    let latest = '—';
    if (u.latest) latest = `<b class="ok-t">${esc(u.latest.version)}</b>`;
    else if (u.no_release) latest = esc(t('upd.noRelease'));
    else if (u.checked_at && !u.error) latest = `<i class="dot ok"></i>${esc(t('upd.upToDate'))}`;
    rows.push(kv('upd.latest', latest));
  }
  const parts = [`<dl class="kv">${rows.join('')}</dl>`];
  if (u.latest?.notes) parts.push(`<p class="upd-notes">${esc(u.latest.notes)}</p>`);
  if (u.latest && u.failed) parts.push(`<p class="warn-t small">${esc(t('upd.failedBefore'))}</p>`);
  const last = u.last;
  if (last?.state === 'rolled_back') {
    parts.push(`<p class="err small">${esc(t('upd.rolledBack', { to: last.to, t: devAgo(last.at), reason: last.reason || '' }))}</p>`);
  } else if (last?.state === 'committed' && last.to === u.version) {
    parts.push(`<p class="dim small">${esc(t('upd.lastOk', { from: last.from, t: devAgo(last.at) }))}</p>`);
  }
  if (u.error) parts.push(`<p class="err small">${esc(u.error)}</p>`);
  if (u.skipped) parts.push(`<p class="dim small">${esc(t('upd.skipped', { version: u.skipped }))}</p>`);
  const ph = u.phase || (updFrom ? 'restarting' : '');
  if (ph) {
    const size = u.latest?.size || 0;
    const pct = ph === 'downloading' && size ? clamp(Math.round((u.got || 0) / size * 100), 0, 100) : -1;
    parts.push(`<div class="upd-prog" role="status"><span>${esc(t('upd.phase.' + ph, { pct: pct < 0 ? '' : pct + '%' }))}…</span><div class="upd-bar${pct < 0 ? ' busy' : ''}"><i style="width:${pct < 0 ? 100 : pct}%"></i></div></div>`);
  }
  if (off) {
    parts.push(`<p class="dim">${esc(t('upd.off'))}</p>`);
  } else {
    if (!u.managed) parts.push(`<p class="dim small">${esc(t('upd.unmanaged'))}</p>`);
    const dis = ph ? ' disabled' : '';
    parts.push(`<div class="actions"><button class="btn" data-upd-check${dis}><svg class="ic"><use href="#i-refresh"/></svg><span>${esc(t('upd.check'))}</span></button>${
      u.latest && u.managed ? `<button class="btn pri" data-upd-install${dis}><span>${esc(t('upd.install', { version: u.latest.version }))}</span></button>` : ''}</div>`);
  }
  const html = parts.join('');
  const el = $('#update-body');
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

/* ---------- SMS ---------- */

let sms = null;
let smsAt = 0;
// ids already sent to /api/sms/read, so a slow reply does not mark them twice
const smsMarked = new Set();

async function loadSMS(force) {
  if (!force && Date.now() - smsAt < 10000) return;
  smsAt = Date.now();
  try {
    sms = await api('/api/sms');
  } catch (e) {
    if (!(e instanceof AuthError) && !(e instanceof NetError)) $('#sms-body').innerHTML = `<p class="err">[ERR] ${esc(e.message)}</p>`;
    return;
  }
  renderSMS();
  markSMSRead();
}

function renderSMS() {
  const d = sms;
  if (!d) return;
  const un = $('#sms-unread');
  un.hidden = !d.unread;
  un.className = 'tag acc';
  un.textContent = t('sms.unread', { n: d.unread });
  $('#sms-sub').textContent = d.number ? t('sms.number', { n: d.number }) : t('p.sms');
  const names = { telegram: 'Telegram', whatsapp: 'WhatsApp', webhook: 'Webhook' };
  const meta = d.forward?.length ? t('sms.forward', { list: d.forward.map((k) => names[k] || k).join(', ') }) : t('sms.forwardOff');
  const parts = [`<p class="sms-meta">${esc(meta)}</p>`];
  if (d.error) parts.push(`<p class="err small">${esc(d.error)}</p>`);
  const items = (d.messages || []).map((m) => {
    const out = m.state === 'sent' || m.state === 'failed' || m.state === 'draft';
    const tag = { unread: 'acc', sent: '', failed: 'bad', draft: '' }[m.state];
    const code = m.code ? `<button class="sms-code" type="button" data-sms-copy="${esc(m.code)}" title="${esc(t('sms.copy'))}">${esc(m.code)}<svg class="ic"><use href="#i-copy"/></svg></button>` : '';
    return `<li class="sms-item ${out ? 'out' : 'in'} ${m.state}" data-id="${m.id}">
      <div class="sms-head"><b>${esc(out ? t('sms.toN', { n: m.number }) : m.number)}</b>${
        tag !== undefined ? `<span class="tag ${tag}">${esc(t('sms.state.' + m.state))}</span>` : ''}<span class="sms-time" title="${esc(fmtClock(m.time))}">${esc(m.time ? relTime(m.time) : '')}</span></div>
      <p class="sms-text">${esc(m.text)}</p>
      <div class="sms-acts">${code}${out ? '' : `<button class="btn sm ghost" type="button" data-sms-reply="${esc(m.number)}">${esc(t('sms.reply'))}</button>`}<button class="btn sm ghost" type="button" data-sms-del="${m.id}">${esc(t('sms.delete'))}</button></div></li>`;
  });
  parts.push(items.length ? `<ul class="sms-list">${items.join('')}</ul>` : `<p class="empty">${esc(t('sms.empty'))}</p>`);
  const html = parts.join('');
  const el = $('#sms-body');
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

// markSMSRead marks the unread messages read once the panel shows them
async function markSMSRead() {
  if (page !== 'device' || !visible()) return;
  const ids = (sms?.messages || []).filter((m) => m.state === 'unread' && !smsMarked.has(m.id)).map((m) => m.id).slice(0, 100);
  if (!ids.length) return;
  ids.forEach((id) => smsMarked.add(id));
  try { await api('/api/sms/read', { ids }); } catch { ids.forEach((id) => smsMarked.delete(id)); }
}

// renderSMSBadge shows the number of unread messages on the Device tab
function renderSMSBadge(s) {
  const n = Number(s.sms_unread) || 0;
  const label = n ? t('sms.unread', { n }) : '';
  $$('[data-sms-badge]').forEach((b) => {
    b.hidden = !n;
    const txt = n > 99 ? '99+' : String(n);
    if (b.textContent !== txt) b.textContent = txt;
    if (b.title !== label) { b.title = label; b.setAttribute('aria-label', label); }
  });
  if (page === 'device' && sms && n !== sms.unread) loadSMS(true);
}

$('#sms-form').addEventListener('input', () => {
  const n = [...$('#sms-form').text.value].length;
  $('#sms-count').textContent = n ? `${n}/500` : '';
});

$('#sms-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const number = f.number.value.replace(/\s+/g, '');
  const text = f.text.value.trim();
  if (!number || !text) return;
  if (!(await confirmBox(t('sms.confirm', { number: esc(number) }), { ok: t('sms.send') }))) return;
  await busy(f.querySelector('button[type=submit]'), t('sms.send'), async () => {
    await api('/api/sms/send', { number, text }, { timeout: 30000 });
    toast(t('sms.sent'));
    f.text.value = '';
    $('#sms-count').textContent = '';
    loadSMS(true);
    // the modem reports the result a few seconds later
    setTimeout(() => page === 'device' && loadSMS(true), 6000);
  }).catch(fail);
});

$('#sms-body').addEventListener('click', async (ev) => {
  const c = ev.target.closest('[data-sms-copy]');
  if (c) {
    const code = c.dataset.smsCopy;
    try {
      await navigator.clipboard.writeText(code);
      toast(t('sms.copied', { code }));
    } catch {
      // the clipboard needs HTTPS: select the code so it can be copied by hand
      const sel = getSelection();
      const range = document.createRange();
      range.selectNodeContents(c);
      sel.empty();
      sel.addRange(range);
    }
    return;
  }
  const r = ev.target.closest('[data-sms-reply]');
  if (r) {
    const f = $('#sms-form');
    f.number.value = r.dataset.smsReply;
    f.text.focus();
    return;
  }
  const d = ev.target.closest('[data-sms-del]');
  if (!d) return;
  if (!(await confirmBox(esc(t('sms.confirmDelete')), { danger: true, ok: t('sms.delete') }))) return;
  await busy(d, '', async () => {
    await api('/api/sms/delete', { ids: [Number(d.dataset.smsDel)] });
    toast(t('sms.deleted'));
    loadSMS(true);
  }).catch(fail);
});

/* ---------- remote diagnostics ---------- */

let diag = null;
let diagAt = 0;
// connection lines shown per job; the rest are counted
const DIAG_CONNS = 30;

async function loadDiag(force) {
  if (!force && Date.now() - diagAt < 10000) return;
  diagAt = Date.now();
  try {
    diag = await api('/api/diag');
  } catch (e) {
    if (!(e instanceof AuthError) && !(e instanceof NetError)) $('#diag-body').innerHTML = `<p class="err">[ERR] ${esc(e.message)}</p>`;
    return;
  }
  renderDiag();
}

function renderDiag() {
  const d = diag;
  if (!d) return;
  const kv = (k, v) => `<dt>${esc(t(k))}</dt><dd>${v}</dd>`;
  const parts = [];
  if (d.enabled) {
    const rows = [kv('diag.source', esc(d.source)),
      kv('diag.polled', esc(`${d.polled_at ? devAgo(d.polled_at) : t('ts.never')} · ${t('upd.every', { d: d.interval })}`))];
    if (d.log_level) rows.push(kv('diag.log', esc(t('diag.logUntil', { level: d.log_level, t: fmtClock(d.log_until).slice(6, 11) }))));
    parts.push(`<dl class="kv">${rows.join('')}</dl>`);
    if (d.error) parts.push(`<p class="err small">${esc(d.error)}</p>`);
    if (d.running) parts.push(`<p class="small">${esc(t('diag.running', { id: d.running }))}…</p>`);
  } else {
    parts.push(`<p class="dim">${esc(t('diag.off'))}</p>`);
  }
  const j = d.last;
  if (j) {
    const items = [];
    let conns = 0;
    let hidden = 0;
    for (const r of j.results || []) {
      if (r.kind === 'job') continue;
      if (r.kind === 'connection') {
        if (++conns > DIAG_CONNS) { hidden++; continue; }
        items.push(`<li class="sub"><span class="msg">${esc(r._msg)}</span></li>`);
        continue;
      }
      conns = 0;
      items.push(`<li${r.ok ? '' : ' class="fail"'}><i class="dot ${r.ok ? 'ok' : 'bad'}"></i><span class="tag">${esc(r.kind)}</span><span class="msg">${esc(r._msg)}</span><span class="ms">${esc(String(r.ms))} ms</span></li>`);
    }
    if (hidden) items.push(`<li class="sub dim">${esc(t('diag.moreConns', { n: hidden }))}</li>`);
    const sum = t('diag.summary', { ok: j.ok, failed: j.failed, s: (j.ms / 1000).toFixed(1), t: devAgo(j.at) });
    parts.push(`<div class="diag-job"><p class="diag-head"><b>${esc(j.id)}</b><span class="${j.failed ? 'err' : 'dim'} small">${esc(sum)}</span></p><ol class="diag-res">${items.join('')}</ol></div>`);
  } else if (d.enabled) {
    parts.push(`<p class="dim small">${esc(t('diag.none'))}</p>`);
  }
  const html = parts.join('');
  const el = $('#diag-body');
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

// renderUpdDot marks the version in the sidebar when a newer one is out
function renderUpdDot(s) {
  const a = $('#upd-dot');
  a.hidden = !s.update;
  const label = s.update ? t('ev.update.info', { msg: s.update }) : '';
  if (a.title !== label) { a.title = label; a.setAttribute('aria-label', label); }
  if (page === 'device' && upd && !updFrom && !upd.phase && (s.update || '') !== (upd.latest?.version || '')) loadUpdate();
}

document.addEventListener('click', async (ev) => {
  const c = ev.target.closest('[data-upd-check]');
  if (c) {
    await busy(c, t('upd.phase.checking'), async () => { upd = await api('/api/update/check', {}, { timeout: 40000 }); }).catch(fail);
    renderUpdate();
    return;
  }
  const b = ev.target.closest('[data-upd-install]');
  const v = upd?.latest;
  if (!b || !v) return;
  const ok = await confirmBox(t('upd.confirm', { version: esc(v.version), current: esc(upd.version) }), { ok: t('upd.install', { version: v.version }) });
  if (!ok) return;
  try {
    await api('/api/update/install', { versionCode: v.versionCode });
    updFrom = upd.code;
    loadUpdate();
  } catch (e) { fail(e); loadUpdate(); }
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

/* interface preferences: theme.js applies the theme before the first paint, this renders the picker */
const TH = window.gloveboxTheme;
const THEME_ORDER = ['auto', 'ios', 'material', 'ice', 'phosphor', 'amber', 'dracula', 'nord', 'gruvbox', 'solarized'];
const THEME_NAMES = { ios: 'iOS', material: 'Material You', ice: 'ICE', phosphor: 'PHOSPHOR', amber: 'AMBER', dracula: 'Dracula', nord: 'Nord', gruvbox: 'Gruvbox', solarized: 'Solarized Light' };
// picker previews: page, card, accent, second accent, corner radius; adaptive skins have a light and a dark set
const THEME_PREVIEW = {
  ice: { bg: '#05070a', card: '#0e141b', a: '#4fd8ff', b: '#b388ff' },
  phosphor: { bg: '#05070a', card: '#0e141b', a: '#3dff8b', b: '#ffd84d' },
  amber: { bg: '#05070a', card: '#0e141b', a: '#ffb547', b: '#ff7aa8' },
  dracula: { bg: '#1e1f29', card: '#282a36', a: '#bd93f9', b: '#ff79c6' },
  nord: { bg: '#242933', card: '#2e3440', a: '#88c0d0', b: '#b48ead' },
  gruvbox: { bg: '#1d2021', card: '#282828', a: '#fabd2f', b: '#83a598' },
  solarized: { bg: '#fdf6e3', card: '#eee8d5', a: '#268bd2', b: '#d33682' },
  ios: { light: { bg: '#f2f2f7', card: '#ffffff', a: '#007aff', b: '#5856d6', r: '6px' }, dark: { bg: '#000000', card: '#1c1c1e', a: '#0a84ff', b: '#5e5ce6', r: '6px' } },
  material: { light: { bg: '#fef7ff', card: '#f3edf7', a: '#6750a4', b: '#7d5260', r: '8px' }, dark: { bg: '#141218', card: '#211f26', a: '#d0bcff', b: '#efb8c8', r: '8px' } },
};
function renderThemes() {
  const r = TH.resolve();
  const adaptive = r.appearance !== 'auto' ? r.appearance : matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  $('#theme-grid').innerHTML = THEME_ORDER.map((name) => {
    const id = name === 'auto' ? TH.deviceTheme() : name;
    const fixed = TH.THEMES[id].mode;
    const c = fixed ? THEME_PREVIEW[id] : THEME_PREVIEW[id][adaptive];
    const label = name === 'auto' ? t('theme.auto') : THEME_NAMES[name];
    const sub = name === 'auto' ? THEME_NAMES[id] : fixed ? t('theme.terminal') : t('theme.adaptive');
    const on = r.name === name;
    return `<button type="button" class="theme-card${on ? ' on' : ''}" data-theme-set="${name}" role="radio" aria-checked="${on}"
      style="--tp-bg:${c.bg};--tp-card:${c.card};--tp-a:${c.a};--tp-b:${c.b};--tp-r:${c.r || '0px'}"><span class="theme-prev" aria-hidden="true"><i></i><b></b><em></em></span><span>${esc(label)}</span><small>${esc(sub)}</small></button>`;
  }).join('');
  $$('#appear-seg button').forEach((b) => { b.classList.toggle('on', b.dataset.appear === r.appearance); b.disabled = r.fixedMode; });
  $('#appear-note').hidden = !r.fixedMode;
  $('#crt-field').hidden = r.skin !== 'terminal';
  document.documentElement.classList.toggle('crt', r.skin === 'terminal' && $('#pref-crt').checked);
}
$('#theme-grid').addEventListener('click', (ev) => { const b = ev.target.closest('[data-theme-set]'); if (b) TH.choose(b.dataset.themeSet); });
$('#appear-seg').addEventListener('click', (ev) => { const b = ev.target.closest('[data-appear]'); if (b && !b.disabled) TH.setAppearance(b.dataset.appear); });
// theme.js fires this on a choice here and when the system switches between light and dark
window.addEventListener('glovebox:theme', () => { renderThemes(); chart.draw(); });

// iOS / Material: the page's large title folds into the top bar once it scrolls away
function foldTitle() { document.documentElement.classList.toggle('scrolled', window.scrollY > 30); }
window.addEventListener('scroll', foldTitle, { passive: true });
$('#lang-seg').addEventListener('click', async (ev) => {
  const b = ev.target.closest('[data-lang-set]');
  if (!b) return;
  store.set('lang', b.dataset.langSet);
  await setLang(b.dataset.langSet);
  renderAll();
});
$('#pref-crt').addEventListener('change', (ev) => {
  store.set('crt', ev.target.checked ? '1' : '0');
  renderThemes();
});

/* ---------- quick actions ---------- */

function bandText(s) {
  if (!s.wifi) return '—';
  if (!s.wifi.on) return t('state.off');
  const ap = wifiData && (wifiData.aps || []).find((a) => a.chip === wifiData.active);
  return ap ? `${ap.band} · ${ap.ssid}` : s.wifi.ssid || '—';
}

// renderQuick fills the overview tiles' status lines; a busy tile keeps its progress text
function renderQuick(s) {
  const p = s.proxy || {};
  const tile = (k, v, off) => {
    const btn = K[k].closest('.qa');
    if (btn.hasAttribute('aria-busy')) return;
    btn.disabled = !!off;
    put(k, v);
  };
  $$('#qa-mode button').forEach((b) => { b.classList.toggle('on', !!p.running && b.dataset.mode === p.mode); b.disabled = !p.running; });
  const chain = p.now || [];
  const down = !p.running;
  tile('qa.node', down ? t('proxy.offline') : chain[chain.length - 1] || p.group || '—', down || !p.group);
  tile('qa.test', testing.has(p.group) ? t('act.testing') + '…' : p.group || '—', down || !p.group || testing.has(p.group));
  const updated = (S.proxies?.providers || []).filter((x) => x.kind === 'proxies').map((x) => Date.parse(x.updated_at)).filter((x) => x > 0);
  tile('qa.update', updated.length ? t('qa.updated', { t: relTime(new Date(Math.min(...updated)).toISOString()) }) : '—', down);
  tile('qa.conns', down ? '—' : t('qa.connsN', { n: p.connections ?? 0 }), down);
  tile('qa.band', bandText(s));
  tile('qa.restart', down ? t('qa.stopped') : ['mihomo', p.version].filter(Boolean).join(' '));
  tile('qa.reboot', s.uptime ? `${t('sys.uptime')} ${fmtDur(s.uptime.seconds)}` : '—');
  // the node and subscription tiles read the proxy list, which the overview refreshes now and then
  if (p.running && Date.now() - Math.max(S.proxiesTry || 0, S.proxiesAt || 0) > 60000) {
    S.proxiesTry = Date.now();
    loadProxies();
  }
}

// openNodeSheet lists the main group's nodes; picking one switches to it
async function openNodeSheet() {
  if (Date.now() - (S.proxiesAt || 0) > 10000) await loadProxies();
  const name = S.status?.proxy?.group;
  const g0 = (S.proxies?.groups || []).find((x) => x.name === name);
  if (!g0) { location.hash = '#proxy'; return; }
  let box = null;
  const draw = () => {
    if (!box?.isConnected) return;
    const g = S.proxies.groups.find((x) => x.name === name) || g0;
    box.querySelector('.nodes').innerHTML = g.nodes.map((n) => nodeCard(g, n)).join('');
    const tb = box.querySelector('[data-sheet-test]');
    tb.disabled = testing.has(name);
    tb.textContent = testing.has(name) ? t('act.testing') + '…' : t('act.test');
  };
  await openModal({
    title: name, wide: true, ok: '', cancel: t('act.close'),
    html: `<div class="sheet"><div class="sheet-tools"><span class="tag${g0.type === 'Selector' ? ' acc' : ''}">${esc(tEnum('gt', g0.type))}</span>
      <a class="btn ghost sm" href="${esc(groupLink(name))}">${t('act.manage')}</a><button type="button" class="btn sm" data-sheet-test>${t('act.test')}</button></div><div class="nodes"></div></div>`,
    onOpen: (body) => {
      box = body.querySelector('.sheet');
      draw();
      box.addEventListener('click', async (ev) => {
        if (ev.target.closest('a')) { modal.close(); return; }
        if (ev.target.closest('[data-sheet-test]')) {
          const done = testGroup(name);
          draw();
          await done;
          draw();
          return;
        }
        const node = ev.target.closest('.node');
        if (node && !node.disabled) { modal.close(); selectNode(node.dataset.g, node.dataset.n); }
      });
    },
  });
}

// quickTest tests the main group and names its fastest proxy (not DIRECT or a nested group)
async function quickTest() {
  const group = S.status?.proxy?.group;
  if (!group || testing.has(group)) return;
  if (!S.proxies) await loadProxies();
  const r = await testGroup(group);
  if (!r || r.error) return;
  const nodes = (S.proxies?.groups || []).find((g) => g.name === group)?.nodes || [];
  const real = new Set(nodes.filter((n) => !n.group && !/^(direct|reject)/i.test(n.type || '')).map((n) => n.name));
  const [name, ms] = Object.entries(r.delays || {}).filter(([n, d]) => d > 0 && real.has(n)).sort((a, b) => a[1] - b[1])[0] || [];
  toast(name ? t('toast.fastest', { name, ms }) : t('toast.testFailed', { err: t('node.timeout') }), !name);
}

// openBandSheet offers the bands; the choice is confirmed by switchBand like on the device page
async function openBandSheet(btn) {
  try { wifiData = await api('/api/wifi'); } catch (e) { fail(e); return; }
  let pick = null;
  await openModal({
    title: t('qa.band'), ok: '', cancel: t('act.close'),
    html: `<div class="sheet wifi-band">${bandSeg(wifiData)}<p class="note">${t('qa.bandNote')}</p>
      <p><a class="btn ghost sm" href="#device/wifi">${t('act.manage')}</a></p></div>`,
    onOpen: (body) => body.querySelector('.sheet').addEventListener('click', (ev) => {
      if (ev.target.closest('a')) { modal.close(); return; }
      const b = ev.target.closest('[data-wifi-band]');
      if (!b || b.classList.contains('on')) return;
      pick = b.dataset.wifiBand;
      modal.close();
    }),
  });
  if (pick !== null) switchBand(pick, btn);
}

const QUICK = {
  node: () => openNodeSheet(),
  test: () => quickTest(),
  update: (b) => updateProviders(b, {}),
  conns: () => openConnections(),
  band: (b) => openBandSheet(b),
  restart: (b) => restartProxy(b),
  reboot: () => rebootDevice(),
};
$('#quick').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-qa]');
  if (b && !b.disabled) QUICK[b.dataset.qa](b);
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
// setFollow turns follow mode on or off; turning it on reloads the log unless the caller is about to
function setFollow(on, quiet) {
  follow = on;
  $('#log-follow').setAttribute('aria-pressed', String(on));
  $('#log-follow .tt').textContent = on ? 'FOLLOW' : 'PAUSED';
  const tn = $('#log-follow .tn');
  tn.dataset.i18n = on ? 'logs.follow' : 'logs.paused';
  tn.textContent = t(tn.dataset.i18n);
  if (on) { $('#log').scrollTop = $('#log').scrollHeight; if (!quiet) pageTick(); }
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
      renderUpdDot(s);
      if (page === 'overview') renderOverview(s);
      if (page === 'device') renderClients(s.client_list, 'clients.table2', 'clients.count2');
      if (page === 'overview' || page === 'device') loadHealth();
      if (page === 'device') loadDiag();
      renderSMSBadge(s);
      $('#crumb').parentElement.title = s.time || '';
    } catch (e) { if (!(e instanceof AuthError) && !(e instanceof NetError)) console.warn(e); }
  }
  if (loggedIn) statusTimer = setTimeout(statusTick, 2000);
}

const loaders = {
  overview: async () => {
    if (S.status) renderOverview(S.status);
    // the status only says whether Wi-Fi is on; the band tile also names the band
    if (!wifiData) api('/api/wifi').then((d) => { wifiData = d; if (S.status) renderQuick(S.status); }).catch(() => { /* the tile shows the SSID */ });
  },
  proxy: loadProxies, device: loadDevice, logs: loadLogs,
};

async function pageTick(first) {
  clearTimeout(pageTimer);
  if (!loggedIn) return;
  const p = page;
  if (visible() && (p !== 'logs' || follow || first)) {
    try { await loaders[p](first); } catch (e) { fail(e); }
  }
  if (loggedIn && p === page && PAGE_EVERY[p]) pageTimer = setTimeout(() => pageTick(false), PAGE_EVERY[p]);
}

// setPage shows a page; a target (#proxy/<group>, #device/wifi, #logs/box …) is opened and scrolled to
function setPage(p, target) {
  if (!PAGES.includes(p)) p = 'overview';
  if (p !== page) window.scrollTo(0, 0);
  page = p;
  store.set('page', p);
  $$('[data-page]').forEach((a) => {
    const on = a.dataset.page === p;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  $$('.page').forEach((s) => s.classList.toggle('on', s.id === 'page-' + p));
  $('#crumb').textContent = p;
  const title = $('#top-title');
  title.dataset.i18n = 'nav.' + p;
  title.textContent = t('nav.' + p);
  document.title = `${p} · Glovebox`;
  foldTitle();
  renderTmuxWins();
  if (p === 'logs') {
    $('#log').dataset.user = '0';
    if (LOG_TITLES[target]) {
      if (target !== logSrc) setLogSrc(target);
      if (!follow) setFollow(true, true);
    }
  }
  if (p === 'proxy' && target) openGroups.add(target);
  pageTick(true).then(() => { if (target && page === p) reveal(p, target); });
  if (p === 'overview') requestAnimationFrame(() => chart.resize());
}

// reveal scrolls to the part of a page a link points at and briefly highlights it
let flashTimer = 0;
function reveal(p, target) {
  const el = p === 'proxy' ? $$('#groups .group').find((g) => g.dataset.group === target) : document.getElementById(target);
  if (!el || !el.closest('#page-' + p)) return;
  el.scrollIntoView({ block: 'start', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  clearTimeout(flashTimer);
  $$('.flash').forEach((x) => x.classList.remove('flash'));
  void el.offsetWidth; // restart the animation
  el.classList.add('flash');
  flashTimer = setTimeout(() => el.classList.remove('flash'), 1300);
}

function route() {
  const [p, ...rest] = location.hash.slice(1).split('/');
  let target = rest.join('/');
  try { target = decodeURIComponent(target); } catch { /* a malformed escape: use it as is */ }
  setPage(p, target);
}
addEventListener('hashchange', route);

// [data-go] turns a card into a link: '#page/target' navigates, a name runs that action
const GO = { conns: () => openConnections() };
function go(el) {
  const to = el.dataset.go;
  if (GO[to]) GO[to]();
  else if (location.hash === to) route();
  else location.hash = to;
}
document.addEventListener('click', (ev) => {
  const a = ev.target.closest('a[href^="#"]');
  // following a link to where the page already is reveals its target again
  if (a) { if (a.getAttribute('href') === location.hash) route(); return; }
  const el = ev.target.closest('[data-go]');
  if (el && !ev.target.closest('button, input, select, textarea, label, summary')) go(el);
});
document.addEventListener('keydown', (ev) => {
  if ((ev.key === 'Enter' || ev.key === ' ') && ev.target.matches?.('[data-go]')) { ev.preventDefault(); go(ev.target); }
});

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
  renderHealth();
  renderReport();
  renderUpdate();
  if (S.status) renderUpdDot(S.status);
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
    $('#gate-boot').innerHTML = `<span class="bad-t boot-tag">[FAIL]</span> ${esc(t('gate.unreachable'))}<span class="caret"></span>`;
    setTimeout(boot, 3000);
  }
}

setInterval(() => {
  const d = new Date();
  $('#clock').textContent = d.toTimeString().slice(0, 8);
}, 1000);

$('#pref-crt').checked = store.get('crt', '0') === '1';
renderThemes();
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
