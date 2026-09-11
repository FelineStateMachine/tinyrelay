import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/chat-activity.js", "utf8");
const actor = "a".repeat(64), author = "b".repeat(64), request = "c".repeat(64);

class Node {
  constructor(tag = "div") { this.localName = tag; this.childNodes = []; this.attributes = {}; this.parent = null; }
  get isConnected() { return Boolean(this.parent); }
  append(...items) { for (const item of items) { if (item instanceof Node) item.parent = this; this.childNodes.push(item); } }
  replaceChildren(...items) { this.childNodes = []; this.append(...items); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  hasAttribute(name) { return Object.hasOwn(this.attributes, name); }
  querySelectorAll() { return []; }
  contains(node) { return node === this || this.childNodes.some(child => child instanceof Node && child.contains(node)); }
  closest(selector) { return selector === this.localName ? this : this.parent?.closest(selector) || null; }
  dispatchEvent(event) { this.listeners?.[event.type]?.(event); return true; }
  addEventListener(name, fn) { (this.listeners ||= {})[name] = fn; }
  removeEventListener(name) { delete this.listeners?.[name]; }
}

function setup() {
  const document = new Node("document"); document.hidden = false; document.activeElement = null;
  const definitions = {}, events = {};
  document.addEventListener = (name, fn) => (events[name] ||= []).push(fn);
  document.removeEventListener = () => {};
  const signer = {getPublicKey: async () => actor};
  const published = [];
  const tiny = {signer: () => signer, localPath: path => path, signing: {publish: async event => { published.push(event); return event; }},
    ui: {FormElement: class extends Node { report(message) { this.message = message; } connectedCallback() { this.form = {}; } }}};
  const template = new Node("template");
  document.createElement = tag => tag === "template" ? template : new Node(tag);
  const sandbox = {document, HTMLElement: Node, customElements: {define: (name, ctor) => definitions[name] = ctor}, tiny,
    location: {href: "https://relay.test/rooms/room", origin: "https://relay.test"}, URL, AbortController, setInterval: () => 1,
    clearInterval: () => {}, setTimeout, clearTimeout, queueMicrotask, CustomEvent: class { constructor(type,opts){this.type=type;Object.assign(this,opts);} }, morph: (target,html)=>{target.morphed=html;}, fetch: async () => ({ok: true, text: async () => ""}), Date, JSON, Error};
  vm.runInNewContext(source, sandbox);
  return {document, definitions, tiny, signer, published, template};
}

test("Fixi refresh preserves a draft focused while the request was in flight", async () => {
  const {definitions, document} = setup();
  const host = new (definitions["chat-activity"])(); host.parent = document;
  const original = new Node("textarea"); host.append(original); host.connectedCallback();
  const cfg={trigger:{target:host},headers:{},abort(){},response:{ok:true},target:new Node(),text:"new cards"};
  host.onConfig({target:host,detail:{cfg}});
  assert.equal(cfg.cache,"no-store");assert.equal(cfg.credentials,"same-origin");assert.equal(cfg.transition,false);
  document.activeElement=original;cfg.swap();assert.equal(cfg.target.morphed,undefined);
  document.activeElement=null;cfg.swap();assert.equal(cfg.target.morphed,"new cards");
  host.disconnectedCallback();
});

test("Fixi swap preserves details and ignores failed responses and logout", () => {
  const {definitions,document}=setup(),host=new (definitions["chat-activity"])();host.parent=document;
  const open={dataset:{task:"first"}},first={dataset:{task:"first"}},second={dataset:{task:"second"}};
  host.querySelectorAll=()=>[open];
  const target={querySelectorAll:selector=>selector.startsWith("details")?[first,second]:[]};
  const cfg={target,text:"cards",response:{ok:false}};host.swap(cfg);assert.equal(target.morphed,undefined);
  cfg.response.ok=true;host.swap(cfg);assert.equal(first.open,true);assert.equal(second.open,false);
  host.stopped=true;cfg.text="logout";host.swap(cfg);assert.equal(target.morphed,"cards");
});

test("decision revalidates the current card before signing and emits a NIP-22 answer", async () => {
  const {definitions, document, template, published} = setup();
  const Decision = definitions["chat-decision"], host = new (definitions["chat-activity"])(); host.localName = "chat-activity"; host.parent = document;
  host.read = async () => "fresh"; host.schedule = () => { host.scheduled = true; };
  const allowed = new Node("chat-decision"); allowed.setAttribute("event", request); allowed.setAttribute("actor", actor); allowed.setAttribute("pubkey", author);
  template.content = {querySelectorAll: selector => selector === "chat-decision" ? [allowed] : []};
  const decision = new Decision(); decision.localName = "chat-decision"; decision.parent = host;
  for (const [name, value] of [["event", request], ["actor", actor], ["pubkey", author], ["room", "room"], ["kind", "5000"], ["question", ""]]) decision.setAttribute(name, value);
  decision.submitter = {value: "?"};
  const form = {elements: {content: {value: " answer "}}};
  await decision.submit(form);
  assert.equal(published.length, 1);
  assert.equal(published[0].kind, 1111);
  assert.deepEqual(JSON.parse(JSON.stringify(published[0].tags)), [["h", "room"], ["E", request, "", author], ["K", "5000"], ["P", author], ["e", request, "", author], ["k", "5000"], ["p", author]]);
  assert.equal(published[0].content, "answer");
});

test("decision refuses a card that disappeared while it was being answered", async () => {
  const {definitions, document, template} = setup();
  const Decision = definitions["chat-decision"], host = new (definitions["chat-activity"])(); host.localName = "chat-activity"; host.parent = document;
  host.read = async () => "gone"; host.schedule = () => { host.scheduled = true; };
  template.content = {querySelectorAll: () => []};
  const decision = new Decision(); decision.localName = "chat-decision"; decision.parent = host;
  for (const [name, value] of [["event", request], ["actor", actor], ["pubkey", author], ["room", "room"], ["kind", "5000"]]) decision.setAttribute(name, value);
  decision.submitter = {value: "+"};
  await assert.rejects(decision.submit({elements: {}}), /no longer waiting/);
  assert.equal(host.scheduled, true);
});
