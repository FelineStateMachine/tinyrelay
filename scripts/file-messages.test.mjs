import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, getEventHash, getPublicKey, nip19, nip44, verifyEvent } from "nostr-tools";

globalThis.NostrSigner = { generateSecretKey, getEventHash, getPublicKey, finalizeEvent, verifyEvent, nip44, decodeNpub: (value) => nip19.decode(value).data };
globalThis.tiny = {files: {}};
vm.runInThisContext(fs.readFileSync("internal/webui/tiny.js", "utf8"), { filename: "tiny.js" });
vm.runInThisContext(fs.readFileSync("internal/webui/file-messages.js", "utf8"), { filename: "file-messages.js" });
const fileMessages = globalThis.tiny.files.messages;

const alice = generateSecretKey();
const bob = generateSecretKey();
const alicePub = getPublicKey(alice);
const bobPub = getPublicKey(bob);
const signer = {
  async getPublicKey() { return alicePub; },
  async signEvent(event) { return finalizeEvent(event, alice); },
  async nip44Encrypt(recipient, plaintext) { return nip44.encrypt(plaintext, nip44.getConversationKey(alice, recipient)); },
};
const list = (pubkey, relays = ["wss://inbox.example"]) => finalizeEvent({ kind: 10050, tags: relays.map((relay) => ["relay", relay]), content: "", created_at: 100 }, pubkey === alicePub ? alice : bob);

test("build creates independently decryptable NIP-17 file gift wraps", async () => {
  const result = await fileMessages.build({ fileURL: "https://files.example/abc", ciphertextHash: "a".repeat(64), plaintextHash: "b".repeat(64), mimeType: "image/png", key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, { signer, now: 200 });
  assert.equal(result.rumor.pubkey, alicePub);
  assert.equal(result.rumor.id, getEventHash(result.rumor));
  assert.equal(result.rumor.kind, 15);
  assert.equal(result.rumor.tags.find((tag) => tag[0] === "x")[1], "a".repeat(64));
  assert.equal(result.events.length, 2);
  const receiver = result.events[0].event;
  assert.equal(receiver.kind, 1059);
  assert.deepEqual(receiver.tags, [["p", bobPub]]);
  assert.notEqual(receiver.pubkey, result.events[1].event.pubkey);
  const sealJSON = nip44.decrypt(receiver.content, nip44.getConversationKey(bob, receiver.pubkey));
  const seal = JSON.parse(sealJSON);
  assert.equal(seal.kind, 13);
  assert.deepEqual(seal.tags, []);
  assert.equal(seal.pubkey, alicePub);
  assert.equal(verifyEvent(seal), true);
  const rumor = JSON.parse(nip44.decrypt(seal.content, nip44.getConversationKey(bob, alicePub)));
  assert.equal(rumor.pubkey, alicePub);
  assert.equal(rumor.content, "https://files.example/abc");
  assert.equal("" + receiver.content.includes("11".repeat(32)), "false");
});

test("share requires valid inbox lists and publishes only to those lists", async () => {
  const published = [];
  const result = await fileMessages.share({ fileURL: "https://files.example/a", ciphertextHash: "c".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, {
    signer,
    queryRelayList: async (pubkey) => list(pubkey, pubkey === bobPub ? ["wss://bob.example"] : ["ws://127.0.0.1:7777"]),
    publish: async (event, relays) => { published.push({ event, relays }); return true; },
    now: 200,
  });
  assert.equal(published.length, 2);
  assert.deepEqual(published[0].relays, ["wss://bob.example"]);
  assert.deepEqual(published[1].relays, ["ws://127.0.0.1:7777"]);
  assert.deepEqual(result.relays.recipient, ["wss://bob.example"]);
});

test("share refuses to send when either inbox list is absent", async () => {
  let sends = 0;
  await assert.rejects(() => fileMessages.share({ fileURL: "https://files.example/a", ciphertextHash: "d".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, {
    signer,
    queryRelayList: async () => null,
    publish: async () => { sends++; },
  }), /valid kind 10050/);
  assert.equal(sends, 0);
});

test("share accepts npub recipients and rejects signer identity mismatches", async () => {
  const published = [];
  await fileMessages.share({ fileURL: "https://files.example/a", ciphertextHash: "e".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: nip19.npubEncode(bobPub) }, {
    signer,
    queryRelayList: async (pubkey) => list(pubkey),
    publish: async (event) => { published.push(event); return true; },
  });
  assert.equal(published.length, 2);
  const mismatched = { ...signer, async signEvent(event) { return finalizeEvent(event, bob); } };
  await assert.rejects(() => fileMessages.build({ fileURL: "https://files.example/a", ciphertextHash: "f".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, { signer: mismatched }), /invalid NIP-17 seal/);
});

test("share rejects an all-failed publish result", async () => {
  await assert.rejects(() => fileMessages.share({ fileURL: "https://files.example/a", ciphertextHash: "1".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, {
    signer,
    queryRelayList: async (pubkey) => list(pubkey),
    publish: async () => [false, false],
  }), /every inbox relay/);
});

test("default transport queries locally and publishes only inbox WebSockets", async () => {
  const queries = [];
  const sockets = [];
  globalThis.tiny = {
    signedFetch: async (path, method, body) => {
      queries.push({ path, method, body });
      const author = JSON.parse(body)[0].authors[0];
      return { json: async () => [list(author, [author === bobPub ? "wss://bob.example" : "wss://alice.example"]) ] };
    },
  };
  globalThis.WebSocket = class {
    constructor(url) { this.url = url; sockets.push(this); setTimeout(() => this.onopen?.(), 0); }
    send(data) { const event = JSON.parse(data)[1]; setTimeout(() => this.onmessage?.({ data: JSON.stringify(["OK", event.id, true, ""]) }), 0); }
    close() { this.closed = true; }
  };
  const result = await fileMessages.share({ fileURL: "https://files.example/a", ciphertextHash: "2".repeat(64), key: "11".repeat(32), nonce: "22".repeat(12), recipient: bobPub }, { signer, timeoutMs: 1000 });
  assert.equal(queries.length, 2);
  assert.deepEqual(sockets.map((socket) => socket.url), ["wss://bob.example", "wss://alice.example"]);
  assert.deepEqual(result.deliveries, [[true], [true]]);
  delete globalThis.tiny;
  delete globalThis.WebSocket;
});

test("default transport answers one NIP-42 challenge and retries the event", async () => {
  let eventAttempts = 0;
  globalThis.WebSocket = class {
    constructor() { setTimeout(() => this.onopen?.(), 0); }
    send(data) {
      const frame = JSON.parse(data);
      if (frame[0] === "EVENT") {
        eventAttempts++;
        if (eventAttempts === 1) {
          setTimeout(() => this.onmessage?.({ data: JSON.stringify(["AUTH", "challenge"]) }), 0);
          setTimeout(() => this.onmessage?.({ data: JSON.stringify(["OK", frame[1].id, false, "auth-required"]) }), 1);
        }
        else setTimeout(() => this.onmessage?.({ data: JSON.stringify(["OK", frame[1].id, true, ""]) }), 0);
      } else if (frame[0] === "AUTH") setTimeout(() => this.onmessage?.({ data: JSON.stringify(["OK", frame[1].id, true, ""]) }), 0);
    }
    close() {}
  };
  const result = await fileMessages.publishEvent({ id: "1".repeat(64) }, ["wss://inbox.example"], { signer, timeoutMs: 1000 });
  assert.deepEqual(result, [true]);
  assert.equal(eventAttempts, 2);
  delete globalThis.WebSocket;
});
