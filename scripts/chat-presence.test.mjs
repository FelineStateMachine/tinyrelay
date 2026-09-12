import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/chat-presence.js", "utf8");
const key = "a".repeat(64), other = "b".repeat(64);

class Node {
  constructor(tag = "div") { this.localName = tag; this.childNodes = []; this.attributes = {}; this.parent = null; }
  get isConnected() { return Boolean(this.parent); }
  append(...items) { for (const item of items) { if (item instanceof Node) { item.parent = this; } this.childNodes.push(item); } }
  replaceChildren(...items) { this.childNodes = []; this.append(...items); }
  get textContent() { return this.childNodes.map(item => item instanceof Node ? item.textContent : String(item)).join(""); }
  set textContent(value) { this.childNodes = value ? [String(value)] : []; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  querySelector(selector) { return this.childNodes.find(item => item.localName === selector) || null; }
  get children() { return this.childNodes.filter(item => item instanceof Node); }
  remove() { if(this.parent) this.parent.childNodes = this.parent.childNodes.filter(item=>item!==this); this.parent=null; }
  insertBefore(item,before) { item.remove(); const index=before ? this.childNodes.indexOf(before) : this.childNodes.length; this.childNodes.splice(index,0,item); item.parent=this; }
  addEventListener(name, fn) { (this.listeners ||= {})[name] = fn; }
  removeEventListener(name) { delete this.listeners?.[name]; }
  matches(selector) { return selector === this.localName; }
  appendChild(item) { this.append(item); return item; }
}

function setup(options={}) {
  const document = new Node("document");
  document.hidden = false;
  document.createElement = tag => new Node(tag);
  document.addEventListener = (name, fn) => (document.listeners ||= {})[name] = fn;
  document.removeEventListener = name => delete document.listeners?.[name];
  const definitions = {}, saved = [], dispatched = [];
  document.dispatchEvent = event => dispatched.push(event);
  const tiny = {signedFetch: async (path,method,body,settings) => {saved.push({path,method,body,settings}); if(options.saveError) throw Error(options.saveError); return {json:async()=>JSON.parse(body)};},localPath:path=>path,signer: () => signer, signing: {publish: async event => { if (options.publishGate) await options.publishGate.promise; if (options.publishError) { const error = options.publishError; options.publishError = null; throw Error(error); } published.push(event); }}};
  const published = [];
  const signer = {getPublicKey: async () => key, signEvent: async () => ({})};
  class HTMLElement extends Node {}
  const sandbox = {document, HTMLElement, customElements: {define: (name, ctor) => definitions[name] = ctor}, tiny,
    AbortController,CustomEvent:class {constructor(type,options){this.type=type;this.detail=options.detail;}},fetch:options.fetch || (async()=>({ok:true,json:async()=>({share_presence:options.share === true})})),setInterval: () => 1, clearInterval: () => {}, setTimeout, clearTimeout, Date, JSON, String, Number, Boolean, Set, Map, Object};
  vm.runInNewContext(source, sandbox);
  return {document, definitions, tiny, published, signer, saved, dispatched};
}

const event = (kind, pubkey, created_at, tags, content = "online") => ({kind, pubkey, created_at, tags, content});

test("presence parser enforces room scope, TTL and typing precedence", () => {
  const {tiny} = setup();
  const {parse, PRESENCE, TYPING} = tiny.roomPresence;
  assert.equal(parse(event(PRESENCE, other, 100, [["h", "other"]]), "room", 100), null);
  assert.equal(parse(event(TYPING, other, 100, [], "typing"), "room", 100), null);
  assert.equal(parse(event(PRESENCE, other, 100, [["h", "room"]]), "room", 281), null);
  assert.equal(parse(event(TYPING, other, 100, [["h", "room"]], "typing"), "room", 106), null);
  assert.equal(parse(event(PRESENCE, other, 100, [], '{"status":"away"}'), "room", 100).status, "away");
  assert.equal(parse(event(TYPING, other, 100, [["h", "room"]], '{"typing":false}'), "room", 100).status, "stopped");
});

test("presence ignores stale events and keeps typing ahead of presence", () => {
  const {definitions, document} = setup();
  const Presence = definitions["chat-presence"];
  const host = new Presence(); host.setAttribute("room", "room"); host.setAttribute("actor", key);
  const output = new Node("output"); host.append(output); host.parent = document;
  host.connectedCallback();
  const now = Math.floor(Date.now() / 1000);
  host.receive(event(20001, other, now, [["h", "room"]], "away"));
  host.receive(event(20001, other, now - 1, [["h", "room"]], "online"));
  assert.match(output.textContent, /away/);
  host.receive(event(20002, other, now, [["h", "room"]], "typing"));
  assert.match(output.textContent, /typing/);
  assert.doesNotMatch(output.textContent, /away/);
  host.disconnectedCallback();
});

test("presence publishing is opt in and always includes the room tag", async () => {
  const {definitions, document, published} = setup();
  const Presence = definitions["chat-presence"];
  const host = new Presence(); host.setAttribute("room", "room"); host.setAttribute("actor", key);
  const output = new Node("output");
  host.append(output); host.parent = document; host.connectedCallback();
  await host.publish(20001, "online");
  assert.equal(published.length, 0);
  await new Promise(resolve=>setTimeout(resolve,0));
  host.applyPreference(true);
  await host.publish(20001, "online");
  assert.deepEqual(JSON.parse(JSON.stringify(published[0].tags)), [["h", "room"]]);
  assert.equal(published[0].kind, 20001);
  host.disconnectedCallback();
});

test("heartbeat repaint preserves resolved names and each user's status node", () => {
  const {definitions, document} = setup();
  const host = new definitions["chat-presence"](); host.setAttribute("room", "room"); host.setAttribute("actor", key);
  const output = new Node("output"); host.append(output); host.parent = document; host.connectedCallback();
  const now = Math.floor(Date.now()/1000), third = "c".repeat(64);
  host.receive(event(20001, other, now, [["h","room"]]));
  const row = output.childNodes[0], name = row.querySelector?.("nostr-name") || row;
  name.textContent = "Alice";
  for(let i=0;i<5;i++) host.paint();
  assert.equal(output.childNodes[0], row, "unchanged heartbeat replaced the name element");
  assert.match(output.textContent, /Alice/);
  host.receive(event(20001, third, now, [["h","room"]], "working"));
  host.receive(event(20002, third, now, [["h","room"]], "typing"));
  assert.equal(output.childNodes[0],row,"another user's update replaced Alice");
  assert.match(output.textContent,/Alice/);
  host.receive(event(20001,other,now+1,[["h","room"]],"offline"));
  host.receive(event(20001,other,now,[["h","room"]],"online"));
  assert.doesNotMatch(output.textContent,/Alice/,"late heartbeat resurrected an offline user");
  host.disconnectedCallback();
});


test("saved account preference enables chat without a chat control, and respects signer identity", async () => {
  const {definitions,document,published,tiny} = setup({share:true});
  const make = () => { const host=new definitions["chat-presence"]();host.setAttribute("room","room");host.setAttribute("actor",key);host.append(new Node("output"));host.parent=document;host.connectedCallback();return host; };
  const host=make(); await new Promise(resolve=>setTimeout(resolve,0));
  assert.equal(host.querySelector("input"),null);
  assert.equal(published.length,1); assert.equal(published[0].kind,20001);
  host.disconnectedCallback(); await new Promise(resolve=>setTimeout(resolve,0));
  published.length=0; tiny.signer=()=>({signEvent:async()=>({}),getPublicKey:async()=>other});
  const otherAccount=make(); await new Promise(resolve=>setTimeout(resolve,0));
  assert.equal(published.length,0); otherAccount.disconnectedCallback();
});

test("signer recovery clears a transient presence failure without a reload", async () => {
  const options = {share:true, publishError:"Signer connection changed. Retry when connected."};
  const {definitions, document, published} = setup(options);
  const host = new definitions["chat-presence"](); host.setAttribute("room","room"); host.setAttribute("actor",key);
  host.append(new Node("output")); host.parent = document; host.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.match(host.status.textContent, /Presence could not be shared/);
  assert.equal(host.failed, true);
  document.listeners["tiny:signer-status"]({detail:{status:"ready"}});
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(host.failed, false);
  assert.equal(host.status.textContent, "");
  await host.publish(20001, "online");
  assert.equal(published.length, 1);
  host.disconnectedCallback();
});

test("a presence failure from before signer recovery cannot restore the warning", async () => {
  let resolve, reject;
  const options = {share:false, publishGate:{promise:new Promise((done, fail) => { resolve = done; reject = fail; })}};
  const {definitions, document} = setup(options);
  const host = new definitions["chat-presence"](); host.setAttribute("room","room"); host.setAttribute("actor",key);
  host.append(new Node("output")); host.parent = document; host.connectedCallback(); host.enabled = true;
  const pending = host.publish(20001, "online");
  document.listeners["tiny:signer-status"]({detail:{status:"ready"}});
  reject(Error("Signer connection changed. Retry when connected."));
  await pending;
  assert.notEqual(host.failed, true);
  assert.equal(host.status.textContent, "");
  resolve();
  host.disconnectedCallback();
});

test("a failed hidden cleanup announcement does not create a signer warning", async () => {
  const {definitions, document} = setup({publishError:"Signer connection changed. Retry when connected."});
  const host = new definitions["chat-presence"](); host.setAttribute("room","room"); host.setAttribute("actor",key);
  host.append(new Node("output")); host.parent = document; host.connectedCallback();
  host.last[20001] = 1; document.hidden = true;
  await host.publish(20001, "offline", true);
  assert.notEqual(host.failed, true);
  assert.equal(host.status.textContent, "");
  host.disconnectedCallback();
});

test("a ready signer event queues preference reload after an in-flight request", async () => {
  let resolveFirst, calls = 0;
  const first = new Promise(resolve => { resolveFirst = resolve; });
  const {definitions, document} = setup({fetch: async () => ++calls === 1 ? first : {ok:true,json:async()=>({share_presence:true})}});
  const host = new definitions["chat-presence"](); host.setAttribute("room","room"); host.setAttribute("actor",key);
  host.append(new Node("output")); host.parent = document; host.connectedCallback();
  document.listeners["tiny:signer-status"]({detail:{status:"ready"}});
  document.listeners["tiny:signer"]({detail:{}});
  assert.equal(host.preferenceReload, true);
  resolveFirst({ok:true,json:async()=>({share_presence:true})});
  await new Promise(resolve => setTimeout(resolve, 0));
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(calls, 2);
  assert.equal(host.enabled, true);
  host.disconnectedCallback();
});

test("a failed visible opt-out announcement does not create a signer warning", async () => {
  const {definitions, document} = setup({publishError:"Signer connection changed. Retry when connected."});
  const host = new definitions["chat-presence"](); host.setAttribute("room","room"); host.setAttribute("actor",key);
  host.append(new Node("output")); host.parent = document; host.connectedCallback();
  host.last[20001] = 1; await host.publish(20001, "offline", true);
  assert.notEqual(host.failed, true);
  assert.equal(host.status.textContent, "");
  host.disconnectedCallback();
});

test("account preference saves with a signed JSON request and restores checkbox after failure", async () => {
  const options={}; const {definitions,document,saved,dispatched}=setup(options);
  const host=new definitions["chat-preferences"]();host.setAttribute("actor",key);host.parent=document;
  const input=new Node("input"), output=new Node("output");host.append(input,output);host.connectedCallback();
  await new Promise(resolve=>setTimeout(resolve,0));
  assert.equal(input.checked,false);assert.equal(input.disabled,false);
  input.checked=true;await host.save();
  assert.equal(saved.length,1);assert.equal(saved[0].path,"/chat/preferences");assert.equal(saved[0].method,"POST");
  assert.deepEqual(JSON.parse(saved[0].body),{share_presence:true});assert.equal(saved[0].settings.contentType,"application/json");
  assert.equal(dispatched[0].detail.actor,key);assert.equal(dispatched[0].detail.share_presence,true);
  options.saveError="Signing refused";input.checked=false;await host.save();
  assert.equal(input.checked,true);assert.match(output.textContent,/Signing refused/);
  host.disconnectedCallback();
});

test("a preference response arriving after logout cannot enable presence", async () => {
  let resolve;const {definitions,document,published}=setup({fetch:()=>new Promise(done=>{resolve=done;})});
  const host=new definitions["chat-presence"]();host.setAttribute("room","room");host.setAttribute("actor",key);host.append(new Node("output"));host.parent=document;host.connectedCallback();
  host.onIdentity();resolve({ok:true,json:async()=>({share_presence:true})});
  await new Promise(done=>setTimeout(done,0));
  assert.equal(host.enabled,false);assert.equal(published.length,0);host.disconnectedCallback();
});
