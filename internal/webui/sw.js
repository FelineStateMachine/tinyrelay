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
  const url = new URL(request.url);
  if (url.origin !== location.origin) return;
  if (request.method === "POST" && /\/share$/.test(url.pathname)) {
    event.respondWith(receiveShare(request, url));
    return;
  }
  if (request.method !== "GET") return;
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

// Shares from other apps arrive as a form post. The files are parked in a
// cache under a fresh id and the Files page picks them up from its fragment,
// so nothing is uploaded until the person presses Upload there.
const receiveShare = async (request, url) => {
  const form = await request.formData();
  const id = crypto.randomUUID();
  const cache = await caches.open("tiny-share");
  const files = form.getAll("files").filter(item => typeof item === "object" && "arrayBuffer" in item);
  await Promise.all(files.map((file, index) => cache.put(new Request(url.origin + "/share/" + id + "/" + index), new Response(file, {headers: {"content-type": file.type || "application/octet-stream", "x-name": encodeURIComponent(file.name || "shared")}}))));
  await cache.put(new Request(url.origin + "/share/" + id + "/meta"), Response.json({title: form.get("title") || "", text: form.get("text") || "", url: form.get("url") || "", count: files.length}));
  return Response.redirect(url.pathname.replace(/\/share$/, "/files") + "#share=" + id, 303);
};

// Notifications are opt-in per device; a push only arrives after the person
// enabled them on this browser. The payload is the relay's short summary.
self.addEventListener("push", event => {
  let data = {};
  try { data = event.data?.json() || {}; } catch { data = {body: event.data?.text() || ""}; }
  const base = new URL(self.registration.scope);
  event.waitUntil(self.registration.showNotification(data.title || "tiny", {
    body: data.body || "", tag: data.tag || "tiny", icon: new URL("icon-192.png", base).href, badge: new URL("badge-96.png", base).href, data: {url: data.url || base.href}
  }));
});
self.addEventListener("notificationclick", event => {
  event.notification.close();
  const target = event.notification.data?.url || self.registration.scope;
  event.waitUntil(self.clients.matchAll({type: "window", includeUncontrolled: true}).then(async list => {
    const open = list.find(client => client.url.startsWith(self.registration.scope));
    if (open) { const focused = await open.navigate(target); return focused?.focus(); }
    return self.clients.openWindow(target);
  }));
});
