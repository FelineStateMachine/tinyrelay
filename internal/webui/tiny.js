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
  tiny.util = Object.freeze({...tiny.util, bytes, hex, sha256, fromHex, element, relayURL, b64url, fromB64url});
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
