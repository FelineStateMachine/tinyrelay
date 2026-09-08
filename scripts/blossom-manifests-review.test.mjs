import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../internal/webui/blossom-manifests.js", import.meta.url), "utf8");
const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");
const sandbox = {TextEncoder, TextDecoder, Uint8Array, ArrayBuffer, crypto: (await import("node:crypto")).webcrypto};
sandbox.globalThis = sandbox;
vm.runInNewContext(shared + source, sandbox);
const api = sandbox.tiny.blossom.manifests;
const hash = value => value.repeat(64);
const bytes = hex => Uint8Array.from(hex.match(/../g), value => parseInt(value, 16));

test("rejects duplicate names split across directory fanout children", async () => {
  const entries = Array.from({length: 173}, (_, i) => ({hash: hash("a"), name: `a${String(i).padStart(3, "0")}`, size: 1}));
  entries.push({hash: hash("b"), name: "dup", size: 1}, {hash: hash("c"), name: "dup", size: 1});
  await assert.rejects(api.buildDirectory(entries), /duplicate directory entry/);
});

test("decrypts encrypted file-manifest children before decoding", async () => {
  const leaf = new TextEncoder().encode("ok");
  const leafHash = api.hex(await api.sha256(leaf));
  const child = {l: [{h: bytes(leafHash), s: leaf.length, t: 0}], t: 1};
  const childBytes = api.encodeManifest(child);
  const encrypted = Uint8Array.from(childBytes, value => value ^ 0xff);
  const childHash = await api.hex(await api.sha256(encrypted));
  const root = {l: [{h: bytes(childHash), k: new Uint8Array(32), s: 2, t: 1}], t: 1};
  const result = await api.readFile(root, async (wantedHash) => {
    if (wantedHash === childHash) return encrypted;
    assert.equal(wantedHash, leafHash);
    return leaf;
  }, {decrypt: async value => {
    assert.equal(api.hex(value), api.hex(encrypted));
    return childBytes;
  }});
  assert.equal(result.length, 2);
});

test("decrypts encrypted directory-manifest children before decoding", async () => {
  const leaf = new TextEncoder().encode("ok");
  const leafHash = api.hex(await api.sha256(leaf));
  const child = {l: [{h: bytes(leafHash), n: "a.txt", s: leaf.length, t: 0}], t: 2};
  const childBytes = api.encodeManifest(child);
  const encrypted = Uint8Array.from(childBytes, value => value ^ 0xff);
  const childHash = await api.hex(await api.sha256(encrypted));
  const root = {l: [{h: bytes(childHash), k: new Uint8Array(32), m: {count: 1, first: "a.txt", last: "a.txt"}, s: leaf.length, t: 2}], t: 3};
  const result = await api.resolveDirectory(root, async wantedHash => {
    assert.equal(wantedHash, childHash);
    return encrypted;
  }, {decrypt: async value => {
    assert.equal(api.hex(value), api.hex(encrypted));
    return childBytes;
  }});
  assert.equal(result[0].n, "a.txt");
});

test("matches the BUD-17 directory fanout vector", async () => {
  const node = {l: [
    {h: bytes("11".repeat(32)), m: {count: 2, first: "a.txt", last: "b.txt"}, s: 30, t: 2},
    {h: bytes("22".repeat(32)), m: {count: 1, first: "c.txt", last: "c.txt"}, s: 40, t: 2}
  ], t: 3};
  assert.equal(await api.manifestHash(node), "6626ab03b5468f417d888fa25fa22b48f5bcb7dfafb88eef34c638d167afc0a3");
});

test("sorts and preserves non-ASCII names by UTF-8 byte order", async () => {
  const node = await api.buildDirectory([
    {hash: hash("a"), name: "é", size: 1},
    {hash: hash("b"), name: "e\u0301", size: 1},
    {hash: hash("c"), name: "😀", size: 1}
  ]);
  assert.deepEqual(node.l.map(link => link.n), ["e\u0301", "é", "😀"]);
});

test("honors zero traversal budgets", async () => {
  const one = new TextEncoder().encode("x");
  const oneHash = api.hex(await api.sha256(one));
  const file = {l: [{h: bytes(oneHash), s: 1, t: 0}], t: 1};
  await assert.rejects(api.readFile(file, async () => one, {maxManifests: 0}), /manifest count limit exceeded/);
  await assert.rejects(api.readFile(file, async () => one, {maxBytes: 0}), /file byte limit exceeded/);
  await assert.rejects(api.resolveDirectory({l: [], t: 3}, async () => new Uint8Array(), {maxManifests: 0}), /manifest count limit exceeded/);
});

test("bounds total directory fetches and permits encryption overhead separately", async () => {
  const child = {l: [{h: bytes("aa".repeat(32)), n: "a", s: 2, t: 0}], t: 2};
  const raw = api.encodeManifest(child), digest = api.hex(await api.sha256(raw));
  const root = {l: [{h: bytes(digest), m: {count: 1, first: "a", last: "a"}, s: 2, t: 2}], t: 3};
  await assert.rejects(api.resolveDirectory(root, async () => raw, {maxBytes: 1}), /byte limit/);
  const ciphertext = new Uint8Array(18), hash = api.hex(await api.sha256(ciphertext));
  const file = {l: [{h: bytes(hash), k: new Uint8Array(32), s: 2, t: 0}], t: 1};
  assert.equal((await api.readFile(file, async () => ciphertext, {maxBytes: 2, decrypt: async () => new Uint8Array(2)})).length, 2);
  await assert.rejects(api.readFile(file, async () => ciphertext, {maxBytes: 2, maxFetchedBytes: 1}), /byte limit/);
});
