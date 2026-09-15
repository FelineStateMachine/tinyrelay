import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("tinyclient/chat-activity.js", "utf8");
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

function nativeForm(selection="single", interaction="question", freeform=true) {
  const {definitions, document, template, published} = setup();
  const host = new (definitions["chat-activity"])(); host.localName="chat-activity"; host.parent=document;
  host.read=async()=>"fresh"; host.schedule=()=>{};
  const allowed=new Node("chat-decision"), decision=new (definitions["chat-decision"])();
  decision.localName="chat-decision"; decision.parent=host;
  for(const el of [allowed,decision]) {
    for(const [name,value] of [["event",request],["actor",actor],["pubkey",author],["room","room"],["kind","9"],["native",""],["selection",selection],["interaction",interaction]]) el.setAttribute(name,value);
    if(interaction==="question")el.setAttribute("question","");
    if(freeform)el.setAttribute("freeform","");
    if(selection==="multiple")el.setAttribute("multiple","");
  }
  allowed.querySelectorAll=()=>selection==="text"?[]:[{value:"c0"},{value:"c1"}];
  template.content={querySelectorAll:()=>[allowed]};
  return {decision,allowed,published};
}

for(const [selection,form,expected] of [
  ["single",{option:{value:"c0",checked:true}},"c0"],
  ["multiple",{option:[{value:"c0",checked:true},{value:"c1",checked:true}]},'["c0","c1"]'],
  ["single",{"custom-answer":{value:"custom reply"}},'{"text":"custom reply"}'],
  ["text",{"custom-answer":{value:"plain reply"}},"plain reply"],
  ["single",{option:{value:"c1",checked:true},"custom-answer":{value:" The question is in the wrong thread. "}},'{"choices":["c1"],"text":"The question is in the wrong thread."}'],
  ["multiple",{option:[{value:"c0",checked:true},{value:"c1",checked:true}],"custom-answer":{value:"Both apply."}},'{"choices":["c0","c1"],"text":"Both apply."}'],
]) test(`native ${selection} question submits the selected or custom answer`,async()=>{
  const {decision,published}=nativeForm(selection);
  await decision.submit({elements:form});
  assert.equal(published.length,1); assert.equal(published[0].content,expected); assert.equal(published[0].kind,1111);
});

test("native approval accepts only an offered option",async()=>{
  const {decision,published}=nativeForm("single","approval",false);
  await assert.rejects(decision.submit({elements:{"custom-answer":{value:"yes"}}}),/options only/);
  await assert.rejects(decision.submit({elements:{option:{value:"always",checked:true}}}),/options changed/);
  await decision.submit({elements:{option:{value:"c0",checked:true}}});
  assert.equal(published[0].content,"c0");
});

test("native decisions validate freeform and selection against the fresh card",async()=>{
  const {decision,allowed,published}=nativeForm();
  delete allowed.attributes.freeform;
  await assert.rejects(decision.submit({elements:{"custom-answer":{value:"custom"}}}),/options only/);
  await assert.rejects(decision.submit({elements:{option:[{value:"c0",checked:true},{value:"c1",checked:true}]}}),/one option/);
  assert.equal(published.length,0);
});

test("an explanation cannot bypass the single-choice limit or offered choices",async()=>{
  const {decision,published}=nativeForm();
  const comment={value:"Here is why."};
  await assert.rejects(decision.submit({elements:{option:[{value:"c0",checked:true},{value:"c1",checked:true}],"custom-answer":comment}}),/one option/);
  await assert.rejects(decision.submit({elements:{option:{value:"unknown",checked:true},"custom-answer":comment}}),/options changed/);
  assert.equal(published.length,0);
});

test("combined answers still require freeform permission on the current question",async()=>{
  for(const interaction of ["question","approval","confirmation"]) {
    const {decision,published}=nativeForm("single",interaction,false);
    await assert.rejects(decision.submit({elements:{option:{value:"c1",checked:true},"custom-answer":{value:"An explanation."}}}),/options only/);
    assert.equal(published.length,0);
  }
});

// The request form and the cancel button share the host, the signer check
// and the publish path with decisions; these cover the events they sign.
const agent = "d".repeat(64), second = "e".repeat(64), root = "f".repeat(64);
function requestForm({root: thread = null} = {}) {
  const {definitions, document, published, signer} = setup();
  const host = new (definitions["chat-activity"])(); host.localName = "chat-activity"; host.parent = document;
  host.refresh = async force => { host.refreshed = force; };
  const request = new (definitions["chat-request"])(); request.localName = "chat-request"; request.parent = host;
  for (const [name, value] of [["room", "room"], ["actor", actor]]) request.setAttribute(name, value);
  if (thread) request.setAttribute("root", thread);
  return {request, host, published, signer};
}

test("request publishes a kind 43001 with room, agent, subject and thread root tags", async () => {
  const {request, host, published} = requestForm({root});
  const form = {elements: {agent: {value: agent.toUpperCase()}, subject: {value: " Build the preview "}, content: {value: " Use the dark palette. "}}, reset() { this.wasReset = true; }};
  await request.submit(form);
  assert.equal(published.length, 1);
  assert.equal(published[0].kind, 43001);
  assert.deepEqual(JSON.parse(JSON.stringify(published[0].tags)), [["h", "room"], ["p", agent], ["subject", "Build the preview"], ["e", root, "", "root"]]);
  assert.equal(published[0].content, "Use the dark palette.");
  assert.equal(typeof published[0].created_at, "number");
  assert.equal(form.wasReset, true);
  assert.equal(host.refreshed, true);
  assert.equal(request.message, "Task sent.");
});

test("request outside a thread omits subject and root tags and validates its fields", async () => {
  const {request, published, signer} = requestForm();
  await request.submit({elements: {agent: {value: agent}, subject: {value: ""}, content: {value: "Transcribe the call."}}});
  assert.deepEqual(JSON.parse(JSON.stringify(published[0].tags)), [["h", "room"], ["p", agent]]);
  await assert.rejects(request.submit({elements: {agent: {value: "not-a-key"}, content: {value: "x"}}}), /public key/);
  await assert.rejects(request.submit({elements: {agent: {value: agent}, subject: {value: " "}, content: {value: " "}}}), /Write the task/);
  await assert.rejects(request.submit({elements: {agent: {value: agent}, subject: {value: "s".repeat(201)}, content: {value: "x"}}}), /subject is too long/);
  signer.getPublicKey = async () => author;
  await assert.rejects(request.submit({elements: {agent: {value: agent}, content: {value: "x"}}}), /Connect the signer/);
  assert.equal(published.length, 1);
});

function cancelButton(listing) {
  const {definitions, document, template, published} = setup();
  const host = new (definitions["chat-activity"])(); host.localName = "chat-activity"; host.parent = document;
  host.read = async () => "fresh"; host.schedule = () => { host.scheduled = true; }; host.refresh = async force => { host.refreshed = force; };
  template.content = {querySelectorAll: selector => selector === "chat-cancel" ? listing : []};
  const cancel = new (definitions["chat-cancel"])(); cancel.localName = "chat-cancel"; cancel.parent = host;
  for (const [name, value] of [["event", request], ["room", "room"], ["actor", actor], ["assignees", agent + " " + second]]) cancel.setAttribute(name, value);
  return {cancel, host, published};
}

test("cancel revalidates the open card and publishes a kind 43005 naming each assignee", async () => {
  const allowed = new Node("chat-cancel");
  for (const [name, value] of [["event", request], ["room", "room"], ["actor", actor], ["assignees", agent + " " + second + " short"]]) allowed.setAttribute(name, value);
  const {cancel, host, published} = cancelButton([allowed]);
  await cancel.submit({elements: {}});
  assert.equal(published.length, 1);
  assert.equal(published[0].kind, 43005);
  assert.deepEqual(JSON.parse(JSON.stringify(published[0].tags)), [["e", request], ["h", "room"], ["p", agent], ["p", second]]);
  assert.equal(published[0].content, "");
  assert.equal(host.refreshed, true);
  assert.equal(cancel.message, "Task cancelled.");
});

test("cancel refuses a task that is no longer open for this account", async () => {
  const stale = new Node("chat-cancel");
  for (const [name, value] of [["event", request], ["room", "room"], ["actor", author]]) stale.setAttribute(name, value);
  const {cancel, host, published} = cancelButton([stale]);
  await assert.rejects(cancel.submit({elements: {}}), /no longer open/);
  assert.equal(host.scheduled, true);
  assert.equal(published.length, 0);
});
