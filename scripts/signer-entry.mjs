import {
  SimplePool,
  finalizeEvent,
  generateSecretKey,
  getPublicKey,
  nip19,
  verifyEvent,
} from "nostr-tools";
import {
  BunkerSigner,
  createNostrConnectURI,
  parseBunkerInput,
} from "nostr-tools/nip46";

globalThis.NostrSigner = {
  generateSecretKey,
  getPublicKey,
  finalizeEvent,
  verifyEvent,
  SimplePool,
  BunkerSigner,
  parseBunkerInput,
  createNostrConnectURI,
  npubEncode: nip19.npubEncode,
  bytesToHex: (bytes) => Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join(""),
  hexToBytes: (hex) => Uint8Array.from(hex.match(/.{1,2}/g) ?? [], (byte) => parseInt(byte, 16)),
};
