import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const bridge = await readFile(new URL("../internal/webui/bridge.js", import.meta.url), "utf8");
const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");

async function page({signedIn = false, bunkerResult = null} = {}) {
  const elements = new Map();
  for (const id of ["session-login", "nostrconnect", "nostrconnect-open", "signer-qr", "signer-status", "bunker", "bunker-url", ...(signedIn ? ["session-logout"] : [])]) {
    elements.set(id, {hidden: true, value: "", addEventListener(event, handler) { this[event] = handler; }, setAttribute() {}, removeAttribute() {}});
  }
  const saved = new Map();
  const requests = [];
  const signer = {bp: {pubkey: "a".repeat(64), relays: ["wss://tiny.example/r/work"]}, getPublicKey: async () => "a".repeat(64), signEvent: async event => ({...event, pubkey: "a".repeat(64), sig: "test"})};
  const sandbox = {
    tiny: {},
    document: {getElementById: id => elements.get(id), querySelector: () => null, addEventListener() {}, dispatchEvent() {}, documentElement: {dataset: {}}},
    crypto: webcrypto, TextEncoder, URL, Uint8Array, ArrayBuffer,
    btoa: value => Buffer.from(value).toString("base64"),
    location: {pathname: "/r/work/signin", href: "https://tiny.example/r/work/signin", origin: "https://tiny.example", assign(url) { sandbox.opened = url; }, reload() { sandbox.reloaded = true; }},
    navigator: {clipboard: {writeText: async () => {} }},
    getComputedStyle: () => ({getPropertyValue: () => ""}),
    matchMedia: () => ({matches: false, addEventListener() {}}),
    localStorage: {getItem: () => null, setItem() {}, removeItem() {}},
    sessionStorage: {getItem: key => saved.get(key), setItem: (key, value) => saved.set(key, value), removeItem: key => saved.delete(key)},
    NostrSigner: {
      generateSecretKey: () => new Uint8Array(32), getPublicKey: () => "b".repeat(64), createNostrConnectURI: () => "nostrconnect://test",
      bytesToHex: () => "test-key", hexToBytes: () => new Uint8Array(32), parseBunkerInput: async () => bunkerResult,
      BunkerSigner: {fromURI: async () => signer, fromBunker: () => signer}
    },
    fetch: async (url, options) => { requests.push({url, options}); return new Response("{}"); }
  };
  sandbox.window = sandbox;
  vm.runInNewContext(shared + bridge, sandbox);
  await new Promise(resolve => setTimeout(resolve, 0));
  return {elements, saved, requests, sandbox};
}

test("approving a phone signer signs in and opens the tenant homepage", async () => {
  const {elements, saved, requests, sandbox} = await page();
  await elements.get("nostrconnect").click({currentTarget: elements.get("nostrconnect"), preventDefault() {}});
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "/r/work/session");
  assert.ok(requests[0].options.headers.authorization.startsWith("Nostr "));
  assert.equal(sandbox.opened, "/r/work/");
  assert.equal(saved.get("tiny.bunker/r/work.identity"), "a".repeat(64));
});

test("invalid bunker input reports a useful error without connecting", async () => {
  const {elements, requests} = await page({bunkerResult: null});
  elements.get("bunker-url").value = "not-a-bunker";
  await elements.get("bunker").submit({currentTarget: elements.get("bunker"), preventDefault() {}});
  assert.equal(requests.length, 0);
  assert.equal(elements.get("signer-status").textContent, "Remote signer error: Enter a valid bunker URL with at least one relay.");
});

test("NIP-07 signer can sign in directly", async () => {
  const {elements, requests, sandbox} = await page();
  sandbox.nostr = {signEvent: async event => ({...event, pubkey: "c".repeat(64), sig: "extension"})};
  await elements.get("session-login").click({currentTarget: elements.get("session-login")});
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "/r/work/session");
  assert.match(elements.get("signer-status").textContent, /Signing in|Signed in/);
  assert.equal(sandbox.opened, "/r/work/");
});

test("sign out revokes the session without requiring a connected phone", async () => {
  const {elements, requests, sandbox} = await page({signedIn: true});
  await elements.get("session-logout").click({currentTarget: elements.get("session-logout")});
  assert.equal(requests[0].url, "/r/work/session/logout");
  assert.equal(requests[0].options.credentials, "same-origin");
  assert.equal(sandbox.reloaded, true);
});
