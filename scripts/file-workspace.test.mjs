import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { webcrypto } from "node:crypto";

class Node {
  constructor(tag = "") { this.tagName = tag.toUpperCase(); this.children = []; this.attributes = {}; this.listeners = {}; this.dataset = {}; this.textContent = ""; }
  append(...nodes) { nodes.forEach(node => { if (node) { this.children.push(node); node.parentNode = this; } }); }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  click() {}
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  hasAttribute(name) { return name in this.attributes; }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  set innerHTML(value) { this.children = []; parse(value, this); }
  get childNodes() { return this.children; }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  querySelectorAll(selector) {
    const result = [];
    const match = node => {
      if (selector === "output") return node.tagName === "OUTPUT";
      if (selector === "form:not([data-share])") return node.tagName === "FORM" && !node.hasAttribute("data-share");
      if (selector === "input[type=file]") return node.tagName === "INPUT" && node.getAttribute("type") === "file";
      const attr = selector.match(/^([^[]+)?\[([^=\]]+)(?:=\"([^\"]+)\")?\]$/);
      if (attr) return (!attr[1] || node.tagName === attr[1].toUpperCase()) && node.hasAttribute(attr[2]) && (attr[3] === undefined || node.getAttribute(attr[2]) === attr[3]);
      return node.tagName === selector.toUpperCase();
    };
    const walk = node => { node.children.forEach(child => { if (match(child)) result.push(child); walk(child); }); };
    walk(this); return result;
  }
}
class Form extends Node {
  constructor() { super("form"); this.elements = { namedItem: name => this.querySelectorAll("input").concat(this.querySelectorAll("select")).find(node => node.getAttribute("name") === name) }; }
}
const parse = (html, parent) => {
  const stack = [parent];
  for (const token of html.match(/<[^>]+>|[^<]+/g) || []) {
    if (!token.startsWith("<")) continue;
    if (/^<\/(option|input|br|meta|img)>/.test(token)) continue;
    if (/^<\//.test(token)) { stack.pop(); continue; }
    const match = token.match(/^<([\w-]+)([^>]*)>/); if (!match) continue;
    const node = match[1] === "form" ? new Form() : new Node(match[1]);
    for (const [, name, quote, value] of match[2].matchAll(/([\w-]+)(?:=(['"])(.*?)\2)?/g)) node.setAttribute(name, value ?? "");
    if (node.tagName === "INPUT" || node.tagName === "SELECT") node.value = node.getAttribute("value") || (node.tagName === "SELECT" ? "random" : "");
    stack.at(-1).append(node);
    if (!/^(input|br|meta|option|img)$/.test(match[1])) stack.push(node);
  }
};
const load = (workspace = false) => {
  const document = new Node("document"); document.documentElement = new Node("html"); document.body = new Node("body"); document.createElement = name => name === "form" ? new Form() : new Node(name); document.addEventListener = () => {}; document.dispatchEvent = () => {};
  const customElements = {registry: {}, define(name, ctor) { this.registry[name] = ctor; }};
  const window = {document, customElements, tiny: {}, location: {href: "https://relay.test/files", origin: "https://relay.test", pathname: "/files", hash: ""}, addEventListener: () => {}, matchMedia: () => ({matches: false, addEventListener: () => {}})};
  class HTMLElement extends Node {}
  Object.assign(globalThis, {window, document, customElements, HTMLElement, location: window.location, Blob, URL, CustomEvent: class {}});
  globalThis.tiny = window.tiny;
  Object.defineProperty(globalThis, "navigator", {configurable: true, value: {clipboard: {writeText: async () => { throw Error("clipboard unavailable"); }}}});
  globalThis.btoa = value => Buffer.from(value, "binary").toString("base64"); globalThis.atob = value => Buffer.from(value, "base64").toString("binary");
  if (workspace) { vm.runInThisContext(fs.readFileSync("internal/webui/blossom-manifests.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/blossom-encryption.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/blossom-upload.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/file-workspace.js", "utf8")); } else { delete globalThis.TinyBlossomUpload; }
  vm.runInThisContext(fs.readFileSync("internal/webui/components.js", "utf8"));
  return {document, window, FileTools: customElements.registry["file-tools"], FileMirror: customElements.registry["file-mirror"]};
};

const workspaceServer = t => {
  const loaded = load(true);
  const {window} = loaded;
  const blobs = new Map(), pending = new Map(), requests = [];
  const originalFetch = globalThis.fetch;
  window.tiny.localPath = path => "/r/test" + path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.authorization = async (url, method, body) => {
    assert.equal(new URL(url).hash, "");
    return "Nostr " + await window.tiny.sha256hex(body);
  };
  globalThis.fetch = async (target, init = {}) => {
    const url = new URL(target, window.location.href);
    requests.push({url, init});
    assert.equal(url.hash, "");
    assert.equal(url.searchParams.has("key"), false);
    if (init.method === "OPTIONS") return new Response(null, {status: 204, headers: {Allow: "PATCH"}});
    if (init.method === "PATCH") {
      const hash = url.pathname.split("/").at(-1);
      assert.match(url.pathname, /^\/r\/test\/[0-9a-f]{64}$/);
      const length = Number(init.headers["upload-length"]), offset = Number(init.headers["upload-offset"]);
      if (!pending.has(hash)) pending.set(hash, new Uint8Array(length));
      pending.get(hash).set(init.body, offset);
      if (offset + init.body.length < length) return new Response(null, {status: 204});
      const bytes = pending.get(hash);
      assert.equal(hash, await window.tiny.sha256hex(bytes));
      blobs.set(hash, bytes); pending.delete(hash);
      return Response.json({sha256: hash, size: bytes.length}, {status: 201});
    }
    const hash = url.searchParams.get("hash");
    return blobs.has(hash) ? new Response(blobs.get(hash)) : new Response(null, {status: 404});
  };
  t.after(() => { globalThis.fetch = originalFetch; });
  const open = async url => {
    const value = new URL(url);
    Object.assign(window.location, {href: value.href, pathname: value.pathname, hash: value.hash});
    const root = new customElements.registry["file-workspace-root"]();
    await root.load();
    t.after(() => root.disconnectedCallback());
    return root;
  };
  const store = async files => {
    const workspace = new customElements.registry["file-workspace"]();
    workspace.connectedCallback();
    const form = workspace.querySelector("[data-folder]");
    form.elements.namedItem("folder").files = files;
    return workspace.run(form);
  };
  return {...loaded, blobs, requests, store, open, originalFetch};
};
const folderFile = (contents, path) => {
  const file = new File([contents], path.split("/").at(-1), {type: "text/plain"});
  Object.defineProperty(file, "webkitRelativePath", {value: path});
  return file;
};

test("nested folder components upload and decrypt large files without exposing keys", async t => {
  const server = workspaceServer(t);
  const large = new Uint8Array(2 * 1024 * 1024 + 19); large[2] = 91; large[large.length - 1] = 73;
  const url = await server.store([
    folderFile(large, "root/__proto__/f"), folderFile("héllo", "root/é.txt"),
    folderFile("emoji", "root/😀.txt"), folderFile("constructor", "root/constructor")
  ]);
  const root = await server.open(url);
  assert.deepEqual(root.entries.map(entry => entry.n), ["__proto__", "constructor", "é.txt", "😀.txt"]);
  const child = await server.open(root.querySelector("a").href);
  assert.equal(child.entries[0].n, "f");
  assert.deepEqual(await child.download(child.entries[0]), large);
  const key = new URLSearchParams(new URL(url).hash.slice(1)).get("key");
  for (const {url: target, init} of server.requests) {
    assert.equal(target.href.includes(key), false);
    if (init.body) assert.equal(new TextDecoder().decode(init.body).includes("héllo"), false);
  }
  assert.ok(server.blobs.size >= 8);
});

test("directory fanout stays invisible while 175 entries remain browsable", async t => {
  const server = workspaceServer(t);
  const files = Array.from({length: 175}, (_, index) => folderFile("content " + index, "wide/file-" + String(index).padStart(3, "0")));
  const root = await server.open(await server.store(files));
  assert.equal(root.entries.length, 175);
  assert.equal(root.entries[174].n, "file-174");
  assert.equal(new TextDecoder().decode(await root.download(root.entries[174])), "content 174");
});

test("folder upload rejects unexpected server hashes", async t => {
  const server = workspaceServer(t);
  globalThis.fetch = async (_url, init = {}) => init.method === "OPTIONS" ? new Response(null, {status:204, headers:{Allow:"PATCH"}}) : Response.json({sha256:"0".repeat(64), size: init.body.length}, {status:201});
  await assert.rejects(server.store([folderFile("private", "root/private.txt")]), /descriptor/);
});

test("encrypted upload keeps ciphertext and link when clipboard fails", async () => {
  const {document, window, FileTools} = load(); const requests = [];
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.signedFetch = async (path, method, body) => { const bytes = new Uint8Array(body); requests.push(bytes); return {json: async () => ({sha256: await window.tiny.sha256hex(bytes)})}; };
  const tools = new FileTools(); document.append(tools); tools.connectedCallback();
  const form = tools.querySelector("form:not([data-share])");
  form.querySelector("input[type=file]").files = [new File(["secret plaintext"], "secret.txt", {type: "text/plain"})];
  await tools.encryptUpload(form);
  assert.equal(requests.length, 1); assert.notEqual(new TextDecoder().decode(requests[0]), "secret plaintext");
  assert.match(tools.querySelector("[data-share-url]").value, /#key=.*&iv=/); assert.match(tools.output.textContent, /Copy the share link above/);
  assert.equal(tools.shareLink(), tools.querySelector("[data-share-url]").value);
});

test("BUD15 key is rejected before mirror request", async () => {
  const {document, window, FileMirror} = load(); const mirror = new FileMirror(); document.append(mirror); mirror.connectedCallback();
  mirror.form.querySelector("input").value = "blossom:" + "a".repeat(64) + "?enc=chk-v1&k=" + "1".repeat(64);
  let sent = false; window.tiny.signedFetch = async () => { sent = true; };
  await assert.rejects(() => mirror.submit(mirror.form), /keep k client side/);
  assert.equal(sent, false);
});

test("sharing a newly uploaded file uses its current key, hash and ciphertext size", async () => {
  const {document, window, FileTools} = load();
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.localPath = path => path;
  let uploaded;
  window.tiny.signedFetch = async (_path, _method, body) => { uploaded = new Uint8Array(body); return {json: async () => ({sha256: await window.tiny.sha256hex(body)})}; };
  const element = new FileTools(); document.append(element); element.connectedCallback();
  const form = element.querySelector("form:not([data-share])");
  form.querySelector("input[type=file]").files = [new File(["new secret"], "private.txt", {type: "text/plain"})];
  await element.encryptUpload(form);
  const shareForm = element.querySelector("[data-share]");
  shareForm.querySelector("input").value = "b".repeat(64);
  globalThis.nostr = {signEvent: async value => value};
  let input;
  globalThis.TinyFileMessages = {share: async (value, options) => { input = value; assert.equal(options.signer, globalThis.nostr); return {}; }};
  await element.share(shareForm);
  assert.ok(input, element.output.textContent);
  assert.equal(input.ciphertextHash, await window.tiny.sha256hex(uploaded));
  assert.equal(input.size, uploaded.length);
  assert.equal(input.plaintextHash, await window.tiny.sha256hex(new TextEncoder().encode("new secret")));
  assert.match(input.key, /^[0-9a-f]{64}$/);
  assert.match(input.nonce, /^[0-9a-f]{24}$/);
  const key = await webcrypto.subtle.importKey("raw", Buffer.from(input.key, "hex"), "AES-GCM", false, ["decrypt"]);
  const plain = await webcrypto.subtle.decrypt({name: "AES-GCM", iv: Buffer.from(input.nonce, "hex")}, key, uploaded);
  assert.equal(new TextDecoder().decode(plain), "new secret");
  assert.equal(new URL(input.fileURL).hash, "");
});

test("HTTP mirror input never submits its secret fragment", async () => {
  const {document, window, FileMirror} = load();
  const mirror = new FileMirror(); document.append(mirror); mirror.connectedCallback();
  mirror.form.querySelector("input").value = "https://files.example/blob#key=secret";
  let body;
  window.tiny.signedFetch = async (_path, _method, value) => { body = value; return {json: async () => ({sha256: "a".repeat(64)})}; };
  await mirror.submit(mirror.form);
  assert.equal(JSON.parse(body).url, "https://files.example/blob");
});

test("local downloads verify integrity, decrypt and release their temporary URL", async () => {
  const {document, window, FileTools} = load();
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.localPath = path => path;
  let uploaded;
  window.tiny.signedFetch = async (_path, _method, body) => { uploaded = new Uint8Array(body); return {json: async () => ({sha256: await window.tiny.sha256hex(body)})}; };
  const element = new FileTools(); document.append(element); element.connectedCallback();
  const form = element.querySelector("form:not([data-share])");
  form.querySelector("input[type=file]").files = [new File(["round trip"], "round.txt", {type: "text/plain"})];
  await element.encryptUpload(form);
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(uploaded);
    await element.decryptFromFragment();
    assert.equal(element.output.textContent, "Decrypted locally.");
    assert.equal(await (await originalFetch(element.downloadURL)).text(), "round trip");
    const temporaryURL = element.downloadURL;
    element.disconnectedCallback();
    await assert.rejects(originalFetch(temporaryURL));
    uploaded[0] ^= 1;
    await element.decryptFromFragment();
    assert.match(element.output.textContent, /ciphertext hash mismatch/);
  } finally { globalThis.fetch = originalFetch; }
});

test("retry keeps the original ciphertext and key after interrupted encrypted upload", async () => {
  const {document, window, FileTools} = load();
  window.tiny.localPath = path => path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  const sent = [];
  globalThis.TinyBlossomUpload = {upload: async (bytes, options) => {
    assert.equal(options.url, "https://relay.test/");
    sent.push(bytes.slice());
    if (sent.length === 1) throw Error("connection interrupted");
    return {descriptor: {sha256: await window.tiny.sha256hex(bytes), size: bytes.length}};
  }};
  const fileTools = new FileTools(); document.append(fileTools); fileTools.connectedCallback();
  const form = fileTools.querySelector("form:not([data-share])");
  form.elements.namedItem("file").files = [new File(["keep the same key"], "private.txt")];
  await fileTools.encryptUpload(form);
  const originalFragment = fileTools.pendingUpload.fragment;
  assert.equal(fileTools.querySelector("[data-upload-retry]").disabled, false);
  await fileTools.retryUpload();
  assert.deepEqual(sent[1], sent[0]);
  assert.equal(fileTools.fileFragment, originalFragment);
  assert.equal(fileTools.pendingUpload, null);
});
