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
  remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter(child => child !== this); }
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
  const window = {document, customElements, tiny: {blossom: {}, files: {}}, location: {href: "https://relay.test/files", origin: "https://relay.test", pathname: "/files", hash: ""}, addEventListener: () => {}, matchMedia: () => ({matches: false, addEventListener: () => {}})};
  class HTMLElement extends Node {}
  Object.assign(globalThis, {window, document, customElements, HTMLElement, location: window.location, Blob, URL, CustomEvent: class {}});
  globalThis.tiny = window.tiny;
  Object.defineProperty(globalThis, "navigator", {configurable: true, value: {clipboard: {writeText: async () => { throw Error("clipboard unavailable"); }}}});
  globalThis.btoa = value => Buffer.from(value, "binary").toString("base64"); globalThis.atob = value => Buffer.from(value, "base64").toString("binary");
  vm.runInThisContext(fs.readFileSync("internal/webui/tiny.js", "utf8"));
  if (workspace) { vm.runInThisContext(fs.readFileSync("internal/webui/blossom-manifests.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/blossom-encryption.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/blossom-upload.js", "utf8")); vm.runInThisContext(fs.readFileSync("internal/webui/file-workspace.js", "utf8")); } else { delete globalThis.tiny.blossom.upload; }
  vm.runInThisContext(fs.readFileSync("internal/webui/components.js", "utf8"));
  return {document, window, FileTools: customElements.registry["file-tools"], FileMirror: customElements.registry["file-mirror"], FileUpload: customElements.registry["file-upload"]};
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
    const upload = new customElements.registry["file-upload"]();
    upload.connectedCallback();
    const form = upload.querySelector("form");
    form.elements.namedItem("file").files = files;
    form.elements.namedItem("encrypt").checked = true;
    return upload.run(form);
  };
  return {...loaded, blobs, requests, store, open, originalFetch};
};
// sealedUpload stores one file through the upload control with encrypt checked
// and the plain signed PUT path, returning the share link and ciphertext.
const sealedUpload = async (window, FileUpload, file) => {
  let uploaded;
  delete window.tiny.blossom.upload;
  window.tiny.localPath = path => path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.signedFetch = async (_path, _method, body) => { if (_path === "/files/metadata") return {ok: true}; uploaded = new Uint8Array(body); return {json: async () => ({sha256: await window.tiny.sha256hex(body)})}; };
  const upload = new FileUpload(); window.document.append(upload); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [file];
  form.elements.namedItem("encrypt").checked = true;
  const link = await upload.run(form);
  return {upload, link, uploaded: () => uploaded};
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
    assert.equal(target.searchParams.has("filename"), false);
    assert.equal(target.searchParams.has("path"), false);
    if (init.body) assert.equal(new TextDecoder().decode(init.body).includes("héllo"), false);
  }
  assert.ok(server.blobs.size >= 8);
});

test("plain uploads retain names and folder paths within the current library folder", async () => {
  const {window, FileUpload} = load(true);
  window.location.href = "https://relay.test/files?view=library&path=Projects";
  const requests = [];
  window.tiny.signedFetch = async (target, method, bytes) => requests.push({target, method, bytes});
  const upload = new FileUpload();
  upload.connectedCallback();
  upload.controller = new AbortController();
  await upload.storePlain([folderFile("image", "Trip/photos/snow & sun.jpg"), new File(["notes"], "notes.txt")]);
  const first = new URL(requests[0].target, window.location.href);
  assert.equal(first.pathname, "/upload");
  assert.equal(first.searchParams.get("filename"), "snow & sun.jpg");
  assert.equal(first.searchParams.get("path"), "Projects/Trip/photos/snow & sun.jpg");
  assert.equal(new URL(requests[1].target, window.location.href).searchParams.get("path"), "Projects/notes.txt");
  assert.equal(new TextDecoder().decode(requests[0].bytes), "image");
  window.location.href = "https://relay.test/files?view=sites&path=weather";
  await upload.storePlain([new File(["file"], "new.txt")]);
  assert.equal(new URL(requests[2].target, window.location.href).searchParams.get("path"), "new.txt");
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

test("encrypted upload never touches the clipboard until Copy is pressed", async () => {
  const {window, FileUpload} = load(true);
  let copies = 0;
  navigator.clipboard.writeText = async () => { copies++; throw Error("denied"); };
  const {upload, link, uploaded} = await sealedUpload(window, FileUpload, new File(["secret plaintext"], "secret.txt", {type: "text/plain"}));
  assert.notEqual(new TextDecoder().decode(uploaded()), "secret plaintext");
  assert.match(link, /\/file\?hash=[0-9a-f]{64}#key=.*&iv=/);
  assert.equal(upload.querySelector("[data-share-url]").value, link);
  assert.equal(upload.querySelector("[data-share]").hidden, false);
  assert.equal(copies, 0);
  assert.equal(upload.querySelector("[data-open]").href, link);
  await upload.copyLink();
  assert.equal(copies, 1);
  assert.match(upload.out.textContent, /Copy the secret link from the field/);
});

test("plain uploads store every chosen file and refresh the listing", async () => {
  const {window, FileUpload} = load(true);
  const sent = [];
  let refreshed = 0;
  window.tiny.signedFetch = async (path, method, body, options) => { sent.push({path, method, size: body.byteLength, type: options.contentType}); return {}; };
  window.tiny.navigate = async () => { refreshed++; };
  const upload = new FileUpload(); window.document.append(upload); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [new File(["one"], "a.txt", {type: "text/plain"}), new File(["two"], "b.bin")];
  assert.equal(await upload.run(form), undefined);
  assert.deepEqual(sent, [{path: "/upload?filename=a.txt&path=a.txt", method: "PUT", size: 3, type: "text/plain"}, {path: "/upload?filename=b.bin&path=b.bin", method: "PUT", size: 3, type: "application/octet-stream"}]);
  assert.equal(refreshed, 1);
  assert.equal(upload.out.textContent, "Stored 2 files.");
});

test("BUD15 key is rejected before mirror request", async () => {
  const {document, window, FileMirror} = load(); const mirror = new FileMirror(); document.append(mirror); mirror.connectedCallback();
  mirror.form.querySelector("input").value = "blossom:" + "a".repeat(64) + "?enc=chk-v1&k=" + "1".repeat(64);
  let sent = false; window.tiny.signedFetch = async () => { sent = true; };
  await assert.rejects(() => mirror.submit(mirror.form), /keep k client side/);
  assert.equal(sent, false);
});

test("sharing a newly uploaded file uses its current key, hash and ciphertext size", async () => {
  const {document, window, FileTools, FileUpload} = load(true);
  const {link, uploaded} = await sealedUpload(window, FileUpload, new File(["new secret"], "private.txt", {type: "text/plain"}));
  window.location.hash = new URL(link).hash;
  const element = new FileTools();
  element.setAttribute("hash", await window.tiny.sha256hex(uploaded()));
  element.setAttribute("type", "text/plain");
  element.setAttribute("size", String(uploaded().length));
  document.append(element); element.connectedCallback();
  const shareForm = element.querySelector("[data-share]");
  shareForm.querySelector("input").value = "b".repeat(64);
  globalThis.nostr = {signEvent: async value => value};
  let input;
  globalThis.tiny.files.messages = {share: async (value, options) => { input = value; assert.equal(options.signer, globalThis.nostr); return {}; }};
  await element.share(shareForm);
  assert.ok(input, element.output.textContent);
  assert.equal(input.ciphertextHash, await window.tiny.sha256hex(uploaded()));
  assert.equal(input.size, uploaded().length);
  assert.equal(input.plaintextHash, await window.tiny.sha256hex(new TextEncoder().encode("new secret")));
  assert.match(input.key, /^[0-9a-f]{64}$/);
  assert.match(input.nonce, /^[0-9a-f]{24}$/);
  const key = await webcrypto.subtle.importKey("raw", Buffer.from(input.key, "hex"), "AES-GCM", false, ["decrypt"]);
  const plain = await webcrypto.subtle.decrypt({name: "AES-GCM", iv: Buffer.from(input.nonce, "hex")}, key, uploaded());
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
  const {document, window, FileTools, FileUpload} = load(true);
  const {link, uploaded} = await sealedUpload(window, FileUpload, new File(["round trip"], "round.txt", {type: "text/plain"}));
  window.location.hash = new URL(link).hash;
  const element = new FileTools();
  element.setAttribute("hash", await window.tiny.sha256hex(uploaded()));
  document.append(element);
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(uploaded());
    element.connectedCallback();
    await element.decryptFromFragment();
    assert.equal(element.output.textContent, "Decrypted locally.");
    assert.equal(await (await originalFetch(element.downloadURL)).text(), "round trip");
    const temporaryURL = element.downloadURL;
    element.disconnectedCallback();
    await assert.rejects(originalFetch(temporaryURL));
    uploaded()[0] ^= 1;
    await element.decryptFromFragment();
    assert.match(element.output.textContent, /ciphertext hash mismatch/);
  } finally { globalThis.fetch = originalFetch; }
});

test("retry keeps the original ciphertext and key after interrupted encrypted upload", async () => {
  const {document, window, FileUpload} = load(true);
  window.tiny.localPath = path => path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  const sent = [];
  globalThis.tiny.blossom.upload = {upload: async (bytes, options) => {
    assert.equal(options.url, "https://relay.test/");
    sent.push(bytes.slice());
    if (sent.length === 1) throw Error("connection interrupted");
    return {descriptor: {sha256: await window.tiny.sha256hex(bytes), size: bytes.length}};
  }};
  const upload = new FileUpload(); document.append(upload); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [new File(["keep the same key"], "private.txt")];
  form.elements.namedItem("encrypt").checked = true;
  await assert.rejects(upload.run(form), /connection interrupted/);
  const originalFragment = upload.pending.fragment;
  assert.equal(upload.querySelector("[data-retry]").disabled, false);
  const link = await upload.run();
  assert.deepEqual(sent[1], sent[0]);
  assert.equal(new URL(link).hash.slice(1), originalFragment);
  assert.equal(upload.pending, null);
});

test("manifest retry keeps completed chunks and reports readable progress", async t => {
  const server = workspaceServer(t);
  const transport = globalThis.fetch;
  let interrupted = true;
  const attempts = new Map();
  globalThis.fetch = async (url, init = {}) => {
    if (init.method === "PATCH") {
      const hash = new URL(url).pathname.split("/").at(-1);
      attempts.set(hash, (attempts.get(hash) || 0) + 1);
      if (interrupted && attempts.size === 2) return new Response(null, {status: 403});
    }
    return transport(url, init);
  };
  const bytes = new Uint8Array(5 * 1024 * 1024);
  bytes[0] = 1; bytes[2 * 1024 * 1024] = 2; bytes[4 * 1024 * 1024] = 3;
  const upload = new server.FileUpload(); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [folderFile(bytes, "video/clip.mp4")];
  form.elements.namedItem("encrypt").checked = true;
  await assert.rejects(upload.run(form), /403/);
  const first = attempts.keys().next().value;
  assert.equal(upload.manifestSession.completed.size, 1);
  interrupted = false;
  const link = await upload.run();
  assert.equal(attempts.get(first), 1, "accepted first chunk must not be reuploaded");
  const root = await server.open(link);
  assert.deepEqual(await root.download(root.entries[0]), bytes);
  upload.progress(3 * 1024 * 1024, 5 * 1024 * 1024);
  assert.match(upload.out.textContent, /60%.*3.0 MiB.*5.0 MiB/);
  assert.equal(upload.querySelector("progress").value, 0.6);
  for (const request of server.requests.filter(r => r.init.method === "PATCH")) {
    assert.equal(request.url.searchParams.get("purpose"), "chunk");
  }
});

test("members upload keeps the MP4 name and type and never creates a secret link", async () => {
  const {window, FileUpload} = load(true);
  const sent = [];
  window.tiny.signedFetch = async (path, method, bytes, options) => { sent.push({path, method, bytes, options}); };
  window.tiny.navigate = async () => {};
  const upload = new FileUpload(); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [new File(["mp4 bytes"], "holiday.mp4", {type: "video/mp4"})];
  form.elements.namedItem("access").value = "members";
  form.elements.namedItem("encrypt").checked = true;
  await upload.run(form);
  assert.equal(sent.length, 1);
  const url = new URL(sent[0].path, location.href);
  assert.equal(url.searchParams.get("access"), "members");
  assert.equal(url.searchParams.get("filename"), "holiday.mp4");
  assert.equal(sent[0].options.contentType, "video/mp4");
  assert.equal(new TextDecoder().decode(sent[0].bytes), "mp4 bytes");
  assert.equal(upload.querySelector("[data-share]").hidden, true);
});

test("retry finalizes the same encrypted root without reuploading or exposing its key", async () => {
  const {window, FileUpload} = load(true);
  window.tiny.localPath = path => path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  let uploads = 0, records = 0;
  window.tiny.blossom.upload = {upload: async (bytes, options) => {
    uploads++;
    assert.equal(options.metadata.purpose, "chunk");
    return {descriptor: {sha256: await window.tiny.sha256hex(bytes), size: bytes.length}};
  }};
  window.tiny.signedFetch = async (path, method, body) => {
    assert.equal(path, "/files/metadata");
    assert.equal(method, "POST");
    const record = JSON.parse(body);
    assert.match(record.name, /^Encrypted file [a-f0-9]{12}$/);
    assert.equal(record.purpose, "file");
    assert.equal(record.size, 7);
    assert.equal(body.includes("secret-name.txt"), false);
    assert.equal("key" in record, false);
    if (++records === 1) throw Error("server restarting");
    return {ok: true};
  };
  const upload = new FileUpload(); upload.connectedCallback();
  const form = upload.querySelector("form");
  form.elements.namedItem("file").files = [new File(["private"], "secret-name.txt")];
  form.elements.namedItem("encrypt").checked = true;
  await assert.rejects(upload.run(form), /server restarting/);
  const saved = upload.querySelector("[data-share-url]").value;
  assert.match(saved, /#key=/);
  assert.equal(upload.querySelector("[data-retry]").disabled, false);
  assert.equal(await upload.run(), saved);
  assert.equal(uploads, 1);
  assert.equal(records, 2);
});

test("a 100 MiB encrypted MP4 opens a media player after reconstruction", async t => {
  const server = workspaceServer(t);
  const bytes = new Uint8Array(100 * 1024 * 1024);
  for (let index = 0; index < 50; index++) bytes[index * 2 * 1024 * 1024] = index;
  const link = await server.store([new File([bytes], "large.mp4", {type: "video/mp4"})]);
  const root = await server.open(link);
  const video = root.querySelector("video");
  assert.ok(video, "decrypted manifest MP4 must have a player");
  assert.equal(video.controls, true);
  const data = await server.originalFetch(video.src).then(response => response.arrayBuffer());
  assert.deepEqual(new Uint8Array(data), bytes);
  assert.match(root.querySelector("output").textContent, /100.0 MiB/);
});

test("large uploads hand signed requests to Background Fetch and read the parked descriptor", async () => {
  const {window, FileUpload} = load(true);
  window.tiny.localPath = path => path;
  window.tiny.sha256hex = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
  window.tiny.authorization = async (url, method, body) => "Nostr " + method + ":" + new URL(url).pathname + ":" + body.byteLength;
  window.tiny.navigate = async () => {};
  const bytes = new Uint8Array(9 * 1024 * 1024);
  bytes[0] = 7;
  const hash = await window.tiny.sha256hex(bytes);
  const descriptor = {sha256: hash, size: bytes.byteLength};
  let started;
  const task = {result: "", uploaded: 0, uploadTotal: bytes.byteLength, listeners: [], addEventListener(name, fn) { this.listeners.push(fn); }, removeEventListener() {}};
  const parked = new Map();
  globalThis.caches = {open: async () => ({match: async key => parked.get(key), delete: async key => parked.delete(key), put: async (key, value) => parked.set(key, value)})};
  globalThis.BackgroundFetchManager = class {};
  const store = new Map();
  globalThis.localStorage = {getItem: key => store.get(key) ?? null, setItem: (key, value) => store.set(key, value), removeItem: key => store.delete(key), get length() { return store.size; }};
  Object.defineProperty(globalThis, "localStorage", {configurable: true, value: globalThis.localStorage});
  Object.keys = ((original => target => target === globalThis.localStorage ? [...store.keys()] : original(target)))(Object.keys);
  Object.defineProperty(globalThis, "navigator", {configurable: true, value: {clipboard: {writeText: async () => { throw Error("unavailable"); }}, serviceWorker: {controller: {}, ready: Promise.resolve({backgroundFetch: {fetch: async (id, requests, options) => { started = {id, requests, options}; return task; }, get: async () => null}})}}});
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (_url, init = {}) => new Response(null, {status: 204, headers: {Allow: "PATCH, PUT"}});
  try {
    const upload = new FileUpload(); window.document.append(upload); upload.connectedCallback();
    const form = upload.querySelector("form");
    form.elements.namedItem("file").files = [new File([bytes], "big.bin", {type: "application/octet-stream"})];
    form.elements.namedItem("background").checked = true;
    const run = upload.run(form);
    await new Promise(resolve => setTimeout(resolve, 50));
    assert.ok(started, "background fetch was not started");
    assert.equal(started.requests.length, 2);
    assert.equal(started.requests[0].method, "PATCH");
    assert.equal(new URL(started.requests[0].url).searchParams.get("filename"), "big.bin");
    assert.equal(new URL(started.requests[1].url).searchParams.get("path"), "big.bin");
    assert.equal(started.requests[1].headers.get("upload-offset"), String(5 * 1024 * 1024));
    assert.match(started.requests[0].headers.get("authorization"), /^Nostr PATCH:/);
    assert.equal(started.options.uploadTotal, bytes.byteLength);
    assert.equal(store.get("tiny.bg." + started.id) !== undefined, true);
    parked.set("https://relay.test/uploads/" + started.id, Response.json(descriptor));
    task.result = "success"; task.uploaded = bytes.byteLength;
    for (const fn of task.listeners) await fn();
    await run;
    assert.equal(store.has("tiny.bg." + started.id), false);
    assert.equal(parked.size, 0);
    assert.equal(upload.out.textContent, "Stored 1 file.");
  } finally { globalThis.fetch = originalFetch; }
});
