/* NIP-17 kind 15 attachments. File keys stay in the decrypted rumor and are
   never copied into markup, URLs, local storage, or the clipboard. */
(() => {
  "use strict";

  const MAX_BYTES = 256 * 1024 * 1024;
  const textDecoder = new TextDecoder();
  const sizeLabel = size => {
    if (size === null) return "";
    if (size < 1024) return size + " B";
    if (size < 1024 * 1024) return (size / 1024).toFixed(1) + " KiB";
    if (size < 1024 * 1024 * 1024) return (size / (1024 * 1024)).toFixed(1) + " MiB";
    return (size / (1024 * 1024 * 1024)).toFixed(1) + " GiB";
  };
  const bytes = value => globalThis.tiny.util.bytes(value);
  const hex = value => globalThis.tiny.util.hex(value);
  const fromHex = (value, message) => {
    if (typeof value !== "string" || !/^[0-9a-f]+$/i.test(value) || value.length % 2)
      throw Error(message);
    return Uint8Array.from(value.match(/../g), pair => parseInt(pair, 16));
  };
  const tagValue = (tags, name) => {
    const tag = (Array.isArray(tags) ? tags : []).find(item => item?.[0] === name);
    return typeof tag?.[1] === "string" ? tag[1] : "";
  };

  function parse(rumor) {
    if (!rumor || rumor.kind !== 15 || typeof rumor.content !== "string")
      throw Error("expected a NIP-17 file message");
    let url;
    try { url = new URL(rumor.content); } catch { throw Error("attachment URL is invalid"); }
    if ((url.protocol !== "https:" && url.protocol !== "http:") || url.username || url.password)
      throw Error("attachment URL must be HTTP(S) without credentials");
    const algorithm = tagValue(rumor.tags, "encryption-algorithm").toLowerCase();
    if (algorithm !== "aes-gcm") throw Error("unsupported attachment encryption");
    const keyText = tagValue(rumor.tags, "decryption-key");
    const nonceText = tagValue(rumor.tags, "decryption-nonce");
    const hash = tagValue(rumor.tags, "x").toLowerCase();
    const key = fromHex(keyText, "attachment key is invalid");
    const nonce = fromHex(nonceText, "attachment nonce is invalid");
    if (key.length !== 32) throw Error("attachment key is invalid");
    if (nonce.length < 8 || nonce.length > 32) throw Error("attachment nonce is invalid");
    if (!/^[0-9a-f]{64}$/.test(hash)) throw Error("attachment hash is invalid");
    const plaintextHash = tagValue(rumor.tags, "ox").toLowerCase();
    if (plaintextHash && !/^[0-9a-f]{64}$/.test(plaintextHash)) throw Error("plaintext hash is invalid");
    const sizeText = tagValue(rumor.tags, "size");
    const size = sizeText ? Number(sizeText) : null;
    if (size !== null && (!Number.isSafeInteger(size) || size < 0 || size > MAX_BYTES))
      throw Error("attachment is too large");
    return Object.freeze({
      url: url.href,
      mime: tagValue(rumor.tags, "file-type") || "application/octet-stream",
      name: tagValue(rumor.tags, "filename") || tagValue(rumor.tags, "name") || "attachment",
      hash,
      plaintextHash,
      size,
      key,
      nonce
    });
  }

  async function responseBytes(response, signal) {
    const declared = Number(response.headers?.get?.("content-length"));
    if (Number.isFinite(declared) && declared > MAX_BYTES) throw Error("attachment is too large");
    if (response.body?.getReader) {
      const reader = response.body.getReader();
      const chunks = [];
      let total = 0;
      try {
        for (;;) {
          if (signal?.aborted) throw Error("attachment opening canceled");
          const part = await reader.read();
          if (part.done) break;
          const chunk = bytes(part.value);
          total += chunk.length;
          if (total > MAX_BYTES) {
            await reader.cancel();
            throw Error("attachment is too large");
          }
          chunks.push(chunk);
        }
      } finally { reader.releaseLock?.(); }
      const output = new Uint8Array(total);
      let offset = 0;
      chunks.forEach(chunk => { output.set(chunk, offset); offset += chunk.length; });
      return output;
    }
    const output = bytes(await response.arrayBuffer());
    if (output.length > MAX_BYTES) throw Error("attachment is too large");
    return output;
  }

  async function fetchCiphertext(file, signal) {
    const init = {method: "GET", credentials: "omit", referrerPolicy: "no-referrer", signal};
    let response = await fetch(file.url, init);
    if (!response.ok && new URL(file.url).origin === globalThis.location.origin && response.status === 401 && globalThis.tiny.signedFetch) {
      const local = new URL(file.url).pathname + new URL(file.url).search;
      response = await globalThis.tiny.signedFetch(local, "GET", undefined, {signal});
    }
    if (!response.ok) throw Error("attachment could not be fetched");
    return responseBytes(response, signal);
  }

  async function decrypt(file, ciphertext) {
    const encrypted = bytes(ciphertext);
    if (hex(await globalThis.tiny.util.sha256(encrypted)) !== file.hash) throw Error("attachment hash mismatch");
    let plain;
    try {
      const cryptoKey = await crypto.subtle.importKey("raw", file.key, {name: "AES-GCM"}, false, ["decrypt"]);
      plain = new Uint8Array(await crypto.subtle.decrypt({name: "AES-GCM", iv: file.nonce}, cryptoKey, encrypted));
    } catch { throw Error("attachment decryption failed"); }
    if (file.plaintextHash && hex(await globalThis.tiny.util.sha256(plain)) !== file.plaintextHash)
      throw Error("attachment plaintext hash mismatch");
    return plain;
  }

  function downloadLink(owner, file, plain) {
    const url = URL.createObjectURL(new Blob([plain], {type: file.mime}));
    owner._attachmentURLs ||= new Set();
    owner._attachmentURLs.add(url);
    const link = document.createElement("a");
    link.textContent = "Download";
    link.download = file.name;
    link.rel = "noopener";
    link.href = url;
    return link;
  }

  function preview(node, plain, file, owner = node) {
    const mime = file.mime.toLowerCase().split(";", 1)[0];
    if (mime === "text/plain" || mime === "text/markdown" || mime === "application/json") {
      const output = document.createElement("pre");
      output.textContent = textDecoder.decode(plain.slice(0, 1024 * 1024));
      const footer = document.createElement("p");
      footer.append(downloadLink(owner, file, plain));
      if (plain.length > 1024 * 1024) footer.append(" (preview truncated)");
      node.replaceChildren(output, footer);
      return;
    }
    if (mime.startsWith("image/") || mime.startsWith("video/") || mime.startsWith("audio/")) {
      const media = document.createElement(mime.startsWith("image/") ? "img" : mime.startsWith("video/") ? "video" : "audio");
      media.controls = mime.startsWith("video/") || mime.startsWith("audio/");
      if (media.localName === "img") media.alt = file.name;
      if (media.localName === "video") media.preload = "metadata";
      const url = URL.createObjectURL(new Blob([plain], {type: file.mime}));
      media.src = url;
      owner._attachmentURLs ||= new Set();
      owner._attachmentURLs.add(url);
      const footer = document.createElement("p");
      footer.append(downloadLink(owner, file, plain));
      node.replaceChildren(media, footer);
      return;
    }
    const output = document.createElement("p");
    output.append("Decrypted attachment is ready: ", downloadLink(owner, file, plain));
    node.replaceChildren(output);
  }

  class DirectFile extends HTMLElement {
    connectedCallback() {
      this.revokeAttachments();
      this.generation = (this.generation || 0) + 1;
      const generation = this.generation;
      let file;
      try { file = parse(this.rumor); } catch (error) { this.textContent = error.message; return; }
      this.file = file;
      this.innerHTML = "";
      const article = document.createElement("article");
      const label = document.createElement("span");
      label.textContent = file.name + (file.size === null ? "" : " (" + sizeLabel(file.size) + ")");
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = "Open attachment";
      const status = document.createElement("output");
      button.addEventListener("click", async () => {
        button.disabled = true;
        status.textContent = "Opening attachment...";
        this.abort?.abort();
        this.abort = new AbortController();
        try {
          const plain = await decrypt(file, await fetchCiphertext(file, this.abort.signal));
          if (this.generation !== generation || !this.isConnected) return;
          preview(article, plain, file, this);
        } catch (error) { if (this.generation === generation) status.textContent = error.message; }
        finally { if (this.generation === generation) button.disabled = false; }
      });
      article.append(label, button, status);
      this.append(article);
    }
    revokeAttachments() {
      for (const url of this._attachmentURLs || []) URL.revokeObjectURL(url);
      this._attachmentURLs?.clear();
    }
    disconnectedCallback() {
      this.generation = (this.generation || 0) + 1;
      this.abort?.abort();
      this.revokeAttachments();
    }
  }

  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.chat = globalThis.tiny.chat || {};
  globalThis.tiny.chat.files = Object.freeze({parse, decrypt, render: rumor => {
    const node = document.createElement("direct-file");
    node.rumor = rumor;
    return node;
  }, MAX_BYTES});
  if (globalThis.customElements && !customElements.get("direct-file")) customElements.define("direct-file", DirectFile);
})();
