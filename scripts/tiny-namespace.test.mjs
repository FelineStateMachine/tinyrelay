import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const read = name => readFile(new URL(`../internal/webui/${name}`, import.meta.url), "utf8");
const [tiny, encryption, manifests, upload] = await Promise.all([
  read("tiny.js"), read("blossom-encryption.js"), read("blossom-manifests.js"), read("blossom-upload.js")
]);

test("feature modules bootstrap through the shared tiny namespace", () => {
  const sandbox = {crypto: webcrypto, TextEncoder, TextDecoder, URL, URLSearchParams, Uint8Array, ArrayBuffer, Blob, Map, setTimeout, clearTimeout};
  sandbox.globalThis = sandbox;
  vm.runInNewContext(tiny + encryption + manifests + upload, sandbox);
  assert.equal(typeof sandbox.tiny.util.hex, "function");
  assert.equal(typeof sandbox.tiny.blossom.encryption.encryptCHK, "function");
  assert.equal(typeof sandbox.tiny.blossom.manifests.encodeManifest, "function");
  assert.equal(typeof sandbox.tiny.blossom.upload.upload, "function");
  assert.equal("TinyBlossomEncryption" in sandbox, false);
  assert.equal("TinyBlossomManifests" in sandbox, false);
  assert.equal("TinyBlossomUpload" in sandbox, false);
  vm.runInNewContext(tiny + encryption + manifests + upload, sandbox);
  assert.equal(typeof sandbox.tiny.blossom.manifests.decodeManifest, "function");
});
