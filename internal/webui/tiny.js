// Shared browser helpers. This file is loaded before feature modules.
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
  tiny.util = Object.freeze({...tiny.util, bytes, hex, sha256});
  // Feature modules add to these groups; guards can read them before load.
  tiny.blossom = tiny.blossom || {};
  tiny.files = tiny.files || {};
  globalThis.tiny = tiny;
})();
