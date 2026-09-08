import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../internal/webui/blossom-manifests.js", import.meta.url), "utf8");
const sandbox = {TextEncoder, TextDecoder, Uint8Array, ArrayBuffer, crypto: (await import("node:crypto")).webcrypto};
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox);
const api = sandbox.TinyBlossomManifests;
const bytes = hex => Uint8Array.from(hex.match(/../g), x => parseInt(x, 16));
const hash = c => c.repeat(64);

test("matches the BUD-16 empty and single-entry vectors", async () => {
  const empty = {l: [], t: 2};
  assert.equal(Buffer.from(api.encodeManifest(empty)).toString("hex"), "82a16c90a17402");
  assert.equal(await api.manifestHash(empty), "0218ed9a4fbb0993757f17e5d08d089cb0c6ac851928ba1ba82d337d76c41c0c");
  const one = {l: [{h: bytes("ab".repeat(32)), n: "test.txt", s: 100, t: 0}], t: 2};
  assert.equal(await api.manifestHash(one), "16121fa792b3afc72ec8bfc1dc85060518b6adba1429973ecc12891165cbe67e");
  const decoded = api.decodeManifest(api.encodeManifest(one));
  assert.equal(decoded.t, 2);
  assert.equal(decoded.l[0].n, "test.txt");
  assert.equal(Buffer.from(decoded.l[0].h).toString("hex"), "ab".repeat(32));
});

test("matches the BUD-17 two-chunk vector and preserves chunk order", async () => {
  const node = {l: [{h: bytes("aa".repeat(32)), s: 100, t: 0}, {h: bytes("bb".repeat(32)), s: 50, t: 0}], t: 1};
  assert.equal(await api.manifestHash(node), "559b726c38295aa0ecbbaef43d438cc86dd63324a0c3e9426dc5f1d0285f483f");
  const decoded = api.decodeManifest(api.encodeManifest(node));
  assert.equal(decoded.t, 1);
  assert.equal(JSON.stringify(decoded.l.map(x => [Buffer.from(x.h).toString("hex"), x.s, x.t])), JSON.stringify([["aa".repeat(32), 100, 0], ["bb".repeat(32), 50, 0]]));
});

test("canonicalizes directory names by UTF-8 bytes and builds file manifests", async () => {
  const directory = await api.buildDirectory([
    {hash: hash("a"), name: "z", size: 2}, {hash: hash("b"), name: "a", size: 1}
  ]);
  assert.deepEqual(directory.l.map(x => x.n), ["a", "z"]);
  const details = await api.buildFile(Array.from({length: 175}, (_, i) => ({hash: hash((i % 10).toString()), size: 1})), {returnDetails: true});
  assert.equal(details.root.t, 1);
  assert.equal(details.root.l.length, 2);
  assert.equal(details.manifests.length, 3);
  assert.equal(details.root.l.reduce((n, x) => n + x.s, 0), 175);
});

test("builds typed directory fanout nodes with bounded child metadata", async () => {
  const details = await api.buildDirectory(Array.from({length: 175}, (_, i) => ({hash: hash("a"), name: `file-${String(i).padStart(3, "0")}`, size: 1})), {returnDetails: true});
  assert.equal(details.root.t, 3);
  assert.equal(details.root.l.length, 2);
  assert.equal(JSON.stringify(details.root.l[0].m), JSON.stringify({count: 174, first: "file-000", last: "file-173"}));
  assert.equal(details.root.l[0].t, 2);
});

test("resolves fanout children only after hash and bound verification", async () => {
  const child = {l: [{h: bytes("aa".repeat(32)), n: "a.txt", s: 1, t: 0}], t: 2};
  const childHash = await api.manifestHash(child);
  const root = {l: [{h: bytes(childHash), m: {count: 1, first: "a.txt", last: "a.txt"}, s: 1, t: 2}], t: 3};
  const resolved = await api.resolveDirectory(root, async hash => hash === childHash ? api.encodeManifest(child) : new Uint8Array());
  assert.equal(resolved.length, 1);
  await assert.rejects(api.resolveDirectory(root, async () => api.encodeManifest({...child, l: []})), /manifest hash mismatch/);
});

test("rejects traversal, duplicate names, unknown types and malformed bytes", () => {
  const link = {h: bytes("aa".repeat(32)), n: "ok", s: 1, t: 0};
  assert.throws(() => api.encodeManifest({l: [{...link, n: "../secret"}], t: 2}), /invalid directory entry name/);
  assert.throws(() => api.encodeManifest({l: [link, link], t: 2}), /duplicate directory entry/);
  assert.throws(() => api.encodeManifest({l: [{...link, t: 9}], t: 2}), /unsupported link type/);
  assert.throws(() => api.decodeManifest(Uint8Array.from([0x82, 0xa1, 0x6c])), /truncated/);
  assert.throws(() => api.encodeManifest({l: [{...link, n: "b"}, {...link, n: "a"}], t: 2}), /canonical/);
});
