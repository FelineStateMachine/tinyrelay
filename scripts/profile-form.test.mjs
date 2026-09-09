// The profile form merges the fields it shows over the profile the relay
// holds, signs a kind 0, publishes it here and then to the listed relays.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, getPublicKey, verifyEvent } from "nostr-tools/pure";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const formSource = source.slice(source.indexOf("  // ProfileForm publishes"), source.indexOf("  // AgentGrant signs"));
const plain = value => JSON.parse(JSON.stringify(value));

function setup({attributes = {}, result, pool, noSigner = false, otherKey = false} = {}) {
  const secret = generateSecretKey(), pubkey = getPublicKey(secret);
  const sent = [], published = [], reports = [];
  class FormElement {
    getAttribute(name) { return attributes[name] ?? (name === "pubkey" ? (otherKey ? "f".repeat(64) : pubkey) : null); }
    report(text, error) { reports.push({text, error: Boolean(error)}); }
  }
  const store = {};
  const localStorage = {getItem: key => store[key] ?? null, setItem: (key, value) => { store[key] = value; }};
  const window = {
    nostr: noSigner ? undefined : {signEvent: async event => finalizeEvent(structuredClone(event), secret)},
    NostrSigner: {verifyEvent, SimplePool: class { publish(relays, event) { published.push({relays, event}); return relays.map(relay => (pool?.[relay] === "fail" ? Promise.reject(Error("closed")) : Promise.resolve("ok"))); } close() {} }},
    tinyNames: {cacheKey: "tiny:names:v1"}
  };
  const tiny = {
    util: {relayURL: value => (/^wss?:\/\/[a-z0-9.-]+(?::\d+)?\/?$/i.test(String(value).trim()) ? String(value).trim().replace(/\/$/, "") : null)},
    signedFetch: async (path, method, body) => { sent.push({path, method, event: JSON.parse(body)}); return Response.json(result ?? {accepted: true}); }
  };
  const Constructor = vm.runInNewContext(`${formSource}\nProfileForm`, {FormElement, window, tiny, localStorage, JSON, Date, Error, Promise, setTimeout, Object, Array, String});
  const form = values => ({elements: Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value}]))});
  return {Constructor, component: new Constructor(), form, sent, published, reports, pubkey, store};
}

test("content merges shown fields over the held profile and validates links", () => {
  const {Constructor} = setup();
  const merged = Constructor.content({name: " dami ", display_name: "", about: "hi", picture: "https://x/p.png"}, JSON.stringify({name: "old", display_name: "Old Name", website: "https://kept.example", custom: {deep: true}}));
  assert.deepEqual(plain(merged), {name: "dami", about: "hi", picture: "https://x/p.png", website: "https://kept.example", custom: {deep: true}});
  assert.deepEqual(plain(Constructor.content({name: "x"}, "not json")), {name: "x"});
  assert.throws(() => Constructor.content({picture: "javascript:alert(1)"}, "{}"), /http or https/);
});

test("relays parse from any separator, dedupe and cap at twelve", () => {
  const {Constructor} = setup();
  assert.deepEqual(plain(Constructor.relays("wss://a.example/\nwss://b.example, wss://a.example ftp://no.example")), ["wss://a.example", "wss://b.example"]);
  assert.throws(() => Constructor.relays(Array.from({length: 13}, (_, i) => "wss://r" + i + ".example").join(" ")), /at most 12/);
});

test("submit publishes here first, then to the listed relays, and forgets the cached name", async () => {
  const s = setup({pool: {"wss://b.example": "fail"}});
  s.store["tiny:names:v1"] = JSON.stringify({[s.pubkey]: {event: null, at: 1}, other: {at: 2}});
  await s.component.submit(s.form({name: "dami", about: "", relays: "wss://a.example\nwss://b.example"}));
  assert.equal(s.sent.length, 1);
  assert.equal(s.sent[0].path, "/events");
  const event = s.sent[0].event;
  assert.equal(event.kind, 0);
  assert.deepEqual(plain(JSON.parse(event.content)), {name: "dami"});
  assert.equal(verifyEvent(event), true);
  assert.deepEqual(plain(s.published[0].relays), ["wss://a.example", "wss://b.example"]);
  assert.equal(s.published[0].event.id, event.id);
  const last = s.reports.at(-1).text.split("\n");
  assert.deepEqual(last, ["Published here.", "wss://a.example: ok", "wss://b.example: failed, closed"]);
  assert.deepEqual(Object.keys(JSON.parse(s.store["tiny:names:v1"])), ["other"]);
});

test("submit refuses without a signer, with another key, or when the relay rejects", async () => {
  await assert.rejects(setup({noSigner: true}).component.submit(setup().form({name: "x"})), /Connect a signer/);
  const other = setup({otherKey: true});
  await assert.rejects(other.component.submit(other.form({name: "x"})), /different key/);
  assert.equal(other.sent.length, 0);
  const refused = setup({result: {accepted: false, message: "blocked: no"}});
  await assert.rejects(refused.component.submit(refused.form({name: "x"})), /blocked: no/);
  assert.equal(refused.published.length, 0);
});
