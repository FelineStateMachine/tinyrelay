import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../internal/webui/chat-files.js", import.meta.url), "utf8");
const hex = value => Buffer.from(value).toString("hex");
let DirectFile;
const revoked = [];
const sandbox = {
  crypto: webcrypto, TextEncoder, TextDecoder, Uint8Array, ArrayBuffer, URL, Blob,
  location: {origin: "https://relay.test"}, fetch: async () => { throw Error("unexpected fetch"); },
  HTMLElement: class {}, customElements: {get: () => undefined, define(_name, ctor) { DirectFile = ctor; }},
  document: {createElement: tag => ({localName: tag, append() {}, replaceChildren() {}, addEventListener() {}})}
};
sandbox.globalThis = sandbox;
sandbox.tiny = {util: {
  bytes: value => value instanceof Uint8Array ? value : new Uint8Array(value),
  hex, sha256: async value => new Uint8Array(await webcrypto.subtle.digest("SHA-256", value))
}};
vm.runInNewContext(source, sandbox);
const api = sandbox.tiny.chat.files;

const makeFile = async (plain = new TextEncoder().encode("hello")) => {
  const key = webcrypto.getRandomValues(new Uint8Array(32));
  const nonce = webcrypto.getRandomValues(new Uint8Array(12));
  const cryptoKey = await webcrypto.subtle.importKey("raw", key, {name: "AES-GCM"}, false, ["encrypt"]);
  const encrypted = new Uint8Array(await webcrypto.subtle.encrypt({name: "AES-GCM", iv: nonce}, cryptoKey, plain));
  const digest = value => webcrypto.subtle.digest("SHA-256", value).then(hex);
  return {plain, encrypted, key, nonce, hash: await digest(encrypted), plaintextHash: await digest(plain)};
};

test("parses interoperable NIP-17 kind 15 tags without exposing key in the node", async () => {
  const file = await makeFile();
  const rumor = {kind: 15, content: "https://cdn.example/file.mp4", tags: [
    ["p", "a".repeat(64)], ["file-type", "video/mp4"], ["encryption-algorithm", "aes-gcm"],
    ["decryption-key", hex(file.key)], ["decryption-nonce", hex(file.nonce)], ["x", file.hash], ["ox", file.plaintextHash], ["size", String(file.encrypted.length)]
  ]};
  const parsed = api.parse(rumor);
  assert.equal(parsed.mime, "video/mp4");
  assert.equal(parsed.hash, file.hash);
  assert.deepEqual(await api.decrypt(parsed, file.encrypted), file.plain);
  const node = api.render(rumor);
  assert.equal(node.rumor, rumor);
});

test("rejects credentialed, non-HTTP and malformed attachment URLs", () => {
  const base = {kind: 15, tags: [["encryption-algorithm", "aes-gcm"], ["decryption-key", "a".repeat(64)], ["decryption-nonce", "b".repeat(24)], ["x", "c".repeat(64)]]};
  for (const content of ["data:text/plain,secret", "javascript:alert(1)", "https://user:pass@example.test/a"]) {
    assert.throws(() => api.parse({...base, content}), /URL/);
  }
  assert.throws(() => api.parse({...base, content: "https://example.test/a", tags: base.tags.map(tag => tag[0] === "x" ? ["x", "bad"] : tag)}), /hash/);
});

test("checks ciphertext and plaintext hashes and authenticated decryption", async () => {
  const file = await makeFile();
  const rumor = {kind: 15, content: "https://cdn.example/file.txt", tags: [["encryption-algorithm", "aes-gcm"], ["decryption-key", hex(file.key)], ["decryption-nonce", hex(file.nonce)], ["x", file.hash], ["ox", file.plaintextHash]]};
  const parsed = api.parse(rumor);
  const altered = new Uint8Array(file.encrypted); altered[0] ^= 1;
  await assert.rejects(api.decrypt(parsed, altered), /hash/);
  const wrongKey = {...parsed, key: new Uint8Array(32).fill(7)};
  await assert.rejects(api.decrypt(wrongKey, file.encrypted), /decryption failed|plaintext hash mismatch/);
});

test("revokes attachment object URLs when its direct-file element disconnects", () => {
  const oldRevoke = sandbox.URL.revokeObjectURL;
  sandbox.URL.revokeObjectURL = value => revoked.push(value);
  const node = new DirectFile();
  node._attachmentURLs = new Set(["blob:attachment-1", "blob:attachment-2"]);
  node.disconnectedCallback();
  assert.deepEqual(revoked, ["blob:attachment-1", "blob:attachment-2"]);
  sandbox.URL.revokeObjectURL = oldRevoke;
});
