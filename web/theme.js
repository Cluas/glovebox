'use strict';
/* Glovebox themes. Loaded synchronously from <head> so the right skin is set before the first paint
 * (app.js is deferred). A theme picks a skin (the overall look) and, for terminal palettes, a fixed
 * light or dark mode; the iOS and Material skins follow the system unless the user pins a mode. */

(() => {
  const THEMES = {
    ice: { skin: 'terminal', mode: 'dark', bar: '#05070a' },
    phosphor: { skin: 'terminal', mode: 'dark', bar: '#05070a' },
    amber: { skin: 'terminal', mode: 'dark', bar: '#05070a' },
    dracula: { skin: 'terminal', mode: 'dark', bar: '#21222c' },
    nord: { skin: 'terminal', mode: 'dark', bar: '#2e3440' },
    gruvbox: { skin: 'terminal', mode: 'dark', bar: '#1d2021' },
    solarized: { skin: 'terminal', mode: 'light', bar: '#fdf6e3' },
    ios: { skin: 'ios', bar: { light: '#f2f2f7', dark: '#000000' } },
    material: { skin: 'material', bar: { light: '#fef7ff', dark: '#141218' } },
  };
  const APPEARANCES = ['auto', 'light', 'dark'];

  const get = (k) => { try { return localStorage.getItem('glovebox.' + k); } catch { return null; } };
  const set = (k, v) => { try { localStorage.setItem('glovebox.' + k, v); } catch { /* private mode */ } };

  // iPadOS reports itself as a Mac, so a touch screen tells them apart
  function deviceTheme() {
    const ua = navigator.userAgent || '';
    if (/iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) return 'ios';
    if (/Android/.test(ua)) return 'material';
    return 'ice';
  }

  // the saved choice; older versions stored the default ICE on every start, so only an explicit
  // non-default palette from them counts as a choice
  function choice() {
    const v = get('ui.theme');
    if (v === 'auto' || THEMES[v]) return v;
    const old = get('theme');
    return old === 'phosphor' || old === 'amber' ? old : 'auto';
  }

  function appearance() {
    const v = get('ui.appearance');
    return APPEARANCES.includes(v) ? v : 'auto';
  }

  const darkQuery = matchMedia('(prefers-color-scheme: dark)');

  function resolve() {
    const name = choice();
    const id = name === 'auto' ? deviceTheme() : name;
    const th = THEMES[id];
    const ap = appearance();
    const mode = th.mode || (ap === 'auto' ? (darkQuery.matches ? 'dark' : 'light') : ap);
    return { name, id, skin: th.skin, mode, appearance: ap, fixedMode: !!th.mode };
  }

  function meta(name, content) {
    let m = document.querySelector(`meta[name="${name}"]`);
    if (!m) {
      m = document.createElement('meta');
      m.name = name;
      document.head.appendChild(m);
    }
    m.content = content;
  }

  function apply() {
    const r = resolve();
    const root = document.documentElement;
    root.dataset.theme = r.id;
    root.dataset.skin = r.skin;
    root.dataset.mode = r.mode;
    root.style.colorScheme = r.mode;
    const bar = THEMES[r.id].bar;
    meta('color-scheme', r.mode);
    meta('theme-color', typeof bar === 'string' ? bar : bar[r.mode]);
    // read by iOS when an installed app starts; light skins need dark status bar text
    meta('apple-mobile-web-app-status-bar-style', r.mode === 'light' ? 'default' : 'black-translucent');
    window.dispatchEvent(new CustomEvent('glovebox:theme', { detail: r }));
    return r;
  }

  function choose(name) {
    set('ui.theme', name === 'auto' || THEMES[name] ? name : 'auto');
    return apply();
  }

  function setAppearance(v) {
    set('ui.appearance', APPEARANCES.includes(v) ? v : 'auto');
    return apply();
  }

  darkQuery.addEventListener('change', apply);
  window.gloveboxTheme = { THEMES, deviceTheme, resolve, apply, choose, setAppearance };
  apply();
})();
