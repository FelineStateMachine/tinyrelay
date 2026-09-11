import assert from "node:assert/strict";
import test from "node:test";
import {finalizeEvent, generateSecretKey, getPublicKey, verifyEvent} from "nostr-tools/pure";
import {sharedSigning} from "./test-signing.mjs";

const secret = generateSecretKey();
const reorder = event => ({content: event.content, tags: event.tags, created_at: event.created_at, kind: event.kind, pubkey: event.pubkey, id: event.id, sig: event.sig});

function setup(signEvent) {
  const window = {nostr: {signEvent}, NostrSigner: {verifyEvent}};
  const tiny = {};
  return sharedSigning(window, tiny);
}

test("signing accepts a valid event when a signer reorders object fields", async () => {
  const signing = setup(async unsigned => reorder(finalizeEvent(structuredClone(unsigned), secret)));
  const event = await signing.signEvent({kind: 10002, created_at: 1, content: "", tags: [["r", "wss://relay.example"]]});
  assert.equal(verifyEvent(event), true);
});

test("signing still rejects a signer that changes an unsigned field", async () => {
  const signing = setup(async unsigned => {
    const event = structuredClone(unsigned);
    event.content = "changed";
    return finalizeEvent(event, secret);
  });
  await assert.rejects(signing.signEvent({kind: 10002, created_at: 1, content: "", tags: []}), /invalid or changed/);
});

test("signing compares against a snapshot when the caller mutates its input", async () => {
  let received;
  const signing = setup(async unsigned => {
    received = unsigned;
    return finalizeEvent(structuredClone(unsigned), secret);
  });
  const unsigned = {kind: 10002, created_at: 1, content: "", tags: []};
  const pending = signing.signEvent(unsigned);
  unsigned.tags.push(["r", "wss://changed.example"]);
  const event = await pending;
  assert.equal(received.tags.length, 0);
  assert.equal(event.tags.length, 0);
});

test("signing rejects an invalid signature even when every requested field matches", async () => {
  const signing = setup(async unsigned => ({...JSON.parse(JSON.stringify(finalizeEvent(structuredClone(unsigned), secret))), sig: "0".repeat(128)}));
  await assert.rejects(signing.signEvent({content: "", tags: [], created_at: 1, kind: 10050}), /invalid or changed/);
});

test("signing preserves a requested author and rejects a different signer", async () => {
  const signing = setup(async unsigned => reorder(finalizeEvent(structuredClone(unsigned), secret)));
  const unsigned = {pubkey: getPublicKey(secret), content: "", tags: [], created_at: 1, kind: 10002};
  assert.equal((await signing.signEvent(unsigned)).pubkey, unsigned.pubkey);
  await assert.rejects(signing.signEvent({...unsigned, pubkey: getPublicKey(generateSecretKey())}), /invalid or changed/);
});

test("signing rejects reordered tags and signer-side mutations", async () => {
  const signing = setup(async unsigned => {
    unsigned.tags.reverse();
    return finalizeEvent(structuredClone(unsigned), secret);
  });
  await assert.rejects(signing.signEvent({content: "", tags: [["r", "wss://first.example"], ["r", "wss://second.example"]], created_at: 1, kind: 10002}), /invalid or changed/);
});
