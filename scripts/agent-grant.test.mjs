import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import {finalizeEvent, generateSecretKey, getPublicKey, nip19, verifyEvent} from "nostr-tools";

const owner = generateSecretKey();
const ownerPubkey = getPublicKey(owner);
const repoOwner = "b".repeat(64);
const source = fs.readFileSync("internal/webui/components.js", "utf8");
const tinySource = fs.readFileSync("internal/webui/tiny.js", "utf8");
const grantSource = source.slice(source.indexOf("  class AgentGrant "), source.indexOf("  class NostrKey "));
const today = new Date();
const dateAfter = days => new Date(today.getTime() + days * 86400000).toISOString().slice(0, 10);

function setup(values = {}, options = {}) {
  const sent = [], signed = [], appended = [];
  let resets = 0, navigated = 0;
  class FormElement {
    getAttribute() { return null; }
    report(message) { this.message = message; }
    append(...nodes) { appended.push(...nodes); }
  }
  const el = (name, text) => ({name, text, children: [], attributes: {}, append(...nodes) { this.children.push(...nodes); }, setAttribute(key, value) { this.attributes[key] = value; }, addEventListener() {}});
  const tinySandbox = {globalThis: {}, crypto: {}, TextEncoder, URL};
  vm.runInNewContext(tinySource, tinySandbox);
  const window = {
    NostrSigner: {verifyEvent, generateSecretKey, getPublicKey},
    nostr: {signEvent: async event => {
      signed.push(structuredClone(event));
      if (options.change) event.tags.push(["k", "7"]);
      return finalizeEvent(structuredClone(event), options.signer || owner);
    }}
  };
  const tiny = {
    util: tinySandbox.globalThis.tiny.util,
    navigate: async () => { navigated++; },
    signedFetch: async (path, method, body) => {
      sent.push({path, method, event: JSON.parse(body)});
      return Response.json(options.result ?? {accepted: true});
    }
  };
  const Constructor = vm.runInNewContext(`${grantSource}\nAgentGrant`, {FormElement, window, tiny, el, URL, isHex64: value => /^[0-9a-f]{64}$/.test(value || ""), globalThis: {location: {pathname: "/manage/agents"}}});
  const component = new Constructor();
  const form = {elements: Object.fromEntries(Object.entries({key: "generate", name: "helper", expires: dateAfter(90), rooms: "", repos: "", kinds: "", wiki: "", rate: "60", pubkey: "", ...values}).map(([key, value]) => [key, {value}])), reset: () => { resets++; }};
  return {Constructor, component, form, sent, signed, appended, resets: () => resets, navigated: () => navigated};
}

test("a generated key yields a signed grant, an nsec shown once and no reload", async () => {
  const s = setup({rooms: "build, agents", repos: `${repoOwner}:tinyrelay:maintain\n${repoOwner}:docs:read`, kinds: "9, 1111, 1621", wiki: "propose", rate: "30"});
  await s.component.submit(s.form);
  assert.equal(s.sent.length, 1);
  const {event} = s.sent[0];
  assert.equal(s.sent[0].path, "/events");
  assert.equal(event.kind, 30392);
  assert.equal(event.pubkey, ownerPubkey);
  assert.equal(verifyEvent(event), true);
  const agent = event.tags[0][1];
  assert.match(agent, /^[0-9a-f]{64}$/);
  assert.notEqual(agent, ownerPubkey);
  const expiration = Number(event.tags.find(tag => tag[0] === "expiration")[1]);
  const now = Math.floor(Date.now() / 1000);
  assert.ok(expiration > now + 89 * 86400 && expiration <= now + 91 * 86400, "expiration is about 90 days out");
  assert.deepEqual(event.tags.filter(tag => tag[0] !== "expiration"), [
    ["d", agent], ["p", agent], ["name", "helper"],
    ["room", "build"], ["room", "agents"],
    ["repo", `${repoOwner}:tinyrelay:maintain`], ["repo", `${repoOwner}:docs:read`],
    ["k", "9"], ["k", "1111"], ["k", "1621"],
    ["wiki", "propose"], ["rate", "30"]
  ]);
  assert.equal(event.content, "");
  // The secret is shown once, in a read-only field, and matches the agent key.
  const secretInput = s.appended.flatMap(node => node.children || []).find(node => typeof node.value === "string" && node.value.startsWith("nsec1"));
  assert.ok(secretInput, "nsec field appended");
  assert.equal(secretInput.readOnly, true);
  const decoded = nip19.decode(secretInput.value);
  assert.equal(decoded.type, "nsec");
  assert.equal(getPublicKey(decoded.data), agent);
  assert.ok(s.appended.some(node => node.attributes?.role === "alert"), "warning shown");
  assert.equal(s.navigated(), 0);
  assert.equal(s.resets(), 0);
  assert.equal(s.component.message, "Granted.");
});

test("a pasted public key signs the grant and reloads the page", async () => {
  const agent = "c".repeat(64);
  const s = setup({key: "paste", pubkey: " " + agent.toUpperCase() + " ", name: "", rate: ""});
  await s.component.submit(s.form);
  const {event} = s.sent[0];
  assert.deepEqual(event.tags.map(tag => tag[0]), ["d", "p", "expiration"]);
  assert.equal(event.tags[0][1], agent);
  assert.equal(s.appended.length, 0);
  assert.equal(s.navigated(), 1);
  assert.equal(s.resets(), 1);
});

test("invalid fields are refused before anything is signed", async () => {
  for (const [values, message] of [
    [{key: "paste", pubkey: "nope"}, /public key/],
    [{name: "x".repeat(65)}, /64 characters/],
    [{expires: dateAfter(-1)}, /after today/],
    [{expires: dateAfter(400)}, /365 days/],
    [{repos: "tinyrelay:maintain"}, /Repositories are/],
    [{repos: `${repoOwner}:tinyrelay:admin`}, /Repositories are/],
    [{kinds: "1, abc"}, /Kinds are/],
    [{kinds: "70000"}, /Kinds are/],
    [{wiki: "delete"}, /propose or edit/],
    [{rate: "0"}, /1 to 600/],
    [{rate: "601"}, /1 to 600/]
  ]) {
    const s = setup(values);
    await assert.rejects(s.component.submit(s.form), message);
    assert.equal(s.signed.length, 0);
    assert.equal(s.sent.length, 0);
  }
});

test("a changed event, a self grant and a relay refusal keep the form", async () => {
  const changed = setup({}, {change: true});
  await assert.rejects(changed.component.submit(changed.form), /invalid or changed/);
  assert.equal(changed.sent.length, 0);
  const refused = setup({}, {result: {accepted: false, message: "restricted: moderator required"}});
  await assert.rejects(refused.component.submit(refused.form), /moderator required/);
  assert.equal(refused.appended.length, 0);
  assert.equal(refused.resets(), 0);
});

test("tags dedupe rooms, repositories and kinds", () => {
  const {Constructor} = setup();
  const agent = "d".repeat(64);
  const tags = Constructor.tags({name: " bot ", expires: dateAfter(10), rooms: "a a, b", repos: `${repoOwner}:r:read\n${repoOwner}:r:maintain`, kinds: "1 1 2", wiki: "", rate: ""}, agent);
  assert.deepEqual(JSON.parse(JSON.stringify(tags)).filter(tag => tag[0] !== "expiration"), [["d", agent], ["p", agent], ["name", "bot"], ["room", "a"], ["room", "b"], ["repo", `${repoOwner}:r:read`], ["k", "1"], ["k", "2"]]);
});
