// BUD-15 draft (June 2026), BUD-15: Client-side CHK encrypted blobs.
// https://github.com/hzrd149/blossom/pull/104
// Keys are client-only values. Callers must not include k in requests to a server.
(() => {
  "use strict";

  const encoder = new TextEncoder();
  const zeroNonce = new Uint8Array(12);
  const salt = encoder.encode("hashtree-chk");
  const info = encoder.encode("encryption-key");

  const {bytes, hex, sha256: digest} = globalThis.tiny.util;
  const fromHex = value => globalThis.tiny.util.fromHex(value, "expected a 64-character lowercase hex key");
  const equal = (left, right) => left.length === right.length && left.every((value, index) => value === right[index]);
  const aesKey = async key => {
    const material = await crypto.subtle.importKey("raw", key, "HKDF", false, ["deriveKey"]);
    return crypto.subtle.deriveKey(
      {name: "HKDF", hash: "SHA-256", salt, info},
      material,
      {name: "AES-GCM", length: 256},
      false,
      ["encrypt", "decrypt"]
    );
  };

  const encryptCHK = async plaintext => {
    const input = bytes(plaintext);
    const key = await digest(input);
    const encrypted = new Uint8Array(
      await crypto.subtle.encrypt({name: "AES-GCM", iv: zeroNonce}, await aesKey(key), input)
    );
    return {
      ciphertext: encrypted,
      key: hex(key),
      hash: hex(await digest(encrypted))
    };
  };

  const decryptCHK = async (ciphertext, key, hash) => {
    const input = bytes(ciphertext);
    const chkKey = fromHex(key);
    if (typeof hash !== "string" || !/^[0-9a-f]{64}$/.test(hash))
      throw new Error("expected a 64-character lowercase blob hash");
    if (hex(await digest(input)) !== hash) throw new Error("ciphertext hash mismatch");
    let plain;
    try {
      plain = new Uint8Array(
        await crypto.subtle.decrypt({name: "AES-GCM", iv: zeroNonce}, await aesKey(chkKey), input)
      );
    } catch {
      throw new Error("CHK decryption failed");
    }
    if (!equal(await digest(plain), chkKey)) throw new Error("plaintext hash mismatch");
    return plain;
  };

  const createURI = (hash, extension, key) => {
    if (typeof hash !== "string" || !/^[0-9a-f]{64}$/.test(hash))
      throw new Error("expected a 64-character lowercase blob hash");
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

  const api = Object.freeze({encryptCHK, decryptCHK, createURI, parseURI});
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.blossom = {...globalThis.tiny.blossom, encryption: api};
})();
