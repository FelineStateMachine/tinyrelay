import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const bridge = await readFile(new URL("../internal/webui/bridge.js", import.meta.url), "utf8");
const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");

class Element {
  constructor(tag = "div") { this.tagName = tag.toUpperCase(); this.children = []; this.attributes = new Map(); this.hidden = false; }
  setAttribute(k, v) { this.attributes.set(k, String(v)); }
  getAttribute(k) { return this.attributes.get(k) ?? null; }
  hasAttribute(k) { return this.attributes.has(k); }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  addEventListener(name, fn) { this.events ||= {}; this.events[name] = fn; }
  dispatch(name, event = {}) { return this.events?.[name]?.({...event, currentTarget: this}); }
  matches() { return false; }
  querySelector() { return null; }
  get textContent() { return this.children.map(child => child.textContent ?? "").join(""); }
  set textContent(value) { this.children = [{textContent: String(value)}]; }
}

async function page({actor = "a".repeat(64), native = null, stored = false, signerFactory = () => null, now = Date.now()} = {}) {
  const nodes = new Map();
  for (const id of ["session-status", "session-login", "session-logout", "signer-status", "bunker", "bunker-url"]) nodes.set(id, new Element(id));
  const connection = new Element("signer-connection"); connection.setAttribute("actor", actor); nodes.set("signer-connection", connection);
  const docEvents = new Map(), winEvents = new Map(), session = new Map(), local = new Map(), requests = [];
  const on = (map, name, fn) => map.set(name, [...(map.get(name) || []), fn]);
  const fire = (map, name, event = {}) => Promise.all((map.get(name) || []).map(fn => fn(event)));
  const nativeObject = native;
  const signer = nativeObject;
  let clock = now;
  const sandbox = {
    tiny: {}, document: {
      visibilityState: "visible", referrer: "", documentElement: {dataset: {}},
      getElementById: id => nodes.get(id), querySelector: selector => selector === "signer-connection" ? nodes.get("signer-connection") : null,
      querySelectorAll: () => [], createElement: tag => new Element(tag),
      addEventListener: on.bind(null, docEvents), dispatchEvent: event => fire(docEvents, event.type, event)
    },
    crypto: webcrypto, TextEncoder, URL, URLSearchParams, Uint8Array, ArrayBuffer, Date: class extends Date { static now() { return clock; } }, setTimeout, clearTimeout,
    btoa: value => Buffer.from(value).toString("base64"),
    location: {pathname: "/r/work/rooms/general", href: "https://tiny.example/r/work/rooms/general?x=1#part", origin: "https://tiny.example", assign(url) { this.assigned = url; }, replace(url) { this.replaced = url; }, reload() { this.reloaded = true; }},
    navigator: {clipboard: {writeText: async () => {}}}, getComputedStyle: () => ({getPropertyValue: () => ""}),
    matchMedia: () => ({matches: false, addEventListener() {}}), addEventListener: on.bind(null, winEvents),
    localStorage: {getItem: k => local.get(k) ?? null, setItem: (k, v) => local.set(k, String(v)), removeItem: k => local.delete(k)},
    sessionStorage: {getItem: k => session.get(k) ?? null, setItem: (k, v) => session.set(k, String(v)), removeItem: k => session.delete(k)},
    NostrSigner: {
      hexToBytes: () => new Uint8Array(32), bytesToHex: () => "key", generateSecretKey: () => new Uint8Array(32), getPublicKey: () => "b".repeat(64),
      parseBunkerInput: async () => ({pubkey: "c".repeat(64), relays: ["wss://relay.test"]}), BunkerSigner: {fromBunker: signerFactory}
    },
    fetch: async (url, options) => { requests.push({url, options}); return new Response("{}"); }
  };
  sandbox.window = sandbox;
  if (signer) sandbox.nostr = signer;
  vm.runInNewContext(shared + bridge, sandbox);
  await new Promise(resolve => setTimeout(resolve, 0));
  if (stored) { session.set("tiny.bunker/r/work.uri", "bunker://saved"); session.set("tiny.bunker/r/work.sk", "key"); session.set("tiny.bunker/r/work.identity", "c".repeat(64)); session.set("tiny.bunker/r/work.remote", JSON.stringify({pubkey: "c".repeat(64), relays: ["wss://relay.test"]})); }
  return {sandbox, nodes, docEvents, winEvents, session, local, requests, setClock: value => { clock = value; }, fire: (map, name, event) => fire(map, name, event)};
}

const saved = (session, actor = "c".repeat(64)) => {
  session.set("tiny.bunker/r/work.uri", "bunker://saved"); session.set("tiny.bunker/r/work.sk", "key"); session.set("tiny.bunker/r/work.identity", actor); session.set("tiny.bunker/r/work.remote", JSON.stringify({pubkey: actor, relays: ["wss://relay.test"]}));
};

test("recovery checks a late native signer without replacing window.nostr", async () => {
  const native = {getPublicKey: async () => "a".repeat(64), signEvent: async event => event};
  const {sandbox, winEvents} = await page();
  sandbox.nostr = native;
  await sandbox.tiny.reconnectSigner();
  assert.equal(sandbox.nostr, native);
  assert.equal(sandbox.tiny.signerState(), "ready");
  assert.equal(sandbox.location.replaced, undefined);
  assert.ok(winEvents.get("pageshow"));
});

test("missing remote credentials gives a connect path and navigation restores it", async () => {
  const {sandbox, nodes, docEvents} = await page();
  await sandbox.tiny.reconnectSigner({force: true}).catch(() => {});
  assert.equal(nodes.get("signer-connection").hidden, false);
  assert.equal(nodes.get("signer-connection").children[1].textContent, "Connect signer");
  nodes.get("signer-connection").children[1].dispatch("click");
  assert.match(sandbox.location.assigned, /signin\?connect=1&next=/);
  const replacement = new Element("signer-connection"); replacement.setAttribute("actor", "a".repeat(64)); nodes.set("signer-connection", replacement);
  await Promise.all((docEvents.get("tiny:navigation") || []).map(fn => fn()));
  assert.equal(replacement.hidden, false);
});

test("remote ping keeps the same signer and failed replacement closes both pools", async () => {
  let oldClose = 0, oldDestroy = 0, candidateClose = 0, candidateDestroy = 0, ping = 0;
  const pool = () => ({destroy: () => { oldDestroy++; }});
  const old = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, pool: pool(), connect: async () => {}, ping: async () => { ping++; }, close: () => { oldClose++; }, getPublicKey: async () => "c".repeat(64), signEvent: async e => e};
  const candidate = {bp: old.bp, pool: {destroy: () => { candidateDestroy++; }}, connect: async () => {}, close: () => { candidateClose++; }, getPublicKey: async () => "c".repeat(64)};
  const failedCandidate = {...candidate, connect: async () => { throw Error("offline"); }};
  const {sandbox, session, setClock} = await page({actor: "c".repeat(64), signerFactory: () => old, now: 1000});
  saved(session);
  await sandbox.tiny.reconnectSigner({force: true});
  const first = sandbox.tinySigner;
  setClock(20000);
  await sandbox.tiny.reconnectSigner();
  assert.equal(sandbox.tinySigner, first); assert.equal(ping, 1);
  // Force a replacement after ping fails.
  old.ping = async () => { throw Error("offline"); };
  sandbox.NostrSigner.BunkerSigner.fromBunker = () => failedCandidate;
  await assert.rejects(sandbox.tiny.reconnectSigner({force: true}), /offline/);
  assert.equal(sandbox.tiny.signerState(), "lost");
  assert.ok(oldClose > 0 && oldDestroy > 0 && candidateClose > 0 && candidateDestroy > 0);
  assert.equal(session.get("tiny.bunker/r/work.sk"), "key");
});

test("signer denial stays ready while transport failure reports lost", async () => {
  let reject = false;
  const remote = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, connect: async () => {}, getPublicKey: async () => "c".repeat(64), signEvent: async () => { throw Error(reject ? "user rejected" : "connection closed"); }};
  const {sandbox, session} = await page({actor: "c".repeat(64), signerFactory: () => remote}); saved(session);
  await sandbox.tiny.reconnectSigner({force: true});
  await assert.rejects(sandbox.tinySigner.signEvent({}), /connection closed/); assert.equal(sandbox.tiny.signerState(), "lost");
  reject = true;
  await sandbox.tiny.reconnectSigner({force: true});
  await assert.rejects(sandbox.tinySigner.signEvent({}), /user rejected/); assert.equal(sandbox.tiny.signerState(), "ready");
});

test("a brief background suspension forces a fresh remote health check", async () => {
  let pings = 0;
  const remote = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, connect: async () => {}, ping: async () => { pings++; }, getPublicKey: async () => "c".repeat(64)};
  const {sandbox, session, fire, docEvents, setClock} = await page({actor: "c".repeat(64), signerFactory: () => remote, now: 1000}); saved(session);
  await sandbox.tiny.reconnectSigner({force: true});
  setClock(2000);
  sandbox.document.visibilityState = "hidden";
  await fire(docEvents, "visibilitychange");
  sandbox.document.visibilityState = "visible";
  await fire(docEvents, "visibilitychange");
  await sandbox.tiny.reconnectSigner();
  assert.equal(sandbox.tiny.signerState(), "ready");
  assert.equal(pings, 1);
});

test("successful remote replacement closes the old signer and changes the facade", async () => {
  let oldClose = 0, oldDestroy = 0;
  const old = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, connect: async () => {}, pool: {destroy: () => { oldDestroy++; }}, close: () => { oldClose++; }, getPublicKey: async () => "c".repeat(64), ping: async () => { throw Error("offline"); }};
  const next = {bp: old.bp, connect: async () => {}, pool: {destroy() {}}, close() {}, getPublicKey: async () => "c".repeat(64)};
  const {sandbox, session} = await page({actor: "c".repeat(64), signerFactory: () => old}); saved(session);
  await sandbox.tiny.reconnectSigner({force: true}); const previous = sandbox.tinySigner;
  sandbox.NostrSigner.BunkerSigner.fromBunker = () => next;
  await sandbox.tiny.reconnectSigner({force: true});
  assert.notEqual(sandbox.tinySigner, previous); assert.equal(sandbox.tiny.signerState(), "ready");
  assert.ok(oldClose > 0 && oldDestroy > 0);
});

test("an old pending signer operation cannot clear a replacement", async () => {
  let release;
  const old = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, connect: async () => {}, close() {}, pool: {destroy() {}}, getPublicKey: async () => "c".repeat(64), signEvent: async () => new Promise(resolve => { release = resolve; }), ping: async () => { throw Error("offline"); }};
  const next = {bp: old.bp, connect: async () => {}, close() {}, pool: {destroy() {}}, getPublicKey: async () => "c".repeat(64)};
  const {sandbox, session} = await page({actor: "c".repeat(64), signerFactory: () => old}); saved(session);
  await sandbox.tiny.reconnectSigner({force: true});
  const pending = sandbox.tinySigner.signEvent({});
  sandbox.NostrSigner.BunkerSigner.fromBunker = () => next;
  await sandbox.tiny.reconnectSigner({force: true});
  await assert.rejects(pending, /connection changed/);
  release({});
  assert.equal(sandbox.tiny.signerState(), "ready");
});

test("native signer identity mismatch reports loss", async () => {
  const native = {getPublicKey: async () => "b".repeat(64), signEvent: async event => event};
  const {sandbox} = await page({actor: "a".repeat(64), native});
  sandbox.nostr = native;
  await assert.rejects(sandbox.tiny.reconnectSigner({force: true}), /signed-in account/);
  assert.equal(sandbox.tiny.signerState(), "lost"); assert.equal(sandbox.nostr, native);
});

test("NIP-44 transport failure reports loss", async () => {
  const remote = {bp: {pubkey: "c".repeat(64), relays: ["wss://relay.test"]}, connect: async () => {}, getPublicKey: async () => "c".repeat(64), nip44Decrypt: async () => { throw Error("connection closed"); }};
  const {sandbox, session} = await page({actor: "c".repeat(64), signerFactory: () => remote}); saved(session);
  await sandbox.tiny.reconnectSigner({force: true});
  await assert.rejects(sandbox.tinySigner.nip44Decrypt("peer", "ciphertext"), /connection closed/);
  assert.equal(sandbox.tiny.signerState(), "lost");
});
