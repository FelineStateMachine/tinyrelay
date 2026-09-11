import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import {getEventHash, verifyEvent} from "nostr-tools/pure";

class FakeNode {
  constructor(name = "node") { this.nodeName = name; this.childNodes = []; this.dataset = {}; this.attributes = {}; this.style = {}; this.isConnected = true; }
  append(...children) { this.childNodes.push(...children.flat().filter(Boolean)); }
  replaceChildren(...children) { this.childNodes = children; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] || null; }
  addEventListener() {}
  removeEventListener() {}
  querySelector() { return null; }
  querySelectorAll() { return []; }
  closest() { return null; }
}
class FakeElement extends FakeNode {}
const definitions = new Map();
globalThis.HTMLElement = FakeElement;
globalThis.document = {visibilityState: "visible", createElement: name => new FakeNode(name), createTextNode: text => Object.assign(new FakeNode("text"), {textContent: text}), addEventListener() {}, removeEventListener() {}};
globalThis.customElements = {get: name => definitions.get(name), define: (name, value) => definitions.set(name, value)};
globalThis.location = {href: "https://relay.test/chat", pathname: "/chat"};
globalThis.setInterval = () => 0; globalThis.clearInterval = () => {};

const key = char => char.repeat(64);
const alice = key("a"), bob = key("b"), carol = key("c");
const rumor = (id, sender, recipient, created_at, content, kind = 14, extra = []) => ({id, kind, pubkey: sender, created_at, content, tags: [["p", recipient], ...extra]});
const wrap = (id, r) => ({id, kind: 1059, pubkey: bob, created_at: r.created_at, tags: [["p", alice]], rumor: r});
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise, resolve}; };
const signer = {getPublicKey: async () => alice};
const protocol = {
  normalizePubkey: value => String(value).toLowerCase(),
  decrypt: async (event, options) => { if (event.invalid) throw Error("invalid wrap"); if (event.recipient && event.recipient !== options.actor) throw Error("wrong recipient"); return {rumor: event.rumor}; },
  relayList: value => value, queryRelayList: async () => ["wss://relay.test"],
  build: async input => ({rumor: rumor("built", alice, input.recipient, 100, input.content), events: [{event: {id: "receiver-wrap"}}, {event: {id: "sender-wrap"}}]}), publishEvent: async () => true
};
let pages = [];
globalThis.tiny = {chat: {protocol}, signer: () => signer, signedFetch: async () => ({ok: true, json: async () => pages.shift()}), localPath: value => value, util: {relayURL: value => value}, rooms: {linkify: value => Object.assign(new FakeNode("text"), {textContent: value})}, ui: {FormElement: FakeElement}, navigate: async () => {}, signing: {publish: async () => ({})}};
globalThis.NostrSigner = {verifyEvent: () => true};
vm.runInThisContext(fs.readFileSync("internal/webui/chat.js", "utf8"), {filename: "chat.js"});
const ui = tiny.chat.ui;

test("cache accepts files, rejects groups, deduplicates and folds reactions", async () => {
  const value = ui.stateFor(alice), first = rumor(key("1"), alice, bob, 20, "one"), second = rumor("two", bob, alice, 10, "two");
  const group = rumor("group", alice, bob, 30, "group", 14, [["p", carol]]), reaction = rumor("reaction", bob, alice, 31, "+", 7, [["e", first.id]]), outsiderReaction = rumor("outsider-reaction", carol, alice, 31, "+", 7, [["e", first.id]]), file = rumor("file", bob, alice, 32, "blossom://relay/file", 15, [["file-type", "video/mp4"]]);
  await ui.cacheEvents(value, [wrap("one-wrap", first), wrap("one-duplicate", first), wrap("two-wrap", second), wrap("group-wrap", group), wrap("reaction-wrap", reaction), wrap("outsider-reaction-wrap", outsiderReaction), wrap("file-wrap", file)], signer);
  assert.deepEqual(ui.messages(value).map(row => row.content), ["two", "one", "blossom://relay/file"]); assert.deepEqual([...ui.reactions(value, first)], [["♥", 1]]); assert.equal(value.events.size, 5); assert.equal(ui.participants(group).length, 3);
});

test("concurrent loads share one request and older pagination clears cursor", async () => {
  ui.clear(); pages = [{events: [wrap("new", rumor("new", bob, alice, 100, "new"))], next_cursor: "older"}]; let calls = 0; const original = tiny.signedFetch; tiny.signedFetch = async (...args) => { calls++; return original(...args); };
  await Promise.all([ui.load(alice), ui.load(alice)]); assert.equal(calls, 1); pages = [{events: [wrap("old", rumor("old", alice, bob, 1, "old"))], next_cursor: ""}]; await ui.load(alice, true); assert.equal(ui.stateFor(alice).cursor, ""); tiny.signedFetch = original;
});

test("refresh scans recent overlap without resetting an older cursor", async () => {
  ui.clear(); pages = [{events: [wrap("seed", rumor("seed", bob, alice, 2000000000, "seed"))], next_cursor: "deep"}]; await ui.load(alice); assert.equal(ui.stateFor(alice).cursor, "deep"); pages = [{events: [wrap("fresh", rumor("fresh", bob, alice, 2000000100, "fresh"))], next_cursor: "overlap"}, {events: [wrap("overlap-old", rumor("overlap-old", alice, bob, 1999999999, "old"))], next_cursor: ""}]; const oldNow = Date.now; Date.now = () => 2000000200 * 1000; await ui.load(alice); Date.now = oldNow; assert.equal(ui.stateFor(alice).events.has("fresh"), true); assert.equal(ui.stateFor(alice).events.has("overlap-old"), true); assert.equal(ui.stateFor(alice).cursor, "deep");
});

test("partial delivery retries exact wraps and skips acknowledged copy", async () => {
  ui.clear(); const value = ui.stateFor(alice); let buildCalls = 0, published = [], reportPending; protocol.build = async input => { buildCalls++; return {rumor: rumor("stable", alice, input.recipient, 200, input.content), events: [{event: {id: "wrap-a"}}, {event: {id: "wrap-b"}}]}; }; let attempt = 0; protocol.publishEvent = async event => { published.push(event.id); attempt++; return attempt === 1 || attempt === 3; };
  const input = {recipient: bob, content: "retry"}; await assert.rejects(ui.deliver(value, input, null, pending => { reportPending = pending; }), /history copy failed/); assert.equal(buildCalls, 1); assert.deepEqual(published, ["wrap-a", "wrap-b"]); await ui.deliver(value, input, reportPending, pending => { reportPending = pending; }); assert.deepEqual(published, ["wrap-a", "wrap-b", "wrap-b"]); protocol.publishEvent = async () => true;
});

test("logout or signer replacement during pending decryption cannot cache plaintext", async () => {
  ui.clear(); const value = ui.stateFor(alice), gate = deferred(); let activeActor = alice; const changingSigner = {getPublicKey: async () => activeActor}; protocol.decrypt = () => gate.promise; const pending = ui.cacheEvents(value, [wrap("pending", rumor("pending", bob, alice, 1, "secret"))], changingSigner); activeActor = bob; gate.resolve({rumor: rumor("pending", bob, alice, 1, "secret")}); await pending; assert.equal(value.events.size, 0); ui.clear(); protocol.decrypt = async event => ({rumor: event.rumor});
});

test("source parity fixture verifies and preserves its signed event hash", () => {
  const source = JSON.parse(fs.readFileSync("scripts/fixtures/chat-client-parity.json", "utf8")).events[0]; assert.equal(verifyEvent(source), true); assert.equal(getEventHash({...source, id: undefined, sig: undefined}), source.id);
});

test("custom chat elements are registered", () => { for (const name of ["direct-chats", "direct-thread", "direct-compose", "direct-start", "direct-settings"]) assert.ok(customElements.get(name)); });

test("wrapped retractions remove only their author's message or reaction", async () => {
  ui.clear();
  const value = ui.stateFor(alice);
  const original = rumor(key("1"), alice, bob, 10, "message");
  const like = rumor(key("2"), bob, alice, 11, "+", 7, [["e", original.id]]);
  const malicious = rumor(key("3"), carol, alice, 12, "", 5, [["e", original.id]]);
  await ui.cacheEvents(value, [wrap("orig", original), wrap("like", like), wrap("bad-delete", malicious)], signer);
  assert.equal(ui.messages(value).length, 1);
  assert.deepEqual([...ui.reactions(value, original)], [["♥", 1]]);
  await ui.cacheEvents(value, [wrap("retract", rumor(key("4"), bob, alice, 13, "", 5, [["e", like.id]]))], signer);
  assert.deepEqual([...ui.reactions(value, original)], []);
  await ui.cacheEvents(value, [wrap("delete", rumor(key("5"), alice, bob, 14, "", 5, [["e", original.id]]))], signer);
  assert.equal(ui.messages(value).length, 0);
});
