// Approvals in the browser: the nostr-react element signs the answer the
// person chose, and the service worker routes a notification action to the
// page that asks for confirmation. Both run in a sandbox with a fake DOM.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, verifyEvent } from "nostr-tools";
import { sharedSigning } from "./test-signing.mjs";

const request = "a".repeat(64), asker = "b".repeat(64);
const source = fs.readFileSync("internal/webui/components.js", "utf8");
const reactSource = source.slice(source.indexOf("  const publishSigned = "), source.indexOf("  // ApprovalItem reacts"));

// node builds enough of an element for the confirmation prompt: text,
// listeners, children and focus.
const node = (name, text) => ({
  name, textContent: text ?? "", children: [], handlers: {}, disabled: false, focused: false, dataset: {},
  addEventListener(event, fn) { this.handlers[event] = fn; },
  replaceChildren(...items) { this.children = items; },
  focus() { this.focused = true; },
  click() { return this.handlers.click?.(); }
});

function setup(attributes, options = {}) {
  const sent = [], signed = [], listeners = {};
  let navigated = null;
  class FormElement {
    constructor() { this.form = {buttons: []}; this.output = node("output"); this.reports = []; }
    getAttribute(name) { return attributes[name] ?? null; }
    report(message, error) { this.reports.push(message); this.output.textContent = message; if (error) this.output.dataset.error = ""; }
    busy() {}
  }
  const window = {NostrSigner: {verifyEvent}, nostr: options.noSigner ? undefined : {signEvent: async event => {
    signed.push(structuredClone(event));
    if (options.change) event.content = "changed";
    return finalizeEvent(structuredClone(event), generateSecretKey());
  }}};
  const tiny = {
    signedFetch: async (path, method, body) => { sent.push({path, method, event: JSON.parse(body)}); return Response.json(options.result ?? {accepted: true}); },
    navigate: async href => { navigated = href; }
  };
  tiny.signing = sharedSigning(window, tiny);
  const document = {addEventListener(event, fn) { listeners[event] = fn; }};
  const Constructor = vm.runInNewContext(`${reactSource}\nNostrReact`, {FormElement, window, tiny, document, el: node, isHex64: value => /^[0-9a-f]{64}$/.test(value || ""), globalThis: {location: {href: "https://tiny.example/approvals?id=" + request + "&answer=approve"}}, Date, JSON, Math, Object, Response});
  const component = new Constructor();
  return {component, sent, signed, listeners, navigated: () => navigated};
}
const attrs = {event: request, pubkey: asker, kind: "1111"};

test("approve and deny sign a kind 7 reaction naming the request and its asker", async () => {
  for (const [choice, word] of [["+", "Approved."], ["-", "Denied."]]) {
    const s = setup(attrs);
    await s.component.submit(s.component.form, choice);
    assert.equal(s.sent.length, 1);
    assert.equal(s.sent[0].path, "/events");
    assert.equal(s.sent[0].method, "POST");
    const event = s.sent[0].event;
    assert.equal(event.kind, 7);
    assert.equal(event.content, choice);
    assert.deepEqual(event.tags, [["e", request, "", asker], ["p", asker], ["k", "1111"]]);
    assert.equal(verifyEvent(event), true);
    assert.equal(s.component.reports.at(-1), word);
    assert.equal(s.navigated(), "https://tiny.example/approvals");
  }
});

test("the pressed button chooses the answer and nothing is signed without a choice", async () => {
  const s = setup(attrs);
  await assert.rejects(s.component.submit(s.component.form), /Choose Approve or Deny/);
  s.component.choice = "-";
  await s.component.submit(s.component.form);
  assert.equal(s.sent[0].event.content, "-");
  assert.equal(s.component.choice, null);
});

test("a missing address, a changed event or a relay refusal signs or publishes nothing", async () => {
  const missing = setup({event: request});
  await assert.rejects(missing.component.submit(missing.component.form, "+"), /request address/);
  assert.equal(missing.signed.length, 0);
  const changed = setup(attrs, {change: true});
  await assert.rejects(changed.component.submit(changed.component.form, "+"), /changed/);
  assert.equal(changed.sent.length, 0);
  const refused = setup(attrs, {result: {accepted: false, message: "blocked"}});
  await assert.rejects(refused.component.submit(refused.component.form, "+"), /blocked/);
  assert.equal(refused.navigated(), null);
  const unsigned = setup(attrs, {noSigner: true});
  await assert.rejects(unsigned.component.submit(unsigned.component.form, "+"), /Connect a signer/);
});

test("the confirmation prompt signs only after Confirm and waits for a signer", async () => {
  const s = setup(attrs);
  s.component.prompt("approve");
  assert.equal(s.signed.length, 0);
  const [question, confirm, , cancel] = s.component.output.children;
  assert.equal(question.textContent, "Approve this request? ");
  assert.equal(confirm.textContent, "Confirm");
  assert.equal(confirm.focused, true);
  cancel.click();
  assert.equal(s.component.output.textContent, "");
  s.component.prompt("deny");
  await s.component.output.children[1].click();
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(s.sent.length, 1);
  assert.equal(s.sent[0].event.content, "-");
  const unknown = setup(attrs);
  unknown.component.prompt("reply");
  assert.equal(unknown.component.output.children.length, 0);
  const waiting = setup(attrs, {noSigner: true});
  waiting.component.prompt("approve");
  assert.match(waiting.component.output.textContent, /Connect a signer/);
  assert.equal(typeof waiting.listeners["tiny:signer"], "function");
});

// The service worker runs against a fake self with a registration and a
// client list, so the click handler's window choice is observable.
const workerSource = fs.readFileSync("internal/webui/sw.js", "utf8");
function worker({windows = []} = {}) {
  const handlers = {}, shown = [], opened = [];
  const self = {
    addEventListener(name, fn) { handlers[name] = fn; },
    skipWaiting() {},
    registration: {scope: "https://tiny.example/", showNotification: async (title, options) => { shown.push({title, options}); }},
    clients: {claim() {}, matchAll: async () => windows, openWindow: async url => { opened.push(url); return {focus() {}}; }}
  };
  vm.runInNewContext(workerSource, {self, navigator: {}, location: new URL("https://tiny.example/"), URL, caches: {keys: async () => []}, console});
  return {handlers, shown, opened};
}
const waitUntil = () => { let done; const event = {waitUntil(promise) { done = promise; }}; return {event, finished: () => done}; };

test("push renders the relay's actions on the notification", async () => {
  const w = worker();
  const {event, finished} = waitUntil();
  event.data = {json: () => ({title: "tiny", body: "hermes asks: Publish?", url: "https://tiny.example/approvals?id=" + request, tag: "tiny-approvals", badge: 1, actions: [{action: "approve", title: "Approve"}, {action: "deny", title: "Deny"}, {action: "reply", title: "Reply"}, {action: "bad action"}]})};
  w.handlers.push(event);
  await finished();
  assert.equal(w.shown.length, 1);
  assert.equal(JSON.stringify(w.shown[0].options.actions), JSON.stringify([{action: "approve", title: "Approve"}, {action: "deny", title: "Deny"}, {action: "reply", title: "Reply"}]));
  assert.equal(w.shown[0].options.data.url, "https://tiny.example/approvals?id=" + request);
  const plain = worker();
  const second = waitUntil();
  second.event.data = {json: () => ({title: "tiny", body: "Replied to you"})};
  plain.handlers.push(second.event);
  await second.finished();
  assert.equal(plain.shown[0].options.actions.length, 0);
});

test("a notification action opens the answer in the existing window, or a new one", async () => {
  const url = "https://tiny.example/approvals?id=" + request;
  const navigated = [];
  const open = {url: "https://tiny.example/inbox", navigate: async target => { navigated.push(target); return {focus() { navigated.push("focused"); }}; }};
  const existing = worker({windows: [open]});
  for (const [action, expected] of [["approve", url + "&answer=approve"], ["deny", url + "&answer=deny"], ["reply", url + "&answer=reply"], ["", url], [undefined, url]]) {
    const {event, finished} = waitUntil();
    event.action = action;
    event.notification = {close() {}, data: {url}};
    existing.handlers.notificationclick(event);
    await finished();
    assert.equal(navigated.at(-2), expected);
    assert.equal(navigated.at(-1), "focused");
  }
  assert.equal(existing.opened.length, 0);
  const fresh = worker();
  const {event, finished} = waitUntil();
  event.action = "approve";
  event.notification = {close() {}, data: {url: "https://tiny.example/approvals"}};
  fresh.handlers.notificationclick(event);
  await finished();
  assert.deepEqual(fresh.opened, ["https://tiny.example/approvals?answer=approve"]);
  const odd = waitUntil();
  odd.event.action = "../x";
  odd.event.notification = {close() {}, data: {url}};
  fresh.handlers.notificationclick(odd.event);
  await odd.finished();
  assert.equal(fresh.opened.at(-1), url);
});
