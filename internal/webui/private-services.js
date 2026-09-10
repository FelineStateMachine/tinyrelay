// PrivateServices edits the encrypted kind 10318 relay list. NIP-44 encryption
// and event signing stay with the user's signer.
(() => {
  "use strict";

  const {relayURL} = window.tiny.util;
  const {signer, nip44Encrypt: encrypt, nip44Decrypt: decrypt} = window.tiny;
  const validURL = value => relayURL(value, {strict: true}) !== null;
  const relay = value => relayURL(value, {strict: true});
  const mergeRows = (rows, urls) =>
    rows.filter(row => !(Array.isArray(row) && row[0] === "g")).concat(urls.map(value => ["g", value]));
  const publicKey = async element => {
    const known = element?.getAttribute("pubkey");
    if (/^[0-9a-f]{64}$/.test(known || "")) return known;
    const active = signer();
    if (active?.getPublicKey) return active.getPublicKey();
    if (window.tiny?.getPublicKey) return window.tiny.getPublicKey();
    throw Error("Connect a signer first.");
  };
  // readList uses the browser session when the page has one, so showing
  // the list never asks the signer; a signed query is the fallback.
  const readList = async pubkey => {
    const body = JSON.stringify([{kinds: [10318], authors: [pubkey], limit: 1}]);
    let response = null;
    if (typeof fetch === "function" && window.tiny?.localPath) {
      response = await fetch(window.tiny.localPath("/query"), {
        method: "POST",
        credentials: "same-origin",
        headers: {"content-type": "application/json"},
        body
      });
      if (response.status === 401) response = null;
    }
    if (!response) {
      response = await window.tiny.signedFetch("/query", "POST", body, {
        contentType: "application/json"
      });
    }
    if (!response || response.ok === false) throw Error("Could not read the private relay list.");
    const rows = await response.json();
    if (!Array.isArray(rows) || rows.length > 1) throw Error("The private relay list response is malformed.");
    const event = rows[0];
    if (rows.length && !acceptEvent(event, pubkey)) throw Error("The private relay list event is invalid.");
    return event;
  };
  const acceptEvent = (event, expectedPubkey) => {
    if (
      !event ||
      event.kind !== 10318 ||
      event.pubkey !== expectedPubkey ||
      !Array.isArray(event.tags) ||
      typeof event.content !== "string"
    )
      return false;
    if (typeof window.NostrSigner?.verifyEvent !== "function") return false;
    try {
      return window.NostrSigner.verifyEvent(event) === true;
    } catch {
      return false;
    }
  };
  const publish = async event => {
    const response = await window.tiny.signedFetch("/events", "POST", JSON.stringify(event), {
      contentType: "application/json"
    });
    const text = await response.text();
    let value;
    try {
      value = JSON.parse(text);
    } catch {
      value = undefined;
    }
    if (value?.error || value?.accepted === false)
      throw Error(value.error || value.message || "The relay rejected the event.");
  };
  const wireEvent = content => ({
    kind: 10318,
    created_at: Math.floor(Date.now() / 1000),
    tags: [],
    content
  });

  class PrivateServices extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.form = this.querySelector("form");
      this.lines = this.querySelector("textarea[name=lines]");
      this.output = this.querySelector("output");
      this.form?.addEventListener("submit", event => {
        event.preventDefault();
        if (this.busy) return;
        this.busy = true;
        this.form.querySelector("button").disabled = true;
        this.save()
          .catch(error => this.say("Error: " + error.message, true))
          .finally(() => {
            this.busy = false;
            this.form.querySelector("button").disabled = false;
          });
      });
      this.querySelector("[data-decrypt]")?.addEventListener("click", () => {
        this.whenSigned(() => this.reveal().catch(error => this.say("Error: " + error.message, true)));
      });
      this.load()
        .then(() => {
          this.form.querySelector("button").disabled = false;
        })
        .catch(error => this.say("Error: " + error.message, true));
    }

    // whenSigned runs once a signer is available. The bridge announces a
    // connected or resumed remote signer with the tiny:signer event.
    whenSigned(fn) {
      if (signer()?.signEvent || window.nostr?.signEvent) return fn();
      this.say("Connect a signer to edit this list.");
      document.addEventListener("tiny:signer", () => fn(), {once: true});
    }

    say(text, error) {
      this.output.textContent = text;
      if (error) this.output.dataset.error = "";
      else delete this.output.dataset.error;
    }

    // load reads the published event without decrypting it. Decryption
    // asks the signer, so it waits for the Decrypt to edit control.
    async load() {
      this.say("Reading encrypted list…");
      const pubkey = await publicKey(this);
      this.event = await readList(pubkey);
      if (!this.event) {
        this.say("No private relay list published yet.");
        return;
      }
      const control = this.querySelector("[data-decrypt]");
      if (control) control.hidden = false;
      this.say("An encrypted list is published. Decrypt to edit it, or publish new URLs to merge them.");
    }

    async reveal() {
      const pubkey = await publicKey(this);
      const event = this.event || (await readList(pubkey));
      if (!event) {
        this.say("No private relay list published yet.");
        return;
      }
      const rows = JSON.parse(await decrypt(pubkey, event.content));
      if (!Array.isArray(rows)) throw Error("The encrypted list is not a JSON array.");
      const urls = rows
        .filter(row => Array.isArray(row) && row[0] === "g" && validURL(row[1]))
        .map(row => relay(row[1]));
      this.lines.value = urls.join("\n");
      this.say("Loaded " + urls.length + " private relay" + (urls.length === 1 ? "" : "s") + ".");
    }

    async save() {
      const rawURLs = this.lines.value
        .split(/\r?\n/)
        .map(value => value.trim())
        .filter(Boolean);
      if (rawURLs.some(value => !validURL(value)))
        throw Error("Use ws:// or wss:// URLs without credentials, queries or fragments.");
      const urls = rawURLs.map(relay);
      const unique = [...new Set(urls)];
      const pubkey = await publicKey(this);
      const existing = await readList(pubkey);
      let rows = [];
      if (existing) {
        rows = JSON.parse(await decrypt(pubkey, existing.content));
        if (!Array.isArray(rows)) throw Error("The encrypted list is not a JSON array.");
      }
      rows = mergeRows(rows, unique);
      const content = await encrypt(pubkey, JSON.stringify(rows));
      const active = signer();
      if (!active?.signEvent) throw Error("Connect a signer first.");
      const event = await active.signEvent(wireEvent(content));
      if (!acceptEvent(event, pubkey) || event.content !== content || event.tags.length !== 0)
        throw Error("The signer returned an invalid private relay list event.");
      await publish(event);
      this.say("Published " + unique.length + " private relay" + (unique.length === 1 ? "" : "s") + ".");
    }
  }

  const api = Object.freeze({
    validURL,
    canonicalURL: relay,
    mergeRows,
    encrypt,
    decrypt,
    wireEvent,
    acceptEvent,
    readList
  });
  window.tiny = window.tiny || {};
  window.tiny.files = {...window.tiny.files, privateServices: api};
  customElements.define("private-services", PrivateServices);
})();
