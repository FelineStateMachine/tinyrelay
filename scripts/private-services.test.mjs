import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const shared = await readFile(new URL("../internal/webui/tiny.js", import.meta.url), "utf8");
const source = await readFile(new URL("../internal/webui/private-services.js", import.meta.url), "utf8");
class Element {}
const sandbox = {
  URL, TextEncoder, HTMLElement: Element,
  document: {}, Response,
  customElements: {define() {}},
  tinySigner: {
    getPublicKey: async () => "a".repeat(64),
    nip44Encrypt: async (_pubkey, value) => "enc:" + value,
    nip44Decrypt: async (_pubkey, value) => value.replace(/^enc:/, "")
  },
  nostr: {signEvent: async event => event},
  NostrSigner: {verifyEvent: () => true}
};
sandbox.window = sandbox;
 sandbox.tiny = {files: {}};
vm.runInNewContext(shared + source, sandbox);
const api = sandbox.tiny.files.privateServices;

test("accepts tenant paths while rejecting unsafe relay URLs", () => {
  assert.equal(api.validURL("wss://Relay.Example/r/Private"), true);
  assert.equal(api.canonicalURL("wss://Relay.Example/r/Private/"), "wss://relay.example/r/Private");
  assert.equal(api.validURL("wss://relay.example/?token=secret"), false);
  assert.equal(api.validURL("wss://user:pass@relay.example"), false);
  assert.equal(api.validURL("https://relay.example"), false);
});

test("NIP-44 helpers round-trip and retain unrelated private entries", async () => {
  const clear = JSON.stringify([["name", "work"], ["g", "wss://old.example"], ["p", "b".repeat(64)]]);
  const encrypted = await api.encrypt("a".repeat(64), clear);
  assert.equal(await api.decrypt("a".repeat(64), encrypted), clear);
  assert.deepEqual(JSON.parse(JSON.stringify(api.mergeRows([["name", "work"], ["g", "wss://old.example"], ["p", "b".repeat(64)]], ["wss://new.example/r/tenant"]))), [["name", "work"], ["p", "b".repeat(64)], ["g", "wss://new.example/r/tenant"]]);
});

test("NIP-07 nip44 methods are accepted alongside bunker methods", async () => {
  sandbox.tinySigner = null;
  sandbox.nostr = {nip44: {
    encrypt: async (_pubkey, value) => "nip07:" + value,
    decrypt: async (_pubkey, value) => value.replace(/^nip07:/, "")
  }};
  const encrypted = await api.encrypt("a".repeat(64), "private");
  assert.equal(encrypted, "nip07:private");
  assert.equal(await api.decrypt("a".repeat(64), encrypted), "private");
});

test("kind 10318 wire has no public tags", () => {
  const event = api.wireEvent("ciphertext");
  assert.equal(event.kind, 10318);
  assert.equal(JSON.stringify(event.tags), "[]");
  assert.equal(event.content, "ciphertext");
});

test("reads only a verified kind 10318 event for the requested author", async () => {
  const pubkey = "a".repeat(64);
  const event = {kind: 10318, pubkey, tags: [], content: "ciphertext", id: "1", sig: "2"};
  const requests = [];
  sandbox.tiny = {signedFetch: async (_path, _method, body) => {
    requests.push(JSON.parse(body));
    return new Response(JSON.stringify([event]), {status: 200});
  }};
  sandbox.NostrSigner.verifyEvent = () => true;
  assert.deepEqual(JSON.parse(JSON.stringify(await api.readList(pubkey))), event);
  assert.deepEqual(JSON.parse(JSON.stringify(requests[0])), [{kinds: [10318], authors: [pubkey], limit: 1}]);
  sandbox.tiny.signedFetch = async () => new Response(JSON.stringify([{...event, pubkey: "b".repeat(64)}]), {status: 200});
  await assert.rejects(api.readList(pubkey), /invalid/);
  sandbox.tiny.signedFetch = async () => new Response(JSON.stringify({items: []}), {status: 200});
  await assert.rejects(api.readList(pubkey), /malformed/);
});

test("rejects a failed event signature", () => {
  sandbox.NostrSigner.verifyEvent = () => false;
  assert.equal(api.acceptEvent({kind: 10318, pubkey: "a".repeat(64), tags: [], content: ""}, "a".repeat(64)), false);
});
