import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";
import {generateSecretKey, getPublicKey, nip19} from "nostr-tools";

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

test("bech32Encode matches the reference nsec and npub encodings", () => {
  const sandbox = {crypto: webcrypto, TextEncoder, TextDecoder, URL, URLSearchParams, Uint8Array, ArrayBuffer};
  sandbox.globalThis = sandbox;
  vm.runInNewContext(tiny, sandbox);
  const {bech32Encode} = sandbox.tiny.util;
  for (let i = 0; i < 8; i++) {
    const secret = generateSecretKey();
    assert.equal(bech32Encode("nsec", secret), nip19.nsecEncode(secret));
    const pubkey = getPublicKey(secret);
    assert.equal(bech32Encode("npub", Uint8Array.from(Buffer.from(pubkey, "hex"))), nip19.npubEncode(pubkey));
  }
  // The NIP-19 example key.
  const example = "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d";
  assert.equal(bech32Encode("npub", Uint8Array.from(Buffer.from(example, "hex"))), "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6");
});
