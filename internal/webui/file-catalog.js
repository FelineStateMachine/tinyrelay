// Browser-local names and keys for encrypted files. Nothing in this catalog
// is sent to the relay; the tenant and signed-in key are part of its scope.
(() => {
  "use strict";

  const MAX_ENTRIES = 1000;
  const MAX_NAME = 1024;
  const HASH = /^[0-9a-f]{64}$/;
  const PUBKEY = /^[0-9a-f]{64}$/;

  const actorKey = value => {
    const key = String(value || "").trim().toLowerCase();
    return PUBKEY.test(key) ? key : "";
  };
  const tenantPath = () => {
    try {
      const local = globalThis.tiny?.localPath;
      const value = new URL(typeof local === "function" ? local("/") : "/", location.href);
      return value.pathname.replace(/\/+$/, "") || "/";
    } catch {
      return "/";
    }
  };
  const storageKey = pubkey => `tiny.files.catalog.v1:${location.origin}:${tenantPath()}:${pubkey}`;
  const storage = () => {
    try { return globalThis.localStorage; } catch { return null; }
  };
  const read = pubkey => {
    const key = actorKey(pubkey), store = storage();
    if (!key || !store) return [];
    try {
      const value = JSON.parse(store.getItem(storageKey(key)) || "[]");
      return Array.isArray(value) ? value.filter(entry => validEntry(entry, key)) : [];
    } catch {
      return [];
    }
  };
  const validName = value =>
    typeof value === "string" && value.length > 0 && value.length <= MAX_NAME &&
    value.isWellFormed?.() !== false && !/[\u0000/\\]/.test(value);
  const validPath = value => value === undefined ||
    (typeof value === "string" && value.length <= MAX_NAME && !/[\u0000\\]/.test(value));
  const validLink = (value, hash) => {
    if (typeof value !== "string") return false;
    try {
      const url = new URL(value, location.href);
      const expected = new URL(globalThis.tiny?.localPath?.("/file") || "/file", location.href);
      if (url.origin !== location.origin || url.pathname !== expected.pathname) return false;
      if (url.searchParams.get("hash") !== hash && url.searchParams.get("sha") !== hash) return false;
      const params = new URLSearchParams(url.hash.slice(1));
      return Boolean((params.get("key") && params.get("iv")) ||
        (params.get("enc") === "chk-v1" && params.get("key")) ||
        (params.get("manifest") === hash && params.get("key")));
    } catch {
      return false;
    }
  };
  const validEntry = (entry, pubkey) => Boolean(entry && typeof entry === "object" &&
    actorKey(pubkey) && HASH.test(entry.hash) && validLink(entry.link, entry.hash) &&
    validName(entry.name) && (entry.type === undefined || typeof entry.type === "string") &&
    (entry.size === undefined || (Number.isSafeInteger(entry.size) && entry.size >= 0)) &&
    (entry.folder === undefined || typeof entry.folder === "boolean") && validPath(entry.path));
  const write = (pubkey, entries) => {
    const store = storage();
    if (!store) return false;
    try {
      store.setItem(storageKey(pubkey), JSON.stringify(entries.slice(-MAX_ENTRIES)));
      return true;
    } catch {
      return false;
    }
  };
  const entryFor = (hash, pubkey) => {
    const key = actorKey(pubkey);
    return HASH.test(String(hash || "")) ? read(key).find(entry => entry.hash === hash) || null : null;
  };
  const formatSize = size => {
    if (!Number.isSafeInteger(size) || size < 0) return "";
    if (size < 1024) return `${size} B`;
    const units = ["KiB", "MiB", "GiB"];
    let value = size, unit = "B";
    for (const next of units) {
      value /= 1024;
      unit = next;
      if (value < 1024 || next === units.at(-1)) break;
    }
    return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${unit}`;
  };
  const decorate = pubkey => {
    const section = document.querySelector?.("#file-library[data-pubkey]");
    if (!section) return 0;
    let count = 0;
    for (const row of section.querySelectorAll?.("tr[data-hash]") || []) {
      const entry = entryFor(row.getAttribute("data-hash"), pubkey);
      if (!entry) continue;
      const link = row.querySelector("td:first-child > a") || row.querySelector("a");
      if (link) {
        link.href = entry.link;
        link.setAttribute("fx-ignore", "");
        const name = link.querySelector("span:last-child") || link;
        name.textContent = entry.name;
      }
      const details = row.querySelector("td:nth-child(2)");
      if (details && entry.size !== undefined) {
        const access = /secret link/i.test(details.textContent || "");
        details.textContent = access ? "Secret link" : formatSize(entry.size);
        if (access) {
          const size = document.createElement("small");
          size.textContent = formatSize(entry.size);
          details.append(size);
        }
        if (entry.type) {
          const type = document.createElement("small");
          type.textContent = entry.type;
          details.append(type);
        }
      }
      count++;
    }
    return count;
  };
  const restore = pubkey => {
    const current = new URL(location.href);
    if (!/(?:^|\/)file$/.test(current.pathname)) return null;
    const hash = current.searchParams.get("hash") || current.searchParams.get("sha");
    const fragment = new URLSearchParams(current.hash.slice(1));
    if (!HASH.test(hash || "") || fragment.get("key") || fragment.get("iv")) return null;
    const entry = entryFor(hash, pubkey);
    if (!entry) return null;
    const saved = new URL(entry.link);
    if (saved.searchParams.get("hash") !== hash && saved.searchParams.get("sha") !== hash) return null;
    history.replaceState?.(null, "", saved.pathname + saved.search + saved.hash);
    return entry;
  };
  const save = (value, pubkey) => {
    const key = actorKey(pubkey);
    if (!key || !validEntry(value, key)) return false;
    const entries = read(key).filter(entry => entry.hash !== value.hash);
    entries.push({
      hash: value.hash,
      link: value.link,
      name: value.name,
      ...(value.type === undefined ? {} : {type: value.type}),
      ...(value.size === undefined ? {} : {size: value.size}),
      ...(value.folder === undefined ? {} : {folder: value.folder}),
      ...(value.path === undefined ? {} : {path: value.path}),
      savedAt: Date.now()
    });
    return write(key, entries);
  };

  const api = Object.freeze({save, get: entryFor, restore, decorate});
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.files = {...(globalThis.tiny.files || {}), catalog: api};
  const sync = () => {
    const surface = document.querySelector?.("#file-library[data-pubkey], #file-detail[data-pubkey], #file-view[data-pubkey]");
    const actor = surface?.getAttribute("data-pubkey");
    if (!actor) return;
    restore(actor);
    decorate(actor);
  };
  sync();
  document.addEventListener?.("tiny:navigation", sync);
})();
