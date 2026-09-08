// BUD-15 draft (June 2026), BUD-15: Client-side CHK encrypted blobs.
// https://raw.githubusercontent.com/mmalmi/blossom/codex/bud-15-chk-encryption/buds/15.md
// Keys are client-only values. Callers must not include k in requests to a server.
(() => {
  "use strict";

  const encoder = new TextEncoder();
  const zeroNonce = new Uint8Array(12);
  const salt = encoder.encode("hashtree-chk");
  const info = encoder.encode("encryption-key");

  const hex = bytes => Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
  const bytes = value => {
    if (value instanceof Uint8Array) return value;
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    throw new TypeError("expected Uint8Array");
  };
  const fromHex = value => {
    if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) throw new Error("expected a 64-character lowercase hex key");
    const result = new Uint8Array(32);
    for (let index = 0; index < result.length; index++) result[index] = parseInt(value.slice(index * 2, index * 2 + 2), 16);
    return result;
  };
  const equal = (left, right) => left.length === right.length && left.every((value, index) => value === right[index]);
  const digest = async value => new Uint8Array(await crypto.subtle.digest("SHA-256", bytes(value)));
  const aesKey = async key => {
    const material = await crypto.subtle.importKey("raw", key, "HKDF", false, ["deriveKey"]);
    return crypto.subtle.deriveKey({name: "HKDF", hash: "SHA-256", salt, info}, material,
      {name: "AES-GCM", length: 256}, false, ["encrypt", "decrypt"]);
  };

  const encryptCHK = async plaintext => {
    const input = bytes(plaintext);
    const key = await digest(input);
    const encrypted = new Uint8Array(await crypto.subtle.encrypt({name: "AES-GCM", iv: zeroNonce}, await aesKey(key), input));
    return {ciphertext: encrypted, key: hex(key), hash: hex(await digest(encrypted))};
  };

  const decryptCHK = async (ciphertext, key, hash) => {
    const input = bytes(ciphertext);
    const chkKey = fromHex(key);
    if (typeof hash !== "string" || !/^[0-9a-f]{64}$/.test(hash)) throw new Error("expected a 64-character lowercase blob hash");
    if (hex(await digest(input)) !== hash) throw new Error("ciphertext hash mismatch");
    let plain;
    try {
      plain = new Uint8Array(await crypto.subtle.decrypt({name: "AES-GCM", iv: zeroNonce}, await aesKey(chkKey), input));
    } catch {
      throw new Error("CHK decryption failed");
    }
    if (!equal(await digest(plain), chkKey)) throw new Error("plaintext hash mismatch");
    return plain;
  };

  const createURI = (hash, extension, key) => {
    if (typeof hash !== "string" || !/^[0-9a-f]{64}$/.test(hash)) throw new Error("expected a 64-character lowercase blob hash");
    const path = "blossom:" + hash + (extension ? "." + String(extension).replace(/^\.+/, "") : "");
    fromHex(key);
    const params = new URLSearchParams({enc: "chk-v1", k: key});
    return path + "?" + params;
  };

  const parseURI = value => {
    if (typeof value !== "string" || !value.startsWith("blossom:")) throw new Error("expected a Blossom URI");
    const parsed = new URL(value.slice("blossom:".length), "https://blossom.invalid");
    const match = parsed.pathname.match(/^\/([0-9a-f]{64})(?:\.([A-Za-z0-9]+))?$/);
    if (!match || parsed.searchParams.get("enc") !== "chk-v1") throw new Error("invalid CHK Blossom URI");
    const key = parsed.searchParams.get("k");
    fromHex(key);
    return {hash: match[1], extension: match[2] || "", key};
  };

  globalThis.TinyBlossomEncryption = Object.freeze({encryptCHK, decryptCHK, createURI, parseURI});
})();
