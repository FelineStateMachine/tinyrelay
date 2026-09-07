#!/usr/bin/env node
// A throwaway NIP-46 bunker for trying the Bunker URL sign-in flow locally.
// It runs a minimal in-memory relay on 127.0.0.1:8789, answers connect,
// ping, get_public_key and sign_event requests for a fresh user key, and
// prints the bunker URL to paste into the sign-in page. Set BUNKER_USER_SECRET
// to a hex secret to sign as a specific key.
import { WebSocketServer, WebSocket } from "ws";
import { finalizeEvent, generateSecretKey, getPublicKey } from "nostr-tools/pure";
import * as nip44 from "nostr-tools/nip44";
import { hexToBytes } from "@noble/hashes/utils.js";

globalThis.WebSocket = WebSocket;

const port = Number(process.env.BUNKER_PORT ?? 8789);
const bunkerSecret = generateSecretKey();
const bunkerPubkey = getPublicKey(bunkerSecret);
const userSecret = process.env.BUNKER_USER_SECRET ? hexToBytes(process.env.BUNKER_USER_SECRET) : generateSecretKey();
const userPubkey = getPublicKey(userSecret);
const clients = new Map();
const events = [];

const matches = (event, filter) => {
  if (filter.kinds && !filter.kinds.includes(event.kind)) return false;
  if (filter.authors && !filter.authors.includes(event.pubkey)) return false;
  for (const [key, values] of Object.entries(filter)) {
    if (key[0] === "#" && !event.tags.some(tag => tag[0] === key.slice(1) && values.includes(tag[1]))) return false;
  }
  return true;
};

const broadcast = event => {
  events.push(event);
  for (const [peer, subscriptions] of clients) {
    for (const [id, filters] of subscriptions) {
      if (filters.some(filter => matches(event, filter))) peer.send(JSON.stringify(["EVENT", id, event]));
    }
  }
};

const server = new WebSocketServer({ host: "127.0.0.1", port });
server.on("connection", socket => {
  const subscriptions = new Map();
  clients.set(socket, subscriptions);
  socket.on("message", raw => {
    let message;
    try { message = JSON.parse(raw); } catch { return; }
    if (message[0] === "REQ") {
      const [id, ...filters] = message.slice(1);
      subscriptions.set(id, filters);
      for (const event of events) {
        if (filters.some(filter => matches(event, filter))) socket.send(JSON.stringify(["EVENT", id, event]));
      }
      socket.send(JSON.stringify(["EOSE", id]));
    } else if (message[0] === "EVENT") {
      broadcast(message[1]);
      socket.send(JSON.stringify(["OK", message[1].id, true, ""]));
    } else if (message[0] === "CLOSE") {
      subscriptions.delete(message[1]);
    }
  });
  socket.on("close", () => clients.delete(socket));
});

const reply = (clientPubkey, payload) => {
  const conversation = nip44.getConversationKey(bunkerSecret, clientPubkey);
  broadcast(finalizeEvent({
    kind: 24133,
    tags: [["p", clientPubkey]],
    content: nip44.encrypt(JSON.stringify(payload), conversation),
    created_at: Math.floor(Date.now() / 1000)
  }, bunkerSecret));
};

const answer = request => {
  switch (request.method) {
    case "connect": return "ack";
    case "ping": return "pong";
    case "get_public_key": return userPubkey;
    case "sign_event": return JSON.stringify(finalizeEvent(JSON.parse(request.params[0]), userSecret));
    default: throw Error("unsupported method " + request.method);
  }
};

const handled = new WeakSet();
setInterval(() => {
  for (const event of events) {
    if (event.kind !== 24133 || event.pubkey === bunkerPubkey || handled.has(event)) continue;
    handled.add(event);
    try {
      const conversation = nip44.getConversationKey(bunkerSecret, event.pubkey);
      const request = JSON.parse(nip44.decrypt(event.content, conversation));
      reply(event.pubkey, { id: request.id, result: answer(request) });
    } catch (err) {
      console.error("bunker request failed:", err.message);
    }
  }
}, 50);

console.log("user pubkey: " + userPubkey);
console.log(`bunker://${bunkerPubkey}?relay=${encodeURIComponent("ws://127.0.0.1:" + port)}`);
