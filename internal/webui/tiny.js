// Shared browser helpers. This file is loaded before every feature module
// and the signer bridge, so each helper exists exactly once.
(() => {
  "use strict";
  const tiny = globalThis.tiny || {};
  const bytes = value => {
    if (value instanceof Uint8Array) return value;
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    if (value instanceof ArrayBuffer) return new Uint8Array(value);
    throw new TypeError("expected bytes");
  };
  const hex = value => Array.from(bytes(value), byte => byte.toString(16).padStart(2, "0")).join("");
  const sha256 = async value => new Uint8Array(await crypto.subtle.digest("SHA-256", bytes(value)));
  // fromHex parses a 64-character lowercase hex hash or key.
  const fromHex = (value, message = "expected a 64-character lowercase hex value") => {
    if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) throw Error(message);
    return Uint8Array.from(value.match(/../g), byte => parseInt(byte, 16));
  };
  const element = (tag, text) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    return node;
  };
  // relayURL normalizes a ws or wss URL without credentials or a fragment, or
  // returns null. Strict mode also rejects query strings, which operator-entered
  // relay lists never need.
  const relayURL = (value, {strict = false} = {}) => {
    if (typeof value !== "string" || value.length > 2048) return null;
    let url;
    try {
      url = new URL(value.trim());
    } catch {
      return null;
    }
    if (
      (url.protocol !== "wss:" && url.protocol !== "ws:") ||
      !url.hostname ||
      url.username ||
      url.password ||
      url.hash
    )
      return null;
    if (strict && url.search) return null;
    url.hostname = url.hostname.toLowerCase();
    if (url.pathname.length > 1) url.pathname = url.pathname.replace(/\/+$/, "");
    return url.toString().replace(/\/$/, "");
  };
  const b64url = value => {
    let binary = "";
    bytes(value).forEach(byte => {
      binary += String.fromCharCode(byte);
    });
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const fromB64url = value => {
    const text = value.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((value.length + 3) % 4);
    return Uint8Array.from(atob(text), char => char.charCodeAt(0));
  };
  // bech32Encode renders bytes as a NIP-19 bare string such as an nsec or
  // npub. The browser bundle decodes these but has no plain encoder.
  const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l";
  const bech32Polymod = values => {
    const generators = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3];
    let checksum = 1;
    for (const value of values) {
      const top = checksum >>> 25;
      checksum = (((checksum & 0x1ffffff) << 5) ^ value) >>> 0;
      generators.forEach((generator, index) => { if ((top >>> index) & 1) checksum = (checksum ^ generator) >>> 0; });
    }
    return checksum;
  };
  const bech32Encode = (prefix, value) => {
    const words = [];
    let accumulator = 0, bits = 0;
    for (const byte of bytes(value)) {
      accumulator = ((accumulator << 8) | byte) >>> 0;
      bits += 8;
      while (bits >= 5) {
        bits -= 5;
        words.push((accumulator >>> bits) & 31);
      }
    }
    if (bits > 0) words.push((accumulator << (5 - bits)) & 31);
    const expanded = [...prefix].map(char => char.charCodeAt(0) >>> 5).concat(0, [...prefix].map(char => char.charCodeAt(0) & 31));
    const polymod = bech32Polymod(expanded.concat(words, [0, 0, 0, 0, 0, 0])) ^ 1;
    const checksum = Array.from({length: 6}, (_, index) => (polymod >>> (5 * (5 - index))) & 31);
    return prefix + "1" + words.concat(checksum).map(word => bech32Charset[word]).join("");
  };
  // wikiName turns a title into a NIP-54 page name the way the relay does:
  // lowercase, whitespace as hyphens, letters, digits and marks kept,
  // everything else dropped, hyphens collapsed and trimmed at both ends.
  const wikiName = title => {
    let out = "";
    for (const ch of String(title ?? "").toLowerCase()) {
      if (/[\s\-_]/u.test(ch)) {
        if (out && !out.endsWith("-")) out += "-";
      } else if (/[\p{L}\p{Nd}\p{M}]/u.test(ch)) {
        out += ch;
      }
    }
    return out.replace(/^-+|-+$/g, "");
  };
  tiny.util = Object.freeze({...tiny.util, bytes, hex, sha256, fromHex, element, relayURL, b64url, fromB64url, bech32Encode, wikiName});
  // signer returns the active signer: a resumed remote signer first, then a
  // NIP-07 extension.
  tiny.signer = () => globalThis.tinySigner || globalThis.nostr;
  const nip44 = (method, pubkey, text, signer = tiny.signer()) => {
    if (typeof signer?.["nip44" + method] === "function") return signer["nip44" + method](pubkey, text);
    const inner = signer?.nip44?.[method.toLowerCase()];
    if (typeof inner === "function") return inner.call(signer.nip44, pubkey, text);
    throw Error("The connected signer does not support NIP-44.");
  };
  tiny.nip44Encrypt = (pubkey, text, signer) => nip44("Encrypt", pubkey, text, signer);
  tiny.nip44Decrypt = (pubkey, text, signer) => nip44("Decrypt", pubkey, text, signer);
  // Event publication is one browser boundary shared by forms, rooms and
  // WebMCP. Signers are treated as untrusted extensions: the returned event
  // must preserve the requested unsigned fields and pass the verifier before
  // it is sent to the relay.
  const signEvent = async (unsigned, {signal} = {}) => {
    if (!globalThis.nostr?.signEvent) throw Error("Connect a signer first.");
    if (typeof globalThis.NostrSigner?.verifyEvent !== "function") throw Error("The signer verifier is still loading. Try again.");
    const expected = JSON.stringify(unsigned);
    const event = await globalThis.nostr.signEvent(JSON.parse(expected));
    const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
    if (actual !== expected || globalThis.NostrSigner.verifyEvent(event) !== true) throw Error("The signer returned an invalid or changed event.");
    if (signal?.aborted) throw Error("Sending canceled.");
    return event;
  };
  const publishEvent = async (unsigned, options = {}) => {
    const event = await signEvent(unsigned, options);
    if (typeof tiny.signedFetch !== "function") throw Error("The signed request bridge is still loading. Try again.");
    const response = await tiny.signedFetch(options.path || "/events", "POST", JSON.stringify(event), {contentType: "application/json", signal: options.signal});
    let result = {};
    try { result = await response.json(); } catch {}
    if (!response.ok || result.accepted !== true) throw Error(result.error || result.message || "The relay rejected the event.");
    return event;
  };
  tiny.signing = Object.freeze({signEvent, publish: publishEvent});
  // Feature bundles load on the pages that use them and again after an
  // in-place navigation lands on such a page. Each script loads once.
  tiny.bundles = {
    files: ["blossom-encryption.js", "blossom-manifests.js", "blossom-upload.js", "file-messages.js", "file-workspace.js"],
    private: ["private-services.js"]
  };
  const loaded = new Map();
  const loadScript = name => {
    if (!loaded.has(name)) {
      const version = document.documentElement.dataset.scripts || "";
      const present = [...(document.scripts || [])].some(script => (script.src || "").includes("/scripts/" + name));
      loaded.set(name, present ? Promise.resolve() : new Promise((resolve, reject) => {
        const script = document.createElement("script");
        script.src = (tiny.localPath ? tiny.localPath("/scripts/" + name) : "/scripts/" + name) + (version ? "?v=" + version : "");
        script.onload = () => resolve();
        script.onerror = () => { loaded.delete(name); reject(Error("Could not load " + name)); };
        document.head.append(script);
      }));
    }
    return loaded.get(name);
  };
  tiny.require = async bundle => {
    for (const name of tiny.bundles[bundle] || [bundle]) await loadScript(name);
  };
  // Feature modules add to these groups; guards can read them before load.
  tiny.blossom = tiny.blossom || {};
  tiny.files = tiny.files || {};
  globalThis.tiny = tiny;
})();
