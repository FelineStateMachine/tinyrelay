import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, getPublicKey, verifyEvent } from "nostr-tools/pure";
import * as nip19 from "nostr-tools/nip19";
import { createHash } from "node:crypto";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const roomsSource = source.slice(source.indexOf("  // Rooms: the compose bar"), source.indexOf("  // JsonView renders any JSON value."));

// A small document: enough of the DOM for the room elements to render
// messages, find their targets and keep the list bounded.
export class FakeNode {
  constructor(tag) { this.localName = tag; this.childNodes = []; this.attributes = {}; this.dataset = {}; this.parent = null; this.id = ""; }
  get children() { return this.childNodes.filter(node => node instanceof FakeNode); }
  get firstElementChild() { return this.children[0] || null; }
  get isConnected() { return Boolean(this.parent) && (this.parent === document || this.parent.isConnected); }
  append(...nodes) { for (const node of nodes) { if (node instanceof FakeNode) { node.remove(); node.parent = this; this.childNodes.push(node); } else if (String(node)) this.childNodes.push(String(node)); } }
  insertBefore(node, before) { if (!before) return this.append(node); node.remove(); const index = this.childNodes.indexOf(before); node.parent = this; this.childNodes.splice(index, 0, node); }
  remove() { if (this.parent) { this.parent.childNodes = this.parent.childNodes.filter(node => node !== this); this.parent = null; } }
  replaceWith(node) { if (!this.parent) return; const index = this.parent.childNodes.indexOf(this); node.remove(); node.parent = this.parent; this.parent.childNodes[index] = node; this.parent = null; }
  replaceChildren(...nodes) { this.children.forEach(node => { node.parent = null; }); this.childNodes = []; this.append(...nodes); }
  get textContent() { return this.childNodes.map(node => typeof node === "string" ? node : node.textContent).join(""); }
  set textContent(value) { this.childNodes = value ? [String(value)] : []; }
  setAttribute(name, value) { if (name === "id") this.id = value; else this.attributes[name] = String(value); }
  getAttribute(name) { return name === "id" ? this.id || null : this.attributes[name] ?? null; }
  removeAttribute(name) { delete this.attributes[name]; }
  hasAttribute(name) { return name.startsWith("data-") ? name.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()) in this.dataset : name in this.attributes; }
  matches(selector) { return selector === this.localName; }
  addEventListener(name, handler) { this.listeners ||= {}; (this.listeners[name] ||= []).push(handler); }
  emit(name, event) { (this.listeners?.[name] || []).forEach(handler => handler(event)); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  querySelectorAll(selector) {
    const steps = selector.replace(/^:scope\s*>\s*/, "> ").trim().split(/\s*>\s*|\s+/).filter(Boolean);
    const child = selector.includes(">");
    let nodes = [this];
    for (const step of steps) {
      const next = [];
      for (const node of nodes) for (const candidate of child ? node.children : node.descendants()) if (candidate.matchesSimple(step)) next.push(candidate);
      nodes = next;
    }
    return nodes;
  }
  descendants() { return this.children.flatMap(node => [node, ...node.descendants()]); }
  matchesSimple(step) {
    if (step.startsWith("#")) return this.id === step.slice(1);
    const [, tag, attr] = step.match(/^([a-z-]+)?(?:\[([a-z-]+)\])?$/) || [];
    return (!tag || tag === this.localName) && (!attr || this.hasAttribute(attr));
  }
}
const document = new FakeNode("#document");
document.body = new FakeNode("body");
document.body.parent = document;
document.documentElement = {scrollHeight: 1000};
document.createElement = tag => new FakeNode(tag);
document.getElementById = id => document.body.descendants().find(node => node.id === id) || null;
document.querySelector = selector => document.body.querySelector(selector);
document.querySelectorAll = selector => document.body.querySelectorAll(selector);

export function setup({attributes = {}, result = {accepted: true}, change = false} = {}) {
  const sent = [], navigated = [];
  const secret = generateSecretKey(), pubkey = getPublicKey(secret);
  document.body.childNodes = [];
  const content = new FakeNode("div"); content.id = "content"; content.scrollHeight = 500; content.clientHeight = 400; content.scrollTop = 100;
  const list = new FakeNode("div"); list.id = "messages";
  const members = new FakeNode("ul"); members.id = "members";
  const agentRow = new FakeNode("li"); agentRow.dataset.agent = "";
  const agentKey = new FakeNode("nostr-name"); agentKey.setAttribute("pubkey", "b".repeat(64));
  agentRow.append(agentKey, " ", Object.assign(new FakeNode("small"), {childNodes: ["member | agent"]}));
  members.append(agentRow);
  document.body.append(content, list, members);
  class HTMLElement extends FakeNode {
    constructor() { super("custom"); }
    getAttribute(name) { return attributes[name] ?? super.getAttribute(name); }
  }
  class FormElement extends HTMLElement {
    connectedCallback() { this.form = {requestSubmit() {}}; }
    report(message) { this.message = message; }
  }
  const sandbox = {
    document, HTMLElement, FormElement, URL, Uint8Array, AbortController, encodeURIComponent, setTimeout, clearTimeout,
    location: {href: "https://relay.test/r/work/rooms/build", origin: "https://relay.test"},
    getComputedStyle: () => ({overflowY: "auto"}),
    window: {scrollY: 0, innerHeight: 800, scrollTo() {}, NostrSigner: {verifyEvent, decodeNpub: value => { const decoded = nip19.decode(value); if (decoded.type !== "npub") throw Error("not an npub"); return decoded.data; }}, nostr: {signEvent: async event => { const copy = JSON.parse(JSON.stringify(event)); if (change) copy.content = "changed"; return finalizeEvent(copy, secret); }}},
    tiny: {localPath: path => "/r/work" + path, sha256hex: async bytes => createHash("sha256").update(bytes).digest("hex"), navigate: async (href, push) => { navigated.push({href, push}); }, signedFetch: async (path, method, body, options) => { sent.push({path, method, options, event: JSON.parse(body)}); return Response.json(result); }},
    el: (tag, text) => { const node = new FakeNode(tag); if (text !== undefined) node.textContent = text; return node; },
    isHex64: value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value)
  };
  sandbox.EventSource = class {
    constructor(url, options) { this.url = url; this.options = options; this.listeners = {}; this.closed = false; sandbox.sources.push(this); }
    addEventListener(name, handler) { (this.listeners[name] = this.listeners[name] || []).push(handler); }
    emit(name, detail) { (this.listeners[name] || []).forEach(handler => handler(detail)); }
    close() { this.closed = true; }
  };
  sandbox.sources = [];
  const classes = vm.runInNewContext(`${roomsSource}\n({RoomCompose, RoomCreate, RoomAction, RoomLive, rooms: tiny.rooms})`, sandbox);
  return {...classes, sandbox, sent, navigated, pubkey, list, content, form: values => ({elements: Object.fromEntries(Object.entries(values).map(([name, value]) => [name, {name, value}])), reset() { this.resets = (this.resets || 0) + 1; }})};
}
const npub = nip19.npubEncode("d".repeat(64));
// Values built inside the sandbox belong to another realm; compare them as plain data.
const same = (actual, expected) => assert.deepEqual(JSON.parse(JSON.stringify(actual)), expected);

test("room-compose signs kind 9 with the room tag and mentions from @npub and @hex", async () => {
  const s = setup({attributes: {room: "build", pubkey: "x"}});
  const compose = new s.RoomCompose();
  compose.connectedCallback();
  const unsigned = compose.event(`hi @${npub} and @${"e".repeat(64)}, again @${"e".repeat(64)} not me@${"f".repeat(64)}`);
  assert.equal(unsigned.kind, 9);
  same(unsigned.tags, [["h", "build"], ["p", "d".repeat(64)], ["p", "e".repeat(64)]]);
  same(s.rooms.roomMentions("@npub1notakey and @abc"), []);
  const form = s.form({content: " hello room "});
  await compose.submit(form);
  assert.equal(s.sent[0].path, "/events");
  assert.equal(s.sent[0].options.contentType, "application/json");
  assert.equal(s.sent[0].event.kind, 9);
  assert.equal(s.sent[0].event.content, "hello room");
  assert.equal(verifyEvent(s.sent[0].event), true);
  assert.equal(form.resets, 1);
  assert.equal(s.list.children.length, 1);
  assert.equal(s.list.children[0].id, "msg-" + s.sent[0].event.id);
  assert.equal(s.content.scrollTop, 500);
  assert.throws(() => new (setup({attributes: {room: "Bad Room"}}).RoomCompose)().event("x"), /room id/);
});

test("room-compose replies in a thread with the root and its author, and refuses a changed event", async () => {
  const root = "1".repeat(64), author = "2".repeat(64);
  const s = setup({attributes: {room: "build", kind: "12", root, "root-pubkey": author, pubkey: "3".repeat(64)}});
  const compose = new s.RoomCompose();
  same(compose.event("ok").tags, [["h", "build"], ["e", root], ["p", author]]);
  const own = setup({attributes: {room: "build", kind: "12", root, "root-pubkey": author, pubkey: author}});
  same(new own.RoomCompose().event("ok").tags, [["h", "build"], ["e", root]]);
  const changed = setup({attributes: {room: "build"}, change: true});
  await assert.rejects(new changed.RoomCompose().submit(changed.form({content: "x"})), /changed event/);
  assert.equal(changed.sent.length, 0);
  const refused = setup({attributes: {room: "build"}, result: {accepted: false, message: "restricted: members only"}});
  await assert.rejects(new refused.RoomCompose().submit(refused.form({content: "x"})), /members only/);
});

const selectedFile = (name = "chart.png", type = "image/png", text = "image bytes") => ({name, type, size: Buffer.byteLength(text), arrayBuffer: async () => Uint8Array.from(Buffer.from(text)).buffer});
const descriptorFor = (file, bytes) => {
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  return {sha256, size: file.size, type: file.type, url: "https://relay.test/r/work/media/" + sha256 + ".png"};
};

test("room-compose uploads only on send, then publishes Buzz metadata and an attachment-only message", async () => {
  const s = setup({attributes: {room: "build"}}), compose = new s.RoomCompose();
  const uploads = [], publish = s.sandbox.tiny.signedFetch;
  const file = selectedFile("my chart.png");
  s.sandbox.tiny.signedFetch = async (path, method, bytes, options) => {
    if (path === "/events") return publish(path, method, bytes, options);
    uploads.push({path, method, bytes, options});
    return Response.json(descriptorFor(file, bytes));
  };
  compose.addFiles([file]);
  assert.equal(uploads.length, 0);
  const form = s.form({content: ""});
  await compose.submit(form);
  assert.equal(uploads.length, 1);
  assert.equal(uploads[0].path, "/rooms/build/attachments?filename=my%20chart.png");
  assert.equal(uploads[0].method, "PUT");
  assert.equal(uploads[0].options.contentType, "image/png");
  const descriptor = descriptorFor(file, uploads[0].bytes);
  assert.equal(s.sent[0].event.content, `![image](${descriptor.url})`);
  same(s.sent[0].event.tags, [["h", "build"], ["imeta", "url " + descriptor.url, "m image/png", "x " + descriptor.sha256, "size " + file.size, "filename my chart.png"]]);
  assert.equal(verifyEvent(s.sent[0].event), true);
  assert.equal(compose.pendingFiles.length, 0);
  assert.equal(form.resets, 1);
});

test("room-compose retains uploaded files after a publish failure and does not upload them twice on retry", async () => {
  const s = setup({attributes: {room: "build"}}), compose = new s.RoomCompose(), file = selectedFile();
  let uploads = 0, publishes = 0;
  const publish = s.sandbox.tiny.signedFetch;
  s.sandbox.tiny.signedFetch = async (path, method, bytes, options) => {
    if (path !== "/events") { uploads++; return Response.json(descriptorFor(file, bytes)); }
    if (++publishes === 1) throw Error("connection lost");
    return publish(path, method, bytes, options);
  };
  compose.addFiles([file]);
  const form = s.form({content: "a chart"});
  await assert.rejects(compose.submit(form), /connection lost/);
  assert.equal(compose.pendingFiles.length, 1);
  assert.equal(form.resets, undefined);
  await compose.submit(form);
  assert.equal(uploads, 1);
  assert.equal(publishes, 2);
  assert.equal(s.sent[0].event.content.startsWith("a chart\n\n![image]"), true);
});

test("room-compose refuses oversized selections and untrusted upload descriptors without publishing", async () => {
  const s = setup({attributes: {room: "build"}}), compose = new s.RoomCompose(), file = selectedFile();
  assert.throws(() => compose.addFiles([{...file, size: 32 * 1024 * 1024 + 1}]), /32 MiB/);
  assert.throws(() => compose.addFiles(Array.from({length: 9}, (_, i) => selectedFile(i + ".png"))), /8 files/);
  compose.addFiles([file]);
  for (const change of [d => ({...d, sha256: "0".repeat(64)}), d => ({...d, size: 1}), d => ({...d, url: "https://another.test/media/" + d.sha256 + ".png"})]) {
    s.sandbox.tiny.signedFetch = async (_path, _method, bytes) => Response.json(change(descriptorFor(file, bytes)));
    await assert.rejects(compose.submit(s.form({content: ""})), /descriptor|hash|size|URL/i);
    assert.equal(s.sent.length, 0);
    assert.equal(compose.pendingFiles.length, 1);
  }
});

test("room-compose does not publish after leaving during an upload", async () => {
  const s = setup({attributes: {room: "build"}}), compose = new s.RoomCompose(), file = selectedFile();
  s.sandbox.tiny.signedFetch = async (_path, _method, bytes) => {
    compose.disconnectedCallback();
    return Response.json(descriptorFor(file, bytes));
  };
  compose.addFiles([file]);
  await assert.rejects(compose.submit(s.form({content: ""})), /cancel/i);
  assert.equal(s.sent.length, 0);
});

test("room-create derives the id from the name and signs 9007 before opening the room", async () => {
  const s = setup();
  assert.equal(s.rooms.roomID("Build & Release  2026!"), "build-release-2026");
  assert.equal(s.rooms.roomID("  ---  "), "");
  assert.equal(s.rooms.roomID("x".repeat(80)).length, 64);
  const create = new s.RoomCreate();
  const {id, event} = create.event({name: " Build & Release ", about: " nightly ", access: "members"});
  assert.equal(id, "build-release");
  assert.equal(event.kind, 9007);
  same(event.tags, [["h", "build-release"], ["name", "Build & Release"], ["about", "nightly"], ["visibility", "members"]]);
  same(create.event({name: "general"}).event.tags, [["h", "general"], ["name", "general"], ["visibility", "open"]]);
  assert.throws(() => create.event({name: "!!!"}), /letters or digits/);
  const fixed = create.event({name: "Hermes", id: " 9782AFBD-88c9-43b1-8b3a-5a68e99d3c52 "});
  assert.equal(fixed.id, "9782afbd-88c9-43b1-8b3a-5a68e99d3c52");
  same(fixed.event.tags, [["h", "9782afbd-88c9-43b1-8b3a-5a68e99d3c52"], ["name", "Hermes"], ["visibility", "open"]]);
  assert.throws(() => create.event({name: "Hermes", id: "not valid!"}), /lowercase letters/);
  await create.submit(s.form({name: "Build", about: "", access: "open"}));
  assert.equal(s.sent[0].event.kind, 9007);
  assert.equal(verifyEvent(s.sent[0].event), true);
  same(s.navigated, [{href: "/r/work/rooms/build", push: true}]);
});

test("room-action signs membership, settings, join and leave events", () => {
  const s = setup({attributes: {room: "build", kind: "9000"}});
  const add = new s.RoomAction();
  same(add.event({pubkey: npub, role: "admin"}).tags, [["h", "build"], ["p", "d".repeat(64), "admin"]]);
  same(add.event({pubkey: "e".repeat(64), role: "nonsense"}).tags, [["h", "build"], ["p", "e".repeat(64), "member"]]);
  assert.throws(() => add.event({pubkey: "who"}), /npub or a 64/);
  const edit = new (setup({attributes: {room: "build", kind: "9002"}}).RoomAction)();
  same(edit.event({name: " Build ", about: "x", picture: "", access: "members"}).tags, [["h", "build"], ["name", "Build"], ["about", "x"], ["picture", ""], ["visibility", "members"]]);
  assert.throws(() => edit.event({name: " "}), /Enter a name/);
  same(new (setup({attributes: {room: "build", kind: "9022"}}).RoomAction)().event().tags, [["h", "build"]]);
  assert.equal(new (setup({attributes: {room: "build", kind: "9021"}}).RoomAction)().event().kind, 9021);
  assert.throws(() => new (setup({attributes: {room: "build", kind: "9008"}}).RoomAction)().event(), /Unsupported/);
});

test("room-live appends streamed messages once, summarizes reactions and reconnects with backoff", async () => {
  const s = setup({attributes: {room: "build"}});
  const live = new s.RoomLive();
  live.connectedCallback();
  assert.equal(s.sandbox.sources.length, 1);
  const source = s.sandbox.sources[0];
  assert.equal(source.url, "/r/work/rooms/build/stream");
  assert.equal(source.options.withCredentials, true);
  const secret = generateSecretKey();
  const message = finalizeEvent({kind: 9, created_at: 1757203600, tags: [["h", "build"], ["p", "c".repeat(64)]], content: "see https://example.com/x, ok"}, secret);
  source.emit("message", {data: JSON.stringify(message)});
  source.emit("message", {data: JSON.stringify(message)});
  assert.equal(s.list.children.length, 1);
  const node = s.list.children[0];
  assert.equal(node.id, "msg-" + message.id);
  assert.equal(node.dataset.kind, "9");
  assert.equal(node.querySelector("header b nostr-name").getAttribute("pubkey"), message.pubkey);
  const link = node.querySelector("p a");
  assert.equal(link.href, "https://example.com/x");
  assert.equal(node.querySelector("div p").textContent, "see https://example.com/x, ok");
  assert.equal(node.querySelector("footer span nostr-name").getAttribute("pubkey"), "c".repeat(64));
  source.emit("message", {data: "not json"});
  source.emit("message", {data: JSON.stringify({...message, id: "9".repeat(64), kind: 20001})});
  assert.equal(s.list.children.length, 1);
  const reaction = finalizeEvent({kind: 7, created_at: 1757203700, tags: [["h", "build"], ["e", message.id]], content: "+"}, secret);
  source.emit("message", {data: JSON.stringify(reaction)});
  source.emit("message", {data: JSON.stringify({...reaction, id: "8".repeat(64)})});
  const summary = node.querySelector("footer span[data-reaction]");
  assert.equal(summary.dataset.reaction, "+1");
  assert.equal(summary.textContent, "+1 2");
  const edit = finalizeEvent({kind: 40003, created_at: 1757203750, tags: [["h", "build"], ["e", message.id]], content: "see https://example.com/y instead"}, secret);
  source.emit("message", {data: JSON.stringify(edit)});
  assert.equal(node.querySelector("div p").textContent, "see https://example.com/y instead");
  assert.equal(node.querySelector("p a").href, "https://example.com/y");
  assert.equal(node.dataset.edited, "");
  assert.equal(node.querySelectorAll("footer span[data-edited]").length, 1);
  const stranger = finalizeEvent({kind: 40003, created_at: 1757203760, tags: [["h", "build"], ["e", message.id]], content: "hijacked"}, generateSecretKey());
  source.emit("message", {data: JSON.stringify(stranger)});
  assert.equal(node.querySelector("div p").textContent, "see https://example.com/y instead");
  assert.equal(s.list.children.length, 1);
  // An agent's message carries the marker from the members list.
  const agent = finalizeEvent({kind: 11, created_at: 1757203800, tags: [["h", "build"]], content: "draft"}, secret);
  s.rooms.roomAppend({...agent, pubkey: "b".repeat(64)}, {room: "build"});
  const agentNode = s.list.children[1];
  assert.equal("agent" in agentNode.dataset, true);
  assert.equal(agentNode.querySelector("footer a").href, "/r/work/rooms/build/thread/" + agent.id);
  assert.equal(agentNode.querySelector("header").textContent.includes("| member"), true);
  // The list stays bounded.
  for (let i = 0; i < 520; i++) s.rooms.roomAppend({...message, id: i.toString(16).padStart(64, "0")}, {room: "build"});
  assert.equal(s.list.children.length, 500);
  source.emit("error", {});
  assert.equal(source.closed, true);
  assert.equal(live.textContent, "reconnecting");
  assert.equal(live.delay, 2000);
  live.close();
  assert.equal(live.timer, null);
});

test("room-live on a thread page keeps only replies to its root", () => {
  const root = "a".repeat(64);
  const s = setup({attributes: {room: "build", root}});
  const live = new s.RoomLive();
  live.connectedCallback();
  const source = s.sandbox.sources[0];
  const secret = generateSecretKey();
  source.emit("message", {data: JSON.stringify(finalizeEvent({kind: 9, created_at: 1, tags: [["h", "build"]], content: "chat"}, secret))});
  source.emit("message", {data: JSON.stringify(finalizeEvent({kind: 12, created_at: 2, tags: [["h", "build"], ["e", "b".repeat(64)]], content: "elsewhere"}, secret))});
  const reply = finalizeEvent({kind: 12, created_at: 3, tags: [["h", "build"], ["e", root]], content: "here"}, secret);
  source.emit("message", {data: JSON.stringify(reply)});
  assert.equal(s.list.children.length, 1);
  assert.equal(s.list.children[0].id, "msg-" + reply.id);
  assert.equal(s.list.children[0].querySelector("footer"), null);
  live.disconnectedCallback();
  assert.equal(source.closed, true);
});

test("chat markdown renders the shared subset without ever parsing markup", () => {
  const s = setup();
  const body = s.rooms.chatMarkdown(new FakeNode("div"), "It works: **[the page](https://012.run/wiki/agents)**.\nsee https://example.com/x, plus <b>not markup</b>\n\n- `code https://not.a.link`\n- [x](javascript:alert(1))\n\n```\nhttps://in.code\n```");
  const [p, list, pre] = body.children;
  assert.equal(p.localName, "p");
  assert.equal(p.querySelector("strong a").href, "https://012.run/wiki/agents");
  assert.equal(p.querySelectorAll("br").length, 1);
  assert.equal(p.querySelectorAll("a").length, 2);
  assert.equal(p.textContent, "It works: the page.see https://example.com/x, plus <b>not markup</b>");
  assert.equal(list.localName, "ul");
  assert.equal(list.children[0].querySelector("code").textContent, "code https://not.a.link");
  assert.equal(list.children[0].querySelectorAll("a").length, 0);
  assert.equal(list.children[1].textContent, "[x](javascript:alert(1))");
  assert.equal(list.children[1].querySelectorAll("a").length, 0);
  assert.equal(pre.localName, "pre");
  assert.equal(pre.querySelector("code").textContent, "https://in.code");
  assert.equal(pre.querySelectorAll("a").length, 0);
});

test("room attachment renderer matches visible imeta URLs and preserves prose and code", () => {
  const s = setup(), url = "https://cdn.example.test/report.pdf";
  const event = {content: "See [report](" + url + ") and `" + url + "`", tags: [["imeta", "url " + url, "m application/pdf", "filename report.pdf"]]};
  const items = s.rooms.roomAttachments(event);
  assert.equal(items.length, 1);
  assert.equal(s.rooms.attachmentContent(event.content, items), event.content);
  assert.equal(s.rooms.roomAttachments({content: "```\n" + url + "\n```", tags: event.tags}).length, 0);
  assert.equal(s.rooms.roomAttachments({content: url + "-backup", tags: event.tags}).length, 0);
  assert.equal(s.rooms.roomAttachments({content: url, tags: [["imeta", "url " + url, "m image/png"], ["imeta", "url " + url, "m image/png"]]}).length, 1);
});

test("room attachment renderer replaces edited media without stale nodes", () => {
  const s = setup(), id = "a".repeat(64), author = "b".repeat(64), oldURL = "https://cdn.example.test/old.png", newURL = "https://cdn.example.test/new.mp4";
  const node = s.rooms.messageNode({id, pubkey: author, kind: 9, created_at: 1, content: oldURL, tags: [["imeta", "url " + oldURL, "m image/png"]]});
  s.list.append(node);
  s.rooms.roomEdit({id: "c".repeat(64), pubkey: author, kind: 40003, created_at: 3, content: newURL, tags: [["e", id], ["imeta", "url " + newURL, "m video/mp4"]]});
  assert.equal(node.querySelectorAll("img").length, 0);
  assert.equal(node.querySelectorAll("video").length, 1);
  s.rooms.roomEdit({pubkey: author, kind: 40003, created_at: 2, content: oldURL, tags: [["e", id], ["imeta", "url " + oldURL, "m image/png"]]});
  assert.equal(node.querySelectorAll("video").length, 1, "older edit must not replace newer media");
  s.rooms.roomEdit({pubkey: author, kind: 40003, created_at: 4, content: "Attachment removed", tags: [["e", id]]});
  assert.equal(node.querySelectorAll("video").length, 0);
  assert.equal(node.querySelectorAll("[data-room-attachments]").length, 0);
});
