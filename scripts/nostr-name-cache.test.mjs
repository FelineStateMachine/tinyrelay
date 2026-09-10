import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("scripts/nostr-name-setup.mjs", "utf8")
  .replace(/^import .*\n/gm, "");
const key = "a".repeat(64);

function setup(initial, events) {
  const store = {"tiny:names:v1": JSON.stringify(initial)};
  let queries = 0;
  const pool = {querySync: async () => { queries++; return events; }};
  const window = {nostrSharedPool: pool};
  const context = {
    window, localStorage: {getItem: key => store[key] ?? null, setItem: (key, value) => { store[key] = value; }},
    location: {pathname: "/rooms/x", protocol: "https:", host: "tiny.example"},
    SimplePool: class {},
    bareNostrUser: pubkey => ({pubkey, image: undefined}),
    nostrUserFromEvent: event => ({pubkey: event.pubkey, image: JSON.parse(event.content).picture}),
    JSON, Date, Promise, String, RegExp, Math, setTimeout, clearTimeout
  };
  vm.runInNewContext(source, context);
  return {load: window.tinyNames.load, queries: () => queries};
}

const old = {pubkey: key, created_at: 1, content: JSON.stringify({name: "old"})};
const fresh = {pubkey: key, created_at: 2, content: JSON.stringify({name: "new", picture: "https://cdn.example/slate.png"})};

test("refresh bypasses an old image-less cached profile once", async () => {
  const {load, queries} = setup({[key]: {event: old, at: Math.floor(Date.now() / 1000)}}, [fresh]);
  const user = await load({pubkey: key, refresh: true});
  assert.equal(user.image, "https://cdn.example/slate.png");
  assert.equal(queries(), 1);
});

test("concurrent refresh callers share one batch and later refreshes use it", async () => {
  const {load, queries} = setup({[key]: {event: old, at: Math.floor(Date.now() / 1000)}}, [fresh]);
  const users = await Promise.all(Array.from({length: 8}, () => load({pubkey: key, refresh: true})));
  assert.equal(queries(), 1);
  assert.ok(users.every(user => user.image === "https://cdn.example/slate.png"));
  assert.equal((await load({pubkey: key, refresh: true})).image, "https://cdn.example/slate.png");
  assert.equal(queries(), 1);
});

test("normal loads continue using a fresh cached profile without querying", async () => {
  const {load, queries} = setup({[key]: {event: fresh, at: Math.floor(Date.now() / 1000)}}, []);
  assert.equal((await load({pubkey: key})).image, "https://cdn.example/slate.png");
  assert.equal(queries(), 0);
});
