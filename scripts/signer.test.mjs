import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const bridge = await readFile(new URL("../internal/webui/bridge.js", import.meta.url), "utf8");
const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");

async function page({signedIn = false, bunkerResult = null, href = "https://tiny.example/r/work/signin", referrer = ""} = {}) {
  const elements = new Map();
  for (const id of ["session-login", "nostrconnect", "nostrconnect-open", "signer-qr", "signer-status", "bunker", "bunker-url", ...(signedIn ? ["session-logout"] : [])]) {
    elements.set(id, {hidden: true, value: "", addEventListener(event, handler) { this[event] = handler; }, setAttribute() {}, removeAttribute() {}});
  }
  const saved = new Map();
  const documentEvents = new Map();
  const requests = [];
  const signer = {bp: {pubkey: "a".repeat(64), relays: ["wss://tiny.example/r/work"]}, connect: async () => {}, getPublicKey: async () => "a".repeat(64), signEvent: async event => ({...event, pubkey: "a".repeat(64), sig: "test"})};
  const sandbox = {
    tiny: {},
    document: {getElementById: id => elements.get(id), querySelector: () => null, referrer, addEventListener(name, handler) {documentEvents.set(name, handler);}, dispatchEvent() {}, documentElement: {dataset: {}}},
    crypto: webcrypto, TextEncoder, URL, URLSearchParams, Uint8Array, ArrayBuffer,
    btoa: value => Buffer.from(value).toString("base64"),
    location: {pathname: new URL(href).pathname, href, origin: new URL(href).origin, assign(url) { sandbox.opened = url; }, replace(url) { sandbox.opened = url; sandbox.replaced = true; }, reload() { sandbox.reloaded = true; }},
    navigator: {clipboard: {writeText: async () => {} }},
    getComputedStyle: () => ({getPropertyValue: () => ""}),
    matchMedia: () => ({matches: false, addEventListener() {}}),
    addEventListener() {},
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
  return {elements, saved, requests, sandbox, documentEvents};
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

test("sign-in returns to the requested page and replaces the login history entry", async () => {
  const target = "/r/work/rooms/hermes/thread/" + "d".repeat(64) + "?view=thread#msg-123";
  const href = "https://tiny.example/r/work/signin?next=" + encodeURIComponent(target.split("#")[0]) + "#return=" + encodeURIComponent(target);
  for (const mode of ["extension", "phone", "bunker"]) {
    const {elements, sandbox} = await page({href, bunkerResult: {pubkey: "a".repeat(64), relays: ["wss://relay.test"]}});
    if (mode === "extension") {
      sandbox.nostr = {signEvent: async event => event};
      await elements.get("session-login").click({currentTarget: elements.get("session-login")});
    } else if (mode === "phone") {
      await elements.get("nostrconnect").click({currentTarget: elements.get("nostrconnect"), preventDefault() {}});
    } else {
      elements.get("bunker-url").value = "bunker://test";
      await elements.get("bunker").submit({currentTarget: elements.get("bunker"), preventDefault() {}});
    }
    assert.equal(sandbox.opened, target, mode);
    assert.equal(sandbox.replaced, true, mode);
  }
});

test("embedded login keeps the protected page, query and fragment", async () => {
  const target = "/r/work/manage/connect?view=client#setup";
  const {elements, sandbox} = await page({href: "https://tiny.example" + target});
  sandbox.nostr = {signEvent: async event => event};
  await elements.get("session-login").click({currentTarget: elements.get("session-login")});
  assert.equal(sandbox.opened, target);
});

test("return targets reject external addresses, tenant changes and sign-in loops", async () => {
  for (const target of ["https://evil.test", "//evil.test", "/r/elsewhere/rooms", "/r/work/signin", "/r/work/signin?next=/r/work/rooms", "/r/work/../../r/other/", "/\\evil.test", "/r/work/rooms\n"]) {
    const {elements, sandbox} = await page({href: "https://tiny.example/r/work/signin?next=" + encodeURIComponent(target)});
    sandbox.nostr = {signEvent: async event => event};
    await elements.get("session-login").click({currentTarget: elements.get("session-login")});
    assert.equal(sandbox.opened, "/r/work/", target);
  }
});

test("sign-in links carry private fragments only in the browser fragment", async () => {
  const target = "/r/work/file?hash=abc#key=private-key&name=notes.txt";
  const {sandbox, documentEvents} = await page({href: "https://tiny.example" + target});
  const attrs = new Map([["fx-action", "/r/work/signin"]]);
  const link = {href: "https://tiny.example/r/work/signin", hasAttribute: name => attrs.has(name), setAttribute: (name,value) => attrs.set(name,value)};
  documentEvents.get("click")({target: {closest: () => link}});
  const login = new URL(link.href, sandbox.location.href);
  assert.equal(login.searchParams.get("next"), "/r/work/file?hash=abc");
  assert.equal(login.search.includes("private-key"), false);
  assert.equal(new URLSearchParams(login.hash.slice(1)).get("return"), target);
  assert.equal(attrs.get("fx-action"), link.href);
});
