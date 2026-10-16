'use strict';
/* Glovebox service worker: caches the app shell so the installed app opens instantly and shows
 * the link-down state when the device is out of reach. The server replaces __BUILD__ with a hash
 * of all web files, so every upgrade gets a fresh cache. API calls always go to the network. */

const CACHE = 'glovebox-__BUILD__';
const SHELL = [
  '/', '/app.js', '/style.css', '/manifest.webmanifest',
  '/icons/icon.svg', '/icons/icon-192.png', '/icons/favicon-32.png',
  '/fonts/jetbrains-mono-400.woff2', '/fonts/jetbrains-mono-500.woff2', '/fonts/jetbrains-mono-700.woff2',
  '/i18n/index.json',
];

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await cache.addAll(SHELL.map((u) => new Request(u, { cache: 'reload' })));
    try {
      const locales = await (await fetch('/i18n/index.json', { cache: 'reload' })).json();
      await cache.addAll(Object.keys(locales).map((c) => new Request(`/i18n/${c}.json`, { cache: 'reload' })));
    } catch { /* locales are fetched on demand */ }
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    for (const key of await caches.keys()) {
      if (key.startsWith('glovebox-') && key !== CACHE) await caches.delete(key);
    }
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith('/api/') || url.pathname === '/ca.crt') return;
  event.respondWith((async () => {
    const cache = await caches.open(CACHE);
    // the shell is a single page with hash routing, so every navigation gets the cached index
    const key = req.mode === 'navigate' ? '/' : req;
    const hit = await cache.match(key, { ignoreSearch: true });
    if (hit) return hit;
    const res = await fetch(req);
    if (res.ok && res.type === 'basic') cache.put(key, res.clone());
    return res;
  })());
});
