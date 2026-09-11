#!/usr/bin/env node
// Local-only NIP-46 signer fixture for browser suspension and recovery checks.
// It exposes a tiny Nostr relay and a deterministic remote signer. The control
// endpoint deliberately drops transport connections so a browser can exercise
// its lost-signer and reconnect states without a phone or external relay.
import { createServer } from "node:http";
import { WebSocketServer, WebSocket } from "ws";
import { finalizeEvent, getPublicKey, verifyEvent } from "nostr-tools/pure";
import { BunkerSigner } from "nostr-tools/nip46";
import * as nip44 from "nostr-tools/nip44";

const relayPort = Number(process.env.SIGNER_RELAY_PORT ?? 18471);
const controlPort = Number(process.env.SIGNER_CONTROL_PORT ?? 18472);
if (!Number.isInteger(relayPort) || !Number.isInteger(controlPort)) throw Error("ports must be integers");

const key = n => Uint8Array.from(Buffer.from(String(n).padStart(64, "0"), "hex"));
// These are the public QA identities used by scripts/qa/buzz-chat-fixture.mjs.
const phoneSecret = key(process.env.SIGNER_PHONE_SECRET ?? "1");
const serviceSecret = key(process.env.SIGNER_SERVICE_SECRET ?? "4");
const clientSecret = key(process.env.SIGNER_CLIENT_SECRET ?? "5");
const phonePubkey = getPublicKey(phoneSecret);
const servicePubkey = getPublicKey(serviceSecret);
const clientPubkey = getPublicKey(clientSecret);
const relayURL = `ws://127.0.0.1:${relayPort}`;
const events = [];
const peers = new Map();
let offline = false;

const matches = (event, filter) => {
  if (filter.ids && !filter.ids.includes(event.id)) return false;
  if (filter.kinds && !filter.kinds.includes(event.kind)) return false;
  if (filter.authors && !filter.authors.includes(event.pubkey)) return false;
  if (filter.since && event.created_at < filter.since) return false;
  if (filter.until && event.created_at > filter.until) return false;
  for (const [name, values] of Object.entries(filter)) {
    if (name.startsWith("#") && !event.tags.some(tag => tag[0] === name.slice(1) && values.includes(tag[1]))) return false;
  }
  return true;
};
const send = (socket, value) => {
  if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(value));
};
const broadcast = event => {
  events.push(event);
  for (const [socket, subscriptions] of peers) {
    for (const [id, filters] of subscriptions) {
      if (filters.some(filter => matches(event, filter))) send(socket, ["EVENT", id, event]);
    }
  }
};

const relay = new WebSocketServer({ host: "127.0.0.1", port: relayPort });
relay.on("connection", socket => {
  if (offline) {
    socket.close(1013, "fixture offline");
    return;
  }
  const subscriptions = new Map();
  peers.set(socket, subscriptions);
  socket.on("message", raw => {
    if (offline) return;
    let message;
    try { message = JSON.parse(raw); } catch { return; }
    if (message[0] === "REQ") {
      const [id, ...filters] = message.slice(1);
      subscriptions.set(id, filters);
      for (const event of events) if (filters.some(filter => matches(event, filter))) send(socket, ["EVENT", id, event]);
      send(socket, ["EOSE", id]);
    } else if (message[0] === "EVENT" && message[1]) {
      const event = message[1];
      if (verifyEvent(event)) {
        broadcast(event);
        send(socket, ["OK", event.id, true, ""]);
      } else send(socket, ["OK", event.id, false, "invalid event"]);
    } else if (message[0] === "CLOSE") subscriptions.delete(message[1]);
  });
  socket.on("close", () => peers.delete(socket));
});

const reply = (requestEvent, request) => {
  const conversation = nip44.getConversationKey(serviceSecret, requestEvent.pubkey);
  const result = (() => {
    switch (request.method) {
      case "connect": return "ack";
      case "ping": return "pong";
      case "get_public_key": return phonePubkey;
      case "sign_event": return JSON.stringify(finalizeEvent(JSON.parse(request.params[0]), phoneSecret));
      case "nip44_encrypt": return nip44.encrypt(request.params[1], nip44.getConversationKey(phoneSecret, request.params[0]));
      case "nip44_decrypt": return nip44.decrypt(request.params[1], nip44.getConversationKey(phoneSecret, request.params[0]));
      default: throw Error("unsupported method " + request.method);
    }
  })();
  broadcast(finalizeEvent({
    kind: 24133,
    tags: [["p", requestEvent.pubkey]],
    content: nip44.encrypt(JSON.stringify({ id: request.id, result }), conversation),
    created_at: Math.floor(Date.now() / 1000)
  }, serviceSecret));
};

const handled = new Set();
const requestPoller = setInterval(() => {
  if (offline) return;
  for (const event of events) {
    if (event.kind !== 24133 || event.pubkey !== clientPubkey || !event.tags.some(tag => tag[0] === "p" && tag[1] === servicePubkey) || handled.has(event)) continue;
    handled.add(event);
    try {
      const conversation = nip44.getConversationKey(serviceSecret, event.pubkey);
      reply(event, JSON.parse(nip44.decrypt(event.content, conversation)));
    } catch (error) { console.error("signer request failed:", error.message); }
  }
}, 25);

const control = createServer((request, response) => {
  if (request.method !== "POST" || !["/offline", "/online"].includes(request.url)) {
    response.writeHead(404, { "content-type": "text/plain" });
    response.end("use POST /offline or POST /online\n");
    return;
  }
  offline = request.url === "/offline";
  if (offline) for (const socket of peers.keys()) socket.close(1013, "fixture offline");
  response.writeHead(200, { "content-type": "application/json" });
  response.end(JSON.stringify({ offline }));
});

const clientPointer = { pubkey: servicePubkey, relays: [relayURL] };
const runSelfTest = async () => {
  const signer = BunkerSigner.fromBunker(clientSecret, clientPointer);
  await signer.connect({ name: "tinyrelay signer resume fixture" });
  if ((await signer.getPublicKey()) !== phonePubkey) throw Error("unexpected signer identity");
  const signed = await signer.signEvent({ kind: 1, content: "local signer fixture", tags: [], created_at: 1 });
  if (!verifyEvent(signed) || signed.pubkey !== phonePubkey) throw Error("signer returned invalid event");
  await signer.close();
  signer.pool.destroy();
  console.log("self-test: connect, get_public_key and sign_event passed");
};

control.listen(controlPort, "127.0.0.1", async () => {
  console.log(JSON.stringify({
    relay: relayURL,
    control: `http://127.0.0.1:${controlPort}`,
    bunker: `bunker://${servicePubkey}?relay=${encodeURIComponent(relayURL)}`,
    phonePubkey,
    servicePubkey,
    clientPubkey,
    localOnly: true,
    controls: ["POST /offline", "POST /online"]
  }, null, 2));
  try { await runSelfTest(); } catch (error) { console.error("self-test failed:", error); process.exitCode = 1; }
});

const stop = () => {
  clearInterval(requestPoller);
  for (const socket of peers.keys()) socket.terminate();
  control.close();
  relay.close();
};
process.once("SIGINT", stop);
process.once("SIGTERM", stop);
