// Custom elements for the plain HTML pages. Every element renders in the
// light DOM with document APIs, takes plain lowercase attributes, and carries
// no styling of its own; page CSS targets the element name directly.
//
//   <rpc-form method="…" [compose="…"] [action="…"] [terms="…"] [json-params]>
//     One signed NIP-86 management call. Children become the form body; the
//     element appends an <output> status line and a <json-view> result.
//   <connect-list>
//     The home page cards: lists, reorders, retargets and removes configured
//     connections and adds from the catalog, saving with setconnections.
//   <signed-form action="…" method="PUT|POST|GET" [download]>
//     A NIP-98 signed upload of one file, or a signed download by name.
//   <publish-list kind="…" tag="…" [scheme="ws"]>
//     Signs a replaceable list event from textarea lines and posts it.
//   <json-view>
//     Renders JSON as tables, lists and definition lists, keeping the raw
//     text in a <details>. Server pages seed it with a <pre>; scripts set .value.
//     Tables sit inside a <scroll-box> so wide rows scroll sideways.
//   <nostr-key hex="…">
//     Shortened key with the full hex as title; click copies it.
//   <file-mirror action="/mirror">
//     Signed BUD-04 mirror of a URL or Blossom URI into this relay.
//   <file-tools [hash="…"] [type="…"]>
//     Copies Blossom links, encrypts and uploads a file, decrypts a share
//     link fragment and sends a random-key file as a NIP-17 message.
//   <file-workspace> and <file-workspace-root> live in file-workspace.js.
(() => {
  "use strict";

  const tiny = window.tiny;
  const el = (name, text) => {
    const node = document.createElement(name);
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const isHex64 = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
  const isURL = value => typeof value === "string" && /^(https?|wss?):\/\/\S+$/.test(value);
  const timeKey = /(^|_)(at|time|created|updated|expires|started|finished|since|until)$/;
  const isTimestamp = (key, value) => Number.isInteger(value) && value > 1e9 && value < 4e9 && timeKey.test(String(key || ""));
  const isPlain = value => value === null || typeof value !== "object";

  // FormElement wraps light DOM children in a real <form> so labels, Enter
  // to submit, and required validation work natively, then adds a status line.
  class FormElement extends HTMLElement {
    connectedCallback() {
      if (this.form) return;
      const form = el("form");
      form.append(...this.childNodes);
      this.form = form;
      this.output = el("output");
      this.output.setAttribute("role", "status");
      this.append(form, this.output);
      form.addEventListener("submit", async event => {
        event.preventDefault();
        if (this.submitting) return;
        this.submitting = true;
        this.busy(true);
        try {
          await this.submit(form);
        } catch (err) {
          this.report("Error: " + err.message, true);
        } finally {
          this.submitting = false;
          this.busy(false);
        }
      });
    }

    busy(on) {
      this.form.querySelectorAll("button").forEach(button => { button.disabled = on; });
    }

    report(text, error) {
      this.output.textContent = text;
      if (error) this.output.dataset.error = "";
      else delete this.output.dataset.error;
    }
  }

  const list = value => value.split(",").map(item => item.trim()).filter(Boolean);
  const composers = {
    member: f => [f.elements["member-pubkey"].value, {name: f.elements["member-name"].value, note: f.elements["member-note"].value, role: f.elements["member-role"].value}],
    memberInvites: f => [{memberInvites: {depth: Number(f.elements["member-invite-depth"].value), quota: Number(f.elements["member-invite-quota"].value)}}],
    succession: f => [{heir: f.elements["succession-heir"].value, afterDays: Number(f.elements["succession-days"].value)}],
    identity: f => [{
      contact: f.elements["identity-contact"].value,
      banner: f.elements["identity-banner"].value,
      postingPolicy: f.elements["identity-posting"].value,
      privacyPolicy: f.elements["identity-privacy"].value,
      tags: list(f.elements["identity-tags"].value),
      languageTags: list(f.elements["identity-languages"].value),
      relayCountries: list(f.elements["identity-countries"].value)
    }]
  };

  // rpc makes one signed management call: the body is serialised once,
  // signed as NIP-98, and sent once. Returns the result or throws the error.
  const rpc = async (method, params = [], action) => {
    const body = JSON.stringify({method, params});
    const endpoint = tiny.localPath(action || "/manage/rpc");
    const url = new URL(endpoint, location.href).href;
    const response = await fetch(endpoint, {
      method: "POST",
      headers: {"content-type": "application/json", authorization: await tiny.authorization(url, "POST", new TextEncoder().encode(body))},
      body
    });
    const text = await response.text();
    let payload;
    try { payload = JSON.parse(text); } catch { payload = undefined; }
    if (!response.ok || payload?.error) throw Error(payload?.error || text || "Request failed (" + response.status + ")");
    return payload && "result" in payload ? payload.result : payload ?? text;
  };

  class RpcForm extends FormElement {
    params(form) {
      const compose = composers[this.getAttribute("compose")];
      if (compose) return compose(form);
      let params = [...form.querySelectorAll("[name=param]")].map(input => {
        try { return JSON.parse(input.value); } catch { return input.value; }
      });
      if (this.hasAttribute("json-params") && params.length === 1 && Array.isArray(params[0])) params = params[0];
      return params;
    }

    async submit(form) {
      const method = this.getAttribute("method") || form.elements.method?.value;
      if (!method) throw Error("Choose a method.");
      let params = this.params(form);
      if (method === "claiminvite") {
        const agree = form.querySelector("[name=termsAgreement]");
        if (agree && !agree.checked) { this.report("Agree to the relay terms before joining."); return; }
        const terms = this.getAttribute("terms");
        if (terms) params = [params[0], await tiny.sha256hex(new TextEncoder().encode(terms))];
      }
      this.report("Signing…");
      const result = await rpc(method, params, this.getAttribute("action"));
      this.report("Done.");
      this.show(result);
    }

    show(result) {
      if (!this.result) {
        this.result = el("json-view");
        this.append(this.result);
      }
      this.result.value = result;
    }
  }

  class SignedForm extends FormElement {
    async submit(form) {
      const action = this.getAttribute("action");
      const method = (this.getAttribute("method") || "POST").toUpperCase();
      if (this.hasAttribute("download")) {
        const name = form.elements.name.value.trim();
        if (!name) return;
        this.report("Signing…");
        const response = await tiny.signedFetch(action + encodeURIComponent(name), "GET", new Uint8Array());
        const href = URL.createObjectURL(await response.blob());
        const anchor = el("a");
        anchor.href = href;
        anchor.download = name;
        anchor.click();
        setTimeout(() => URL.revokeObjectURL(href), 1000);
        this.report("Downloaded " + name + ".");
        return;
      }
      const file = form.querySelector("input[type=file]")?.files[0];
      if (!file) { this.report("Choose a file first."); return; }
      this.report("Signing…");
      const response = await tiny.signedFetch(action, method, await file.arrayBuffer(), {contentType: file.type || "application/octet-stream"});
      this.report("Done: " + await response.text());
    }
  }

  class FileMirror extends FormElement {
    async submit(form) {
      let source = form.elements.namedItem("url").value.trim();
      if (!source) throw Error("Enter a URL or Blossom URI.");
      if (/^blossom:/i.test(source) && new URLSearchParams(source.split("?", 2)[1] || "").has("k")) throw Error("Encrypted Blossom URIs keep k client side; paste the public URI without its key.");
      if (/^https?:/i.test(source)) { const url = new URL(source); url.hash = ""; source = url.href; }
      const action = this.getAttribute("action") || "/mirror";
      const body = JSON.stringify({url: source});
      this.report("Signing…");
      const response = await tiny.signedFetch(action, "PUT", body, {contentType: "application/json"});
      let result;
      try { result = await response.json(); } catch { result = await response.text(); }
      this.report("Stored.");
      const view = el("json-view");
      view.value = result;
      this.append(view);
    }

    connectedCallback() {
      if (this.form) return;
      this.innerHTML = '<form><label>URL or Blossom URI <input name="url" type="text" placeholder="https://… or blossom:…" required></label><button>Mirror</button></form>';
      super.connectedCallback();
    }
  }

  const b64url = bytes => {
    let binary = "";
    bytes.forEach(byte => { binary += String.fromCharCode(byte); });
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const fromB64url = value => {
    const text = value.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((value.length + 3) % 4);
    return Uint8Array.from(atob(text), char => char.charCodeAt(0));
  };

  class FileTools extends HTMLElement {
    disconnectedCallback() {
      if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
      this.uploadTask?.cancel?.();
      this.uploadController?.abort();
      this.uploadCanceled = true;
    }

    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.innerHTML = '<h3>File tools</h3><p><button type="button" data-copy>Copy Blossom URI</button> <button type="button" data-link>Copy share link</button></p><form><label>Encrypt a file <input type="file" name="file"></label><label>Encryption <select name="scheme"><option value="random">Random key</option><option value="chk">Deduplicated key</option></select></label><button>Encrypt and upload</button></form><p><button type="button" data-upload-cancel disabled>Cancel upload</button> <button type="button" data-upload-retry disabled>Retry upload</button></p><p><small>Random keys keep identical files distinct. Deduplication reveals matching files and allows guesses about predictable content.</small></p><form data-share><label>Send to <input name="recipient" inputmode="text" placeholder="npub or hex pubkey"></label><button>Send privately</button></form><label>Share link <input data-share-url readonly></label><output role="status"></output>';
      this.output = this.querySelector("output");
      this.querySelector("[data-copy]").addEventListener("click", () => this.copy(this.blossomURI()));
      this.querySelector("[data-link]").addEventListener("click", () => this.copy(this.shareLink()));
      this.querySelector("form:not([data-share])").addEventListener("submit", event => { event.preventDefault(); this.encryptUpload(event.currentTarget); });
      this.querySelector("[data-upload-cancel]").addEventListener("click", () => { this.uploadCanceled = true; this.uploadTask?.cancel?.(); this.uploadController?.abort(); });
      this.querySelector("[data-upload-retry]").addEventListener("click", () => this.retryUpload());
      this.querySelector("[data-share]").addEventListener("submit", event => { event.preventDefault(); this.share(event.currentTarget); });
      this.updateFileControls();
      this.decryptFromFragment();
    }

    hash() { return this.fileHash || this.getAttribute("hash") || ""; }
    type() { return this.getAttribute("type") || "application/octet-stream"; }
    updateFileControls() {
      const available = Boolean(this.hash());
      this.querySelectorAll("[data-copy], [data-link], [data-share] button").forEach(button => { button.disabled = !available; });
    }
    blossomURI() {
      const params = new URLSearchParams(this.fileFragment || location.hash.slice(1));
      const extension = params.has("manifest") ? (["2", "3"].includes(params.get("node") || "2") ? "bdir" : params.get("node") === "1" ? "bfile" : "bin") : "bin";
      const uri = (params.get("enc") === "chk-v1" || params.has("manifest")) && params.get("key") ? TinyBlossomEncryption.createURI(this.hash(), extension, params.get("key")) : "blossom:" + this.hash();
      return tiny.root ? uri : uri + (uri.includes("?") ? "&" : "?") + "xs=" + encodeURIComponent(new URL(location.href).host);
    }
    shareLink() {
      if (this.fileShareLink) return this.fileShareLink;
      const fragment = this.fileFragment || location.hash.slice(1);
      const params = new URLSearchParams(fragment);
      if ((params.get("key") && params.get("iv")) || ((params.get("enc") === "chk-v1" || params.has("manifest")) && params.get("key"))) return location.href.split("#", 1)[0] + "#" + fragment;
      return location.href.split("#", 1)[0] + "#blossom=" + encodeURIComponent(this.hash());
    }
    async copy(value) {
      try { await navigator.clipboard.writeText(value); this.say("Copied."); }
      catch { this.say(value); }
    }
    say(value, error) { this.output.textContent = value; if (error) this.output.dataset.error = ""; else delete this.output.dataset.error; }

    uploadBusy(value) {
      this.busy = value;
      this.querySelector("[data-upload-cancel]").disabled = !value;
      this.querySelector("[data-upload-retry]").disabled = value || !this.pendingUpload;
    }

    async encryptUpload(form) {
      if (this.busy) return;
      const file = form.elements.namedItem("file").files[0];
      if (!file) { this.say("Choose a file first.", true); return; }
      this.uploadBusy(true);
      this.uploadCanceled = false;
      try {
        if (file.size > 256 * 1024 * 1024) throw Error("Encrypted browser uploads are limited to 256 MiB.");
        this.say("Encrypting and signing…");
        const scheme = form.elements.namedItem("scheme").value;
        const plaintext = new Uint8Array(await file.arrayBuffer());
        const plaintextHash = await tiny.sha256hex(plaintext);
        let ciphertext, fragment;
        if (scheme === "chk") {
          if (!globalThis.TinyBlossomEncryption) throw Error("CHK encryption is unavailable. Reload this page.");
          const encrypted = await TinyBlossomEncryption.encryptCHK(plaintext);
          ciphertext = encrypted.ciphertext;
          fragment = "enc=chk-v1&key=" + encrypted.key;
        } else {
          const key = await crypto.subtle.generateKey({name: "AES-GCM", length: 256}, true, ["encrypt", "decrypt"]);
          const iv = crypto.getRandomValues(new Uint8Array(12));
          ciphertext = new Uint8Array(await crypto.subtle.encrypt({name: "AES-GCM", iv}, key, plaintext));
          const rawKey = new Uint8Array(await crypto.subtle.exportKey("raw", key));
          fragment = "key=" + b64url(rawKey) + "&iv=" + b64url(iv);
        }
        fragment += "&name=" + encodeURIComponent(file.name) + "&type=" + encodeURIComponent(file.type || "application/octet-stream") + "&ox=" + plaintextHash;
        this.pendingUpload = {ciphertext, fragment, file, state: new Map()};
        if (this.uploadCanceled) throw Error("Upload canceled.");
        await this.storePreparedUpload();
      } catch (error) { this.say("Upload stopped: " + error.message + (this.pendingUpload ? " Retry while this page stays open." : ""), true); }
      finally { this.uploadBusy(false); }
    }

    async retryUpload() {
      if (this.busy || !this.pendingUpload) return;
      this.uploadBusy(true);
      this.uploadCanceled = false;
      try { await this.storePreparedUpload(); }
      catch (error) { this.say("Upload stopped: " + error.message + " Retry while this page stays open.", true); }
      finally { this.uploadBusy(false); }
    }

    async storePreparedUpload() {
      const {ciphertext, fragment, file, state} = this.pendingUpload;
      const actualHash = await tiny.sha256hex(ciphertext);
      this.uploadController = new AbortController();
      let descriptor;
      if (globalThis.TinyBlossomUpload?.upload) {
        const task = TinyBlossomUpload.upload(ciphertext, {
          url: new URL(tiny.localPath("/"), location.href).href, hash: actualHash,
          type: "application/octet-stream", state, signal: this.uploadController.signal,
          authorize: (url, verb, body) => tiny.authorization(url, verb, body),
          onProgress: progress => this.say("Uploading " + Math.round(progress.fraction * 100) + "%…")
        });
        this.uploadTask = task;
        descriptor = (await task).descriptor;
      } else {
        const response = await tiny.signedFetch("/upload", "PUT", ciphertext, {contentType: "application/octet-stream", signal: this.uploadController.signal});
        descriptor = await response.json();
      }
      if (descriptor.sha256 !== actualHash) throw Error("relay returned an unexpected hash");
      this.fileHash = actualHash;
      this.fileFragment = fragment;
      this.setAttribute("hash", actualHash);
      this.setAttribute("type", file.type || "application/octet-stream");
      this.setAttribute("size", String(ciphertext.length));
      this.updateFileControls();
      const link = new URL(location.href);
      link.pathname = location.pathname.replace(/\/files?$/, "/file");
      link.search = "?hash=" + actualHash;
      link.hash = fragment;
      this.fileShareLink = link.href;
      const share = this.querySelector("[data-share-url]");
      if (share) share.value = link.href;
      this.pendingUpload = null;
      this.uploadTask = null;
      try {
        if (!navigator.clipboard?.writeText) throw Error("Clipboard unavailable");
        await navigator.clipboard.writeText(link.href);
        this.say("Encrypted file stored. Share link copied; the key is only in its fragment.");
      } catch {
        if (share) share.select?.();
        this.say("Encrypted file stored. Copy the share link above; the key is only in its fragment.");
      }
    }

    async decryptFromFragment() {
      const params = new URLSearchParams(this.fileFragment || location.hash.slice(1));
      if ((!params.get("key") || !params.get("iv")) && !(params.get("enc") === "chk-v1" && params.get("key"))) return;
      try {
        this.say("Decrypting…");
        const response = await fetch(tiny.localPath("/files/raw?hash=" + encodeURIComponent(this.hash())));
        if (!response.ok) throw Error("download failed (" + response.status + ")");
        const encrypted = new Uint8Array(await response.arrayBuffer());
        if (await tiny.sha256hex(encrypted) !== this.hash()) throw Error("ciphertext hash mismatch");
        let plain;
        if (params.get("enc") === "chk-v1") plain = await TinyBlossomEncryption.decryptCHK(encrypted, params.get("key"), this.hash());
        else {
          const key = await crypto.subtle.importKey("raw", fromB64url(params.get("key")), "AES-GCM", false, ["decrypt"]);
          plain = await crypto.subtle.decrypt({name: "AES-GCM", iv: fromB64url(params.get("iv"))}, key, encrypted);
        }
        if (params.get("ox") && await tiny.sha256hex(plain) !== params.get("ox")) throw Error("plaintext hash mismatch");
        if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
        const link = el("a"); this.downloadURL = URL.createObjectURL(new Blob([plain], {type: params.get("type") || this.type()})); link.href = this.downloadURL; link.download = params.get("name") || "decrypted-file"; link.textContent = "download decrypted file"; this.append(link); this.say("Decrypted locally.");
      } catch (error) { this.say("Could not decrypt this file: " + error.message, true); }
    }

    async share(form) {
      if (this.sharing) return;
      const recipient = form.elements.namedItem("recipient").value.trim();
      if (!recipient) { this.say("Enter a public key or npub.", true); return; }
      const params = new URLSearchParams(this.fileFragment || location.hash.slice(1));
      const signer = globalThis.tinySigner || globalThis.nostr;
      if (!globalThis.TinyFileMessages || !signer) { this.say("Connect a NIP-44 signer first. Use the encrypted link, which keeps its key in the URL fragment.", true); return; }
      if (!params.get("key") || !params.get("iv")) { this.say("NIP-17 sharing currently requires a random AES-GCM encrypted file.", true); return; }
      this.sharing = true;
      try {
        const key = fromB64url(params.get("key")), nonce = fromB64url(params.get("iv"));
        if (key.length !== 32 || nonce.length !== 12) throw Error("Invalid file key or nonce.");
        const hex = bytes => Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("");
        this.say("Preparing sealed NIP-17 delivery…");
        await TinyFileMessages.share({recipient, fileURL: new URL(tiny.localPath("/files/raw?hash=" + this.hash()), location.href).href, ciphertextHash: this.hash(), plaintextHash: params.get("ox") || undefined, key: hex(key), nonce: hex(nonce), mimeType: params.get("type") || this.type(), size: Number(this.getAttribute("size") || 0) || undefined}, {signer});
        this.say("Encrypted message delivered; sender copy saved.");
      } catch (error) { this.say("NIP-17 delivery failed: " + error.message, true); } finally { this.sharing = false; }
    }
  }

  class PublishList extends FormElement {
    async submit(form) {
      if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
      const tag = this.getAttribute("tag");
      const scheme = this.getAttribute("scheme");
      const lines = form.elements.lines.value.split(/\r?\n/).map(line => line.trim()).filter(Boolean)
        .filter(line => scheme !== "ws" || /^wss?:\/\//i.test(line));
      const event = await window.nostr.signEvent({
        kind: Number(this.getAttribute("kind")),
        created_at: Math.floor(Date.now() / 1000),
        tags: lines.map(line => [tag, line]),
        content: ""
      });
      const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event));
      let result = null;
      try { result = await response.clone().json(); } catch {}
      if (result?.error || result?.accepted === false) throw Error(result?.error || result?.message || "The relay rejected the event.");
      this.report("Published.");
    }
  }

  class NostrKey extends HTMLElement {
    static get observedAttributes() { return ["hex"]; }

    connectedCallback() {
      this.update();
      if (this.bound) return;
      this.bound = true;
      this.addEventListener("click", () => {
        navigator.clipboard?.writeText(this.getAttribute("hex") || "").then(() => {
          const title = this.title;
          this.title = "Copied";
          setTimeout(() => { this.title = title; }, 1200);
        }).catch(() => {});
      });
    }

    attributeChangedCallback() { this.update(); }

    update() {
      const hex = this.getAttribute("hex") || "";
      this.title = hex;
      if (!this.textContent.trim()) this.textContent = hex.slice(0, 12);
    }
  }

  // JsonView renders any JSON value. Arrays of objects become tables, objects
  // become definition lists, scalar arrays become lists, and deep nesting
  // falls back to compact JSON so a response never explodes the page.
  class JsonView extends HTMLElement {
    connectedCallback() {
      if (this.rendered) return;
      const seed = this.querySelector(":scope > pre");
      if (!seed) return;
      try { this.render(JSON.parse(seed.textContent), seed); } catch { /* leave the raw text */ }
    }

    set value(value) { this.render(value); }

    render(value, seed) {
      this.rendered = true;
      const raw = seed || el("pre");
      raw.textContent = JSON.stringify(value, null, 2);
      const details = el("details");
      details.append(el("summary", "JSON"), raw);
      this.replaceChildren(this.node(value, 0), details);
    }

    node(value, depth, key) {
      if (isPlain(value)) return this.scalar(value, key);
      if (depth >= 3) return el("code", JSON.stringify(value));
      if (Array.isArray(value)) {
        if (!value.length) return el("code", "[]");
        if (value.every(item => item && typeof item === "object" && !Array.isArray(item))) return this.table(value, depth);
        const items = el("ul");
        value.forEach(item => { const li = el("li"); li.append(this.node(item, depth + 1)); items.append(li); });
        return items;
      }
      const keys = Object.keys(value);
      if (!keys.length) return el("code", "{}");
      const dl = el("dl");
      keys.forEach(name => {
        const dd = el("dd");
        dd.append(this.node(value[name], depth + 1, name));
        dl.append(el("dt", name), dd);
      });
      return dl;
    }

    table(rows, depth) {
      const columns = [];
      rows.forEach(row => Object.keys(row).forEach(name => { if (!columns.includes(name)) columns.push(name); }));
      const table = el("table");
      const head = el("tr");
      columns.forEach(name => { const th = el("th", name); th.setAttribute("scope", "col"); head.append(th); });
      const thead = el("thead");
      thead.append(head);
      const tbody = el("tbody");
      rows.forEach(row => {
        const tr = el("tr");
        columns.forEach(name => {
          const td = el("td");
          if (name in row) td.append(isPlain(row[name]) ? this.scalar(row[name], name) : el("code", JSON.stringify(row[name])));
          tr.append(td);
        });
        tbody.append(tr);
      });
      table.append(thead, tbody);
      const box = el("scroll-box");
      box.append(table);
      return box;
    }

    scalar(value, key) {
      if (value === null || value === undefined) return el("code", "null");
      if (typeof value === "boolean") return el("code", String(value));
      if (typeof value === "number") {
        if (!isTimestamp(key, value)) return el("code", String(value));
        const time = el("time", new Date(value * 1000).toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC"));
        time.dateTime = new Date(value * 1000).toISOString();
        return time;
      }
      if (isHex64(value)) {
        const node = el("nostr-key", value.slice(0, 12));
        node.setAttribute("hex", value);
        return node;
      }
      if (isURL(value)) {
        const link = el("a", value);
        link.href = value;
        link.rel = "noopener";
        return link;
      }
      if (value.length > 160 || value.includes("\n")) return el("pre", value);
      return document.createTextNode(value);
    }
  }

  // RelayLists reads the signed-in key's relay lists from this relay, says
  // whether this relay is on each, and republishes a list with this relay
  // added. Lists are replaceable events, so the new one carries every
  // existing entry plus this relay. Each publish goes to this relay and to
  // the relays already on the list so other clients find the update.
  class RelayLists extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.output = this.querySelector("output");
      this.addEventListener("click", event => {
        const button = event.target.closest("button[name]");
        const row = button?.closest("tr[data-kind]");
        if (!button || !row) return;
        button.disabled = true;
        const action = button.name === "add" ? this.add(row) : this.check(row);
        action.catch(err => this.say(row, "error: " + err.message)).finally(() => { button.disabled = false; });
      });
    }

    say(row, text) {
      let note = row.querySelector("output");
      if (!note) { note = el("output"); row.lastElementChild.append(" ", note); }
      note.textContent = text;
    }

    value(row) { return row.dataset.value === "server" ? this.getAttribute("server") : this.getAttribute("relay"); }

    normal(value) { return String(value || "").trim().replace(/\/+$/, "").toLowerCase(); }

    // socket opens one websocket, runs fn with send and a message queue, then closes.
    socket(url, fn, timeout = 8000) {
      return new Promise((resolve, reject) => {
        let ws;
        try { ws = new WebSocket(url); } catch (err) { reject(err); return; }
        const timer = setTimeout(() => { ws.close(); reject(Error("timed out talking to " + url)); }, timeout);
        const done = (value, err) => { clearTimeout(timer); ws.close(); err ? reject(err) : resolve(value); };
        ws.addEventListener("error", () => done(null, Error("could not reach " + url)));
        ws.addEventListener("open", () => fn(message => ws.send(JSON.stringify(message)), handler => ws.addEventListener("message", e => handler(JSON.parse(e.data))), done));
      });
    }

    current(row) {
      const kind = Number(row.dataset.kind);
      return this.socket(this.getAttribute("relay"), (send, onMessage, done) => {
        let latest = null;
        onMessage(message => {
          if (message[0] === "EVENT" && message[2]?.kind === kind && (!latest || message[2].created_at > latest.created_at)) latest = message[2];
          if (message[0] === "EOSE") done(latest);
        });
        send(["REQ", "lists", {kinds: [kind], authors: [this.getAttribute("pubkey")], limit: 1}]);
      });
    }

    entries(event, tag) {
      return (event?.tags || []).filter(t => t[0] === tag).map(t => t[1]);
    }

    async check(row) {
      this.say(row, "checking…");
      const event = await this.current(row);
      if (!event) { this.say(row, "no list published here yet"); return; }
      const listed = this.entries(event, row.dataset.tag).some(v => this.normal(v) === this.normal(this.value(row)));
      this.say(row, listed ? "listed" : "not listed, " + this.entries(event, row.dataset.tag).length + " other entries");
    }

    async add(row) {
      if (!window.nostr?.signEvent) throw Error("connect a signer first");
      this.say(row, "reading…");
      const existing = await this.current(row);
      const value = this.value(row);
      const tags = (existing?.tags || []).filter(t => t.length);
      if (!this.entries(existing, row.dataset.tag).some(v => this.normal(v) === this.normal(value))) tags.push([row.dataset.tag, value]);
      const event = await window.nostr.signEvent({kind: Number(row.dataset.kind), created_at: Math.floor(Date.now() / 1000), content: existing?.content || "", tags});
      const targets = [this.getAttribute("relay")];
      for (const t of tags) if ((t[0] === "r" || t[0] === "relay") && /^wss?:\/\//i.test(t[1]) && !targets.includes(t[1])) targets.push(t[1]);
      let accepted = 0;
      const failures = [];
      for (const url of targets) {
        try {
          await this.socket(url, (send, onMessage, done) => {
            onMessage(message => { if (message[0] === "OK" && message[1] === event.id) message[2] ? done(true) : done(null, Error(message[3] || "rejected")); });
            send(["EVENT", event]);
          }, 6000);
          accepted++;
        } catch (err) {
          failures.push(url.replace(/^wss?:\/\//, ""));
        }
      }
      if (!accepted) throw Error("no relay accepted the list: " + failures.join(", "));
      this.say(row, "listed, published to " + accepted + " of " + targets.length + " relays" + (failures.length ? " (failed: " + failures.join(", ") + ")" : ""));
    }
  }

  // ConnectList edits the cards on the home page: the configured connections
  // in order, each with its audience, plus add from the catalog. Every change
  // saves the whole list with one signed call.
  class ConnectList extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.body = this.querySelector("tbody");
      this.select = this.querySelector("select[name=template]");
      this.output = this.querySelector("output");
      this.rows = [];
      this.catalog = [];
      this.addEventListener("click", event => {
        const button = event.target.closest("button[name]");
        if (!button) return;
        button.disabled = true;
        this.change(button.name, button.closest("tr")).catch(err => this.say("error: " + err.message, true)).finally(() => { button.disabled = false; });
      });
      this.addEventListener("change", event => {
        const select = event.target.closest("select[name=visibility]");
        if (select) this.change("visibility", select.closest("tr"), select.value).catch(err => this.say("error: " + err.message, true));
      });
      this.whenSigned(() => this.load().catch(err => this.say("error: " + err.message, true)));
    }

    // whenSigned waits briefly for a signer, since a remote one resumes async.
    whenSigned(fn, tries = 20) {
      if (window.nostr?.signEvent) return fn();
      if (tries <= 0) { this.say("connect a signer, then reload this page"); return; }
      setTimeout(() => this.whenSigned(fn, tries - 1), 500);
    }

    say(text, error) {
      this.output.textContent = text;
      if (error) this.output.dataset.error = "";
      else delete this.output.dataset.error;
    }

    async load() {
      this.say("loading…");
      [this.rows, this.catalog] = await Promise.all([rpc("listconnections"), rpc("listconnectiontemplates")]);
      this.render();
      this.say("");
    }

    entry(name) { return this.catalog.find(c => c.name === name) || {name, title: name, visibility: "public", available: true}; }

    render() {
      const audiences = ["public", "members", "owner"];
      const rows = this.rows.map((row, index) => {
        const name = row.template || row.name;
        const c = this.entry(name);
        const tr = el("tr");
        tr.dataset.index = index;
        tr.dataset.template = name;
        const app = el("td");
        if (c.app) app.append(el("b", c.app), " ");
        app.append(c.about || "");
        if (c.available === false) app.append(" ", el("small", "(feature off, hidden)"));
        const audience = el("select");
        audience.name = "visibility";
        const current = row.visibility === "auth" || row.visibility === "member" ? "members" : row.visibility || c.visibility;
        for (const value of audiences.includes(current) ? audiences : [current, ...audiences]) {
          const option = el("option", value);
          option.value = value;
          option.selected = value === current;
          audience.append(option);
        }
        const actions = el("td");
        for (const [label, action] of [["up", "up"], ["down", "down"], ["remove", "remove"]]) {
          const button = el("button", label);
          button.type = "button";
          button.name = action;
          if ((action === "up" && index === 0) || (action === "down" && index === this.rows.length - 1)) button.disabled = true;
          actions.append(button, " ");
        }
        const cell = el("td");
        cell.append(audience);
        tr.append(el("td", c.title || name), app, cell, actions);
        return tr;
      });
      if (!rows.length) {
        const empty = el("tr");
        const cell = el("td", "No cards. Add one from the catalog below.");
        cell.colSpan = 4;
        empty.append(cell);
        rows.push(empty);
      }
      this.body.replaceChildren(...rows);
      const used = new Set(this.rows.map(row => row.template || row.name));
      const options = [el("option", "choose one")];
      options[0].value = "";
      for (const c of this.catalog) {
        if (used.has(c.name)) continue;
        const option = el("option", c.title + (c.app ? " (" + c.app + ")" : "") + (c.available === false ? ", feature off" : ""));
        option.value = c.name;
        options.push(option);
      }
      this.select.replaceChildren(...options);
    }

    async change(action, row, value) {
      const index = Number(row?.dataset.index);
      const next = this.rows.map(entry => ({...entry}));
      switch (action) {
        case "remove": next.splice(index, 1); break;
        case "up": if (index > 0) [next[index - 1], next[index]] = [next[index], next[index - 1]]; break;
        case "down": if (index < next.length - 1) [next[index + 1], next[index]] = [next[index], next[index + 1]]; break;
        case "visibility": next[index].visibility = value; break;
        case "add": {
          const name = this.select.value;
          if (!name) { this.say("choose a card to add"); return; }
          next.push({template: name, visibility: this.entry(name).visibility || "public"});
          break;
        }
        default: return;
      }
      this.say("saving…");
      this.rows = await rpc("setconnections", [next]);
      this.render();
      this.say("saved");
    }
  }

  // ConnectCard copies a command or address, filling {input:name} from the
  // card's inputs, so a clone command carries the repository name typed.
  class ConnectCard extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.addEventListener("click", async event => {
        const button = event.target.closest("button[data-copy]");
        if (!button) return;
        const text = button.dataset.copy.replace(/\{input:([a-z]+)\}/g, (match, name) => this.querySelector(`input[name="${name}"]`)?.value.trim() || "<" + name + ">");
        const output = this.querySelector("output");
        try {
          await navigator.clipboard.writeText(text);
          if (output) output.textContent = "copied: " + text;
        } catch {
          if (output) output.textContent = text;
        }
      });
    }
  }

  customElements.define("rpc-form", RpcForm);
  customElements.define("connect-card", ConnectCard);
  customElements.define("connect-list", ConnectList);
  customElements.define("relay-lists", RelayLists);
  customElements.define("signed-form", SignedForm);
  customElements.define("file-mirror", FileMirror);
  customElements.define("file-tools", FileTools);
  customElements.define("publish-list", PublishList);
  customElements.define("nostr-key", NostrKey);
  customElements.define("json-view", JsonView);

  // Fixi normally swaps a small target. Internal page links return the full
  // shell, so use one shared swap that keeps the shell's live nodes and makes
  // navigation behave like a normal document request. The links retain their
  // hrefs, which is the no-JavaScript fallback and keeps them bookmarkable.
  (() => {
    document.documentElement.setAttribute("data-js", "yes");
    const navTargets = ["#topbar", "#railbox", "#content", "#panel", "#footer"];
    const status = () => document.querySelector("#navigation-status");
    const sameDocument = href => {
      let url;
      try { url = new URL(href, location.href); } catch { return false; }
      if (url.origin !== location.origin || url.hash || url.protocol !== location.protocol) return false;
      if (!allowedRoute(routePath(url))) return false;
      return !/\.(?:json|xml|rss|atom|txt|svg|ico|webmanifest|js|css|png|jpg|jpeg|gif|webp|zip|tar)$/i.test(url.pathname);
    };
    const setBusy = busy => {
      document.querySelector("#content")?.setAttribute("aria-busy", busy ? "true" : "false");
      const node = status();
      if (node && (busy || node.textContent === "loading")) node.textContent = busy ? "loading" : "";
    };
    const routePath = url => {
      const root = (location.pathname.match(/^\/r\/[^/]+/) || [""])[0];
      if (root && !(url.pathname === root || url.pathname.startsWith(root + "/"))) return null;
      return root ? url.pathname.slice(root.length) || "/" : url.pathname;
    };
    const allowedRoute = path => /^(?:\/(?:inbox|outbox|search|articles|private|chat|media|sites|marmot|grasp|terms|signin|connect|tools|repo|repos|file|files)?\/?|\/manage(?:\/(?:people|moderation|rules|identity|connect|data|sync|views|health|owner|status))?\/?|\/(?:invite|e|a)\/.+)$/.test(path || "");
    let navigationSerial = 0, activeAbort;
    const streams = new Set();
    const closeStreams = () => {
      streams.forEach(cfg => cfg.sse.close());
      streams.clear();
    };
    document.addEventListener("fx:sse:open", event => {
      const cfg = event.detail.cfg;
      if (!cfg.target.isConnected) { cfg.sse.close(); event.preventDefault(); return; }
      streams.add(cfg);
    });
    document.addEventListener("fx:sse:close", event => streams.delete(event.detail.cfg));
    window.addEventListener("pagehide", closeStreams);
    const repairComponents = () => {
      document.querySelectorAll("rpc-form,signed-form,publish-list").forEach(node => {
        if (node.form?.isConnected) return;
        node.form = null;
        node.output = null;
        node.connectedCallback();
        if (node.result && !node.result.isConnected) node.result = null;
      });
      document.querySelectorAll("json-view").forEach(node => {
        if (!node.rendered || !node.querySelector(":scope > pre")) return;
        node.rendered = false;
        node.connectedCallback();
      });
    };
    const swapShell = (text, url, push) => {
      if (url.__tinyNavigationSerial && url.__tinyNavigationSerial !== navigationSerial) return false;
      const parsed = new DOMParser().parseFromString(text, "text/html");
      const incoming = new Map(navTargets.map(selector => [selector, parsed.querySelector(selector)]));
      if ([...incoming.values()].some(node => !node)) return false;
      const focusID = document.activeElement?.id;
      closeStreams();
      navTargets.forEach(selector => morph(document.querySelector(selector), incoming.get(selector).outerHTML));
      repairComponents();
      document.title = parsed.title || document.title;
      if (push) history.pushState({}, "", url.href);
      const focus = focusID && document.getElementById(focusID);
      if (focus && typeof focus.focus === "function") focus.focus({preventScroll: true});
      else document.querySelector("#content")?.focus({preventScroll: true});
      document.dispatchEvent(new CustomEvent("tiny:navigation", {detail: {url: url.href}}));
      decorate();
      decorateForms();
      document.dispatchEvent(new CustomEvent("fx:process", {detail: {url: url.href}, bubbles: false}));
      return true;
    };
    const load = async (url, push) => {
      const serial = ++navigationSerial, controller = new AbortController();
      activeAbort?.();
      activeAbort = () => controller.abort();
      url.__tinyNavigationSerial = serial;
      setBusy(true);
      try {
        const response = await fetch(url.href, {headers: {"FX-Request": "true", Accept: "text/html"}, credentials: "same-origin", signal: controller.signal});
        if (!response.ok) throw Error("Navigation failed (" + response.status + ")");
        if (!swapShell(await response.text(), url, push)) throw Error("Page response did not contain the UI shell");
      } catch (error) {
        const node = status();
        if (serial === navigationSerial && node) node.textContent = "Unable to load page: " + error.message;
        throw error;
      } finally { if (serial === navigationSerial) setBusy(false); }
    };
    document.addEventListener("fx:config", event => {
      const cfg = event.detail.cfg, elt = cfg.trigger?.target?.closest?.("a[data-fixi-nav], form[data-fixi-nav]");
      if (!elt?.matches?.("a[data-fixi-nav], form[data-fixi-nav]")) return;
      if ((cfg.trigger.type === "click" && cfg.trigger.button !== 0) || cfg.trigger.metaKey || cfg.trigger.ctrlKey || cfg.trigger.shiftKey || cfg.trigger.altKey) { cfg.drop = 1; cfg.preventTrigger = false; return; }
      const serial = ++navigationSerial;
      activeAbort?.();
      activeAbort = cfg.abort;
      cfg.__tinyNavigationSerial = serial;
      cfg.target = document.querySelector("#content");
      cfg.transition = false;
      cfg.swap = async current => {
        if (serial !== navigationSerial) return;
        const url = new URL(cfg.action, location.href);
        url.__tinyNavigationSerial = serial;
        if (!cfg.response.ok || !swapShell(cfg.text, url, true)) {
          const node = status();
          if (node) node.textContent = "Unable to load page";
        }
      };
    });
    document.addEventListener("fx:before", event => {
      if (event.detail.cfg.trigger?.target?.closest?.("a[data-fixi-nav], form[data-fixi-nav]")) setBusy(true);
    });
    document.addEventListener("fx:finally", event => {
      if (event.detail.cfg.__tinyNavigationSerial === navigationSerial) setBusy(false);
    });
    document.addEventListener("fx:after", event => {
      const cfg = event.detail.cfg;
      if (cfg.__tinyNavigationSerial || cfg.response.ok) return;
      event.preventDefault();
      const node = status();
      if (node) node.textContent = cfg.response.status === 401 ? "Sign in again to load this preview." : "Unable to load preview (" + cfg.response.status + ").";
    });
    document.addEventListener("fx:error", event => {
      const cfg = event.detail.cfg;
      if (cfg.__tinyNavigationSerial && cfg.__tinyNavigationSerial !== navigationSerial) return;
      const node = status();
      if (node) node.textContent = "Unable to load page: " + event.detail.error;
    });
    const decorate = () => document.querySelectorAll("a[href]").forEach(link => {
      if (link.hasAttribute("fx-action") || link.closest("[fx-ignore]") || link.target || link.hasAttribute("download") || !sameDocument(link.href)) return;
      link.setAttribute("data-fixi-nav", "");
      link.setAttribute("fx-action", link.href);
      link.setAttribute("fx-method", "get");
      link.setAttribute("fx-trigger", "click");
    });
    const decorateForms = () => document.querySelectorAll("form[method=get][action]").forEach(form => {
      if (form.hasAttribute("fx-action") || !sameDocument(form.action)) return;
      form.setAttribute("data-fixi-nav", "");
      form.setAttribute("fx-action", form.action);
      form.setAttribute("fx-method", "get");
      form.setAttribute("fx-trigger", "submit");
    });
    const prepare = () => {
      decorate();
      decorateForms();
      // Fixi's DOMContentLoaded handler runs before this footer script. Ask
      // it to initialize the attributes added after the initial scan.
      document.dispatchEvent(new CustomEvent("fx:process"));
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", prepare, {once: true});
    else prepare();
    window.addEventListener("popstate", () => load(new URL(location.href), false).catch(() => {}));
  })();
})();
