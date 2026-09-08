import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../internal/webui/blossom-encryption.js", import.meta.url), "utf8");
const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");
const sandbox = {crypto: webcrypto, TextEncoder, URL, URLSearchParams, Uint8Array, ArrayBuffer};
sandbox.globalThis = sandbox;
vm.runInNewContext(shared + source, sandbox);
const api = sandbox.tiny.blossom.encryption;
const utf8 = value => new TextEncoder().encode(value);
const hex = bytes => Buffer.from(bytes).toString("hex");

test("matches BUD-15 empty plaintext vector", async () => {
  const result = await api.encryptCHK(new Uint8Array());
  assert.equal(result.key, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  assert.equal(hex(result.ciphertext), "7cd161ae8406d82cdf553c1100d012db");
  assert.equal(result.hash, "346c46e7cc6722c99efe7f7bc316d8f3ff5f025f1031bf94418ef4db891e04cd");
  assert.deepEqual(await api.decryptCHK(result.ciphertext, result.key, result.hash), new Uint8Array());
});

test("matches BUD-15 hello vector", async () => {
  const result = await api.encryptCHK(utf8("hello"));
  assert.equal(result.key, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824");
  assert.equal(hex(result.ciphertext), "c65308d9c8649ff1c59820d0b3a030db34ad00f92d");
  assert.equal(result.hash, "70b977414934faa6270f323f117d05dbcc412e9d7ba2354b4d0f88f60aad2461");
  assert.deepEqual(await api.decryptCHK(result.ciphertext, result.key, result.hash), utf8("hello"));
});

test("rejects tampered ciphertext, hash, and key", async () => {
  const result = await api.encryptCHK(utf8("private"));
  const tampered = new Uint8Array(result.ciphertext);
  tampered[0] ^= 1;
  await assert.rejects(api.decryptCHK(tampered, result.key, result.hash), /ciphertext hash mismatch/);
  await assert.rejects(api.decryptCHK(result.ciphertext, result.key, "0".repeat(64)), /ciphertext hash mismatch/);
  const wrongKey = "f".repeat(64);
  await assert.rejects(api.decryptCHK(result.ciphertext, wrongKey, result.hash), /CHK decryption failed|plaintext hash mismatch/);
});

test("creates and parses a CHK URI without altering the key", async () => {
  const result = await api.encryptCHK(utf8("hello"));
  const uri = api.createURI(result.hash, "txt", result.key);
  assert.equal(uri, "blossom:" + result.hash + ".txt?enc=chk-v1&k=" + result.key);
  const parsed = api.parseURI(uri);
  assert.equal(parsed.hash, result.hash);
  assert.equal(parsed.extension, "txt");
  assert.equal(parsed.key, result.key);
  assert.throws(() => api.parseURI("blossom:" + result.hash + "?enc=chk-v1&k=" + result.key.toUpperCase()), /lowercase hex key/);
});

test("derives a distinct key and zero-nonce ciphertext for each content", async () => {
  const first = await api.encryptCHK(utf8("first"));
  const second = await api.encryptCHK(utf8("second"));
  assert.notEqual(first.key, second.key);
  assert.notEqual(first.hash, second.hash);
  assert.notDeepEqual(first.ciphertext, second.ciphertext);
});
