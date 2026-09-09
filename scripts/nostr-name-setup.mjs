// Shared state for the vendored <nostr-name> element. Names resolve from this
// relay only: tiny is a personal relay, so the profiles that matter live here,
// and asking public indexers would leak who is being looked at. The element
// reads window.nostrSharedMetadataLoader when its module evaluates, so this
// file must be imported first.
import { SimplePool } from "@nostr/tools/pool";
import { bareNostrUser, nostrUserFromEvent } from "@nostr/gadgets/metadata";

const pool = window.nostrSharedPool || new SimplePool();
window.nostrSharedPool = pool;

// The relay listens at the tenant root: wss://host for the default tenant,
// wss://host/r/<name> for the others.
const relayURL = () => {
  const root = (location.pathname.match(/^\/r\/[^/]+/) || [""])[0];
  return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + root;
};

const CACHE = "tiny:names:v1";
const FRESH = 6 * 60 * 60; // seconds a profile is trusted before a refetch
const MISSING = 10 * 60; // seconds before asking again about a key with no profile
const BATCH = 50; // milliseconds to gather keys before one request
const LIMIT = 100; // authors per request

const readCache = () => {
  try { return JSON.parse(localStorage.getItem(CACHE) || "{}") || {}; } catch { return {}; }
};
const writeCache = cache => {
  try { localStorage.setItem(CACHE, JSON.stringify(cache)); } catch {}
};

const users = new Map(); // pubkey -> NostrUser, from cache or the relay
const pending = new Map(); // pubkey -> [resolve]
let timer = null;

const userFor = (pubkey, entry) => entry?.event ? nostrUserFromEvent(entry.event) : bareNostrUser(pubkey);

const flush = async () => {
  timer = null;
  const keys = [...pending.keys()].slice(0, LIMIT);
  if (!keys.length) return;
  const cache = readCache();
  const now = Math.floor(Date.now() / 1000);
  let events = [];
  try {
    events = await pool.querySync([relayURL()], {kinds: [0], authors: keys});
  } catch {}
  const newest = new Map();
  for (const event of events) {
    if (!newest.has(event.pubkey) || newest.get(event.pubkey).created_at < event.created_at) newest.set(event.pubkey, event);
  }
  for (const key of keys) {
    const event = newest.get(key) || cache[key]?.event || null;
    cache[key] = {event, at: now};
    const user = userFor(key, cache[key]);
    users.set(key, user);
    for (const resolve of pending.get(key) || []) resolve(user);
    pending.delete(key);
  }
  writeCache(cache);
  if (pending.size) timer = setTimeout(flush, BATCH);
};

// load answers like @nostr/gadgets' loadNostrUser: a NostrUser from cache,
// refreshed from this relay when stale, with a placeholder while unknown.
const load = ({pubkey}) => {
  const key = String(pubkey || "").toLowerCase();
  if (!/^[0-9a-f]{64}$/.test(key)) return Promise.resolve(bareNostrUser(key || "0".repeat(64)));
  const cached = readCache()[key];
  const now = Math.floor(Date.now() / 1000);
  if (cached && now - cached.at < (cached.event ? FRESH : MISSING)) return Promise.resolve(users.get(key) || userFor(key, cached));
  return new Promise(resolve => {
    pending.set(key, [...(pending.get(key) || []), resolve]);
    if (!timer) timer = setTimeout(flush, BATCH);
  });
};

window.nostrSharedMetadataLoader = window.nostrSharedMetadataLoader || (request => load(typeof request === "string" ? {pubkey: request} : request));
window.tinyNames = Object.freeze({load, relayURL, cacheKey: CACHE});
