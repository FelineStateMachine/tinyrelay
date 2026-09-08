import {
  SimplePool,
  finalizeEvent,
  generateSecretKey,
  getPublicKey,
  getEventHash,
  nip44,
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
  getEventHash,
  verifyEvent,
  nip44,
  SimplePool,
  BunkerSigner,
  parseBunkerInput,
  createNostrConnectURI,
  npubEncode: nip19.npubEncode,
  decodeNpub: (value) => {
    const decoded = nip19.decode(value);
    if (decoded.type !== "npub") throw new Error("expected an npub");
    return decoded.data;
  },
  bytesToHex: (bytes) => Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join(""),
  hexToBytes: (hex) => Uint8Array.from(hex.match(/.{1,2}/g) ?? [], (byte) => parseInt(byte, 16)),
};
