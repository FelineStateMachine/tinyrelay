import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, getEventHash, getPublicKey, nip19, nip44, verifyEvent } from "nostr-tools";

globalThis.NostrSigner = {generateSecretKey, getPublicKey, getEventHash, finalizeEvent, verifyEvent, nip44, decodeNpub: value => nip19.decode(value).data};
globalThis.tiny = {util: {relayURL: value => value}, files: {messages: {normalizePubkey: value => /^[0-9a-f]{64}$/i.test(value) ? value.toLowerCase() : nip19.decode(value).data}}};
vm.runInThisContext(fs.readFileSync("internal/webui/direct-messages.js", "utf8"), {filename: "direct-messages.js"});

const alice = generateSecretKey();
const bob = generateSecretKey();
const alicePub = getPublicKey(alice);
const bobPub = getPublicKey(bob);
const makeSigner = secret => ({
  async getPublicKey() { return getPublicKey(secret); },
  async signEvent(unsigned) { return finalizeEvent(unsigned, secret); },
  async nip44Encrypt(peer, value) { return nip44.encrypt(value, nip44.getConversationKey(secret, peer)); },
  async nip44Decrypt(peer, value) { return nip44.decrypt(value, nip44.getConversationKey(secret, peer)); },
  nip44: {
    getConversationKey(peer) { return nip44.getConversationKey(secret, peer); },
    encrypt(first, second) {
      if (/^[0-9a-f]{64}$/i.test(first)) return nip44.encrypt(second, nip44.getConversationKey(secret, first));
      return nip44.encrypt(first, second);
    },
    decrypt(peer, value) { return nip44.decrypt(value, nip44.getConversationKey(secret, peer)); },
  },
});

// Model a NIP-07 signer: the shared wrappers pass peer and payload to these
// methods, while the signer retains the private key internally.
globalThis.tiny.nip44Encrypt = (peer, value, signer) => signer.nip44.encrypt(peer, value);
globalThis.tiny.nip44Decrypt = (peer, value, signer) => signer.nip44.decrypt(peer, value);
const nip07Signer = secret => {
  const base = makeSigner(secret);
  return {...base, nip44Encrypt: undefined, nip44Decrypt: undefined};
};

test("build creates independently decryptable recipient and self-copy wraps", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const result = await protocol.build({recipient: bobPub, content: "hello", reply: {id: "a".repeat(64), pubkey: alicePub}}, {signer: makeSigner(alice), now: 100});
  assert.equal(result.rumor.kind, 14);
  assert.equal(result.rumor.id, getEventHash(result.rumor));
  assert.equal(result.events.length, 2);
  assert.deepEqual(result.events.map(item => item.event.tags), [[["p", bobPub]], [["p", alicePub]]]);
  assert.deepEqual((await protocol.decrypt(result.events[0].event, {signer: makeSigner(bob)})).rumor, result.rumor);
  assert.deepEqual((await protocol.decrypt(result.events[1].event, {signer: makeSigner(alice)})).rumor, result.rumor);
});

test("decrypt rejects tampering, spoofed recipient and third-party access", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const result = await protocol.build({recipient: bobPub, content: "private"}, {signer: makeSigner(alice), now: 100});
  const tampered = {...result.events[0].event, content: result.events[0].event.content + "x"};
  await assert.rejects(() => protocol.decrypt(tampered, {signer: makeSigner(bob)}), /invalid gift wrap|unable to decrypt/);
  await assert.rejects(() => protocol.decrypt(result.events[0].event, {signer: makeSigner(alice)}), /not addressed/);
  const carol = generateSecretKey();
  await assert.rejects(() => protocol.decrypt(result.events[0].event, {signer: makeSigner(carol)}), /not addressed|unable to decrypt/);
});

test("normalizes npub and preserves relay transport wrappers", () => {
  const protocol = globalThis.tiny.chat.protocol;
  assert.equal(protocol.normalizePubkey(nip19.npubEncode(alicePub)), alicePub);
  assert.equal(typeof protocol.relayList, "function");
  assert.equal(typeof protocol.queryRelayList, "function");
  assert.equal(typeof protocol.publishEvent, "function");
});

test("round trips a kind 15 file rumor through the NIP-07 path", async () => {
  vm.runInThisContext(fs.readFileSync("internal/webui/file-messages.js", "utf8"), {filename: "file-messages.js"});
  const file = await globalThis.tiny.files.messages.build({
    recipient: bobPub, fileURL: "https://files.example/photo.jpg", ciphertextHash: "a".repeat(64),
    mimeType: "image/jpeg", key: "b".repeat(64), nonce: "c".repeat(24)
  }, {signer: nip07Signer(alice), now: 100});
  const direct = globalThis.tiny.chat.protocol;
  const opened = await direct.decrypt(file.events[0].event, {signer: nip07Signer(bob)});
  assert.equal(opened.rumor.kind, 15);
  assert.equal(opened.rumor.content, "https://files.example/photo.jpg");
});

test("rejects signed rumors, malformed timestamps and optional-tag abuse", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const result = await protocol.build({recipient: bobPub, content: "bounded"}, {signer: nip07Signer(alice), now: 100});
  const opened = await protocol.decrypt(result.events[0].event, {signer: nip07Signer(bob)});
  const makeWrap = rumor => {
    const seal = finalizeEvent({kind: 13, created_at: 1, tags: [], content: nip44.encrypt(JSON.stringify(rumor), nip44.getConversationKey(alice, bobPub))}, alice);
    const ephemeral = generateSecretKey();
    const wrapped = nip44.encrypt(JSON.stringify(seal), nip44.getConversationKey(ephemeral, bobPub));
    return finalizeEvent({kind: 1059, created_at: 1, tags: [["p", bobPub], ["expiration", String(Math.floor(Date.now() / 1000) + 3600)]], content: wrapped}, ephemeral);
  };
  const signed = {...opened.rumor, sig: "f".repeat(128)};
  signed.id = getEventHash(signed);
  await assert.rejects(() => protocol.decrypt(makeWrap(signed), {signer: nip07Signer(bob)}), /malformed direct message/);
  const badTime = {...opened.rumor, created_at: 1.5};
  badTime.id = getEventHash(badTime);
  await assert.rejects(() => protocol.decrypt(makeWrap(badTime), {signer: nip07Signer(bob)}), /malformed direct message/);
  const duplicateP = {...result.events[0].event, tags: [["p", bobPub], ["p", bobPub]]};
  await assert.rejects(() => protocol.decrypt(duplicateP, {signer: nip07Signer(bob)}), /recipient/);
});

test("rejects a signer that mutates the requested seal", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const evil = nip07Signer(alice);
  evil.signEvent = async unsigned => finalizeEvent({...unsigned, content: unsigned.content + " altered"}, alice);
  await assert.rejects(() => protocol.build({recipient: bobPub, content: "do not alter"}, {signer: evil}), /invalid NIP-59 seal/);
});

test("round trips an encrypted NIP-25 reaction in both copies", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const target = {id: "d".repeat(64), pubkey: alicePub, kind: 14};
  const result = await protocol.build({recipient: bobPub, reaction: {...target, content: "+"}}, {signer: nip07Signer(alice), now: 100});
  assert.equal(result.rumor.kind, 7);
  assert.deepEqual(result.rumor.tags.slice(1), [["e", target.id, "", "root", target.pubkey], ["p", target.pubkey], ["k", "14"]]);
  assert.equal((await protocol.decrypt(result.events[0].event, {signer: nip07Signer(bob)})).rumor.content, "+");
  assert.equal((await protocol.decrypt(result.events[1].event, {signer: nip07Signer(alice)})).rumor.kind, 7);
});

test("rejects reactions with malformed targets", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  await assert.rejects(() => protocol.build({recipient: bobPub, reaction: {id: "nope", pubkey: alicePub, content: "+"}}, {signer: nip07Signer(alice)}), /reaction target/);
});

test("accepts an inbound Flotilla reaction with only the target-author p tag", async () => {
  const protocol = globalThis.tiny.chat.protocol;
  const target = {id: "e".repeat(64), pubkey: bobPub, kind: 14};
  const signer = nip07Signer(alice);
  const rumor = {pubkey: bobPub, created_at: 100, kind: 7, tags: [["e", target.id, "", "root"], ["p", bobPub], ["k", "14"]], content: "+"};
  rumor.id = getEventHash(rumor);
  const seal = finalizeEvent({kind: 13, created_at: 100, tags: [], content: nip44.encrypt(JSON.stringify(rumor), nip44.getConversationKey(bob, alicePub))}, bob);
  const ephemeral = generateSecretKey();
  const wrapped = nip44.encrypt(JSON.stringify(seal), nip44.getConversationKey(ephemeral, alicePub));
  const gift = finalizeEvent({kind: 1059, created_at: 100, tags: [["p", alicePub]], content: wrapped}, ephemeral);
  const opened = await protocol.decrypt(gift, {signer});
  assert.equal(opened.rumor.pubkey, bobPub);
  assert.equal(opened.rumor.kind, 7);
});

test("large escaped messages are rejected before asking the signer to encrypt", async () => {
  let calls = 0;
  const signer = nip07Signer(alice);
  signer.nip44.encrypt = () => { calls++; throw Error("must not reach encryption"); };
  await assert.rejects(() => tiny.chat.protocol.build({recipient: bobPub, content: "\n".repeat(24000)}, {signer}), /too long/);
  assert.equal(calls, 0);
});

test("accepts an encrypted author retraction without a conversation p tag", async () => {
  const rumor = {pubkey: bobPub, created_at: 100, kind: 5, tags: [["e", "e".repeat(64)]], content: ""};
  rumor.id = getEventHash(rumor);
  const seal = finalizeEvent({kind: 13, created_at: 100, tags: [], content: nip44.encrypt(JSON.stringify(rumor), nip44.getConversationKey(bob, alicePub))}, bob);
  const secret = generateSecretKey();
  const gift = finalizeEvent({kind: 1059, created_at: 100, tags: [["p", alicePub]], content: nip44.encrypt(JSON.stringify(seal), nip44.getConversationKey(secret, alicePub))}, secret);
  assert.deepEqual((await tiny.chat.protocol.decrypt(gift, {signer: nip07Signer(alice)})).rumor, rumor);
});

test("decrypts independently generated Flotilla/Welshman gift wraps", async () => {
  const fixture = JSON.parse(fs.readFileSync("scripts/fixtures/flotilla-gift-wraps.json", "utf8"));
  const signer = nip07Signer(Uint8Array.from(Buffer.from(fixture.recipientSecret, "hex")));
  const kinds = [];
  for (const {gift, rumor} of fixture.vectors) {
    assert.equal(verifyEvent(gift), true);
    const opened = await tiny.chat.protocol.decrypt(gift, {signer});
    assert.deepEqual(opened.rumor, rumor);
    kinds.push(rumor.kind);
  }
  assert.deepEqual(kinds, [14, 14, 7, 15, 5]);
});
