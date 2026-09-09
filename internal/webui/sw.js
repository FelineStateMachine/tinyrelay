// Service worker: makes the relay installable and keeps navigation working
// when the network fails. Every request goes to the network first. Successful
// page loads and the shell scripts are kept as the offline fallback; relay
// data requested by scripts is never cached.
const CACHE = "tiny-shell-v1";
const shellAsset = url => /\/(fixi\.js|signer\.js|webmcp\.js|sw\.js|icon[^/]*\.(svg|png)|apple-touch-icon\.png|manifest\.webmanifest)$/.test(url.pathname);
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => {
  event.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(key => key !== CACHE).map(key => caches.delete(key)))).then(() => self.clients.claim()));
});
self.addEventListener("fetch", event => {
  const request = event.request;
  if (request.method !== "GET") return;
  const url = new URL(request.url);
  if (url.origin !== location.origin) return;
  const keep = request.mode === "navigate" || shellAsset(url);
  if (!keep) return;
  event.respondWith(fetch(request).then(response => {
    if (response.ok && response.type === "basic") {
      const copy = response.clone();
      caches.open(CACHE).then(cache => cache.put(request, copy)).catch(() => {});
    }
    return response;
  }).catch(async () => (await caches.match(request)) || (request.mode === "navigate" ? caches.match(new Request(url.origin + url.pathname.replace(/\/[^/]*$/, "/"))) : undefined)));
});
