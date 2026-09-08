// Service worker: makes the relay installable and keeps navigation working
// when the network fails by serving the last shell seen. Relay data is never
// cached; every request goes to the network first.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));
self.addEventListener("fetch", event => {
  if (event.request.method !== "GET") return;
  event.respondWith(fetch(event.request).catch(() => caches.match(event.request)));
});
