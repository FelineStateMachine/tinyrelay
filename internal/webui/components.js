// Custom elements for the plain HTML pages. Every element renders in the
// light DOM with document APIs, takes plain lowercase attributes, and carries
// no styling of its own; page CSS targets the element name directly.
//
//   <rpc-form method="…" [compose="…"] [action="…"] [terms="…"] [json-params] [refresh]>
//     One signed NIP-86 management call. Children become the form body; the
//     element appends an <output> status line and a <json-view> result.
//     With refresh, the page reloads in place after the call succeeds.
//   <agent-grant>
//     Signs a kind 30392 agent grant from its fields. Generates the agent
//     key here and shows the nsec once, or takes a pasted public key.
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
//   <file-tools hash="…" type="…" size="…">
//     Shares one stored file: copies its Blossom URI or share link, decrypts
//     a share link fragment and sends a random-key file as a NIP-17 message.
//   <nostr-compose kind="…" [coordinate="30617:…"] [root="…" root-pubkey="…" root-kind="…"] [parent="…" …] [file="…" line="…" side="old|new"]>
//     Signs a NIP-34 issue, pull request, NIP-22 reply or status event and
//     posts it to the relay. A reply outside a repository omits the coordinate.
//     A reply under a pull request or patch may name one diff line through
//     the file, line and side attributes or hidden inputs of those names.
//   <wiki-compose name="…" [author="…" coordinate="30818:…" event="…"]>
//     Signs a NIP-54 article and posts it. Someone other than the author
//     publishes a fork of the version in view; a "propose" button also signs
//     a merge request to that author.
//   <nostr-react event="…" pubkey="…" [kind="…"]>
//     Signs a NIP-25 reaction to one event; the pressed button carries "+"
//     or "-". A request for a decision is approved or denied and a wiki
//     merge request accepted or rejected this way. The prompt method asks
//     for a visible confirmation first, which the answer link from a
//     notification uses.
//   <approval-item id="approval-…">
//     One request on the Approvals page. When the page opens with the item's
//     id and an answer in the query, it scrolls into view and offers that answer.
//   <share-link [url="…"] [title="…"]>
//     The device share sheet for the page; empty without browser support.
//   <push-toggle>
//     Opts this browser into relay notifications; asks for permission only
//     when pressed and registers the device with a signed request.
//   <file-upload> and <file-workspace-root> live in file-workspace.js;
//   <private-services> lives in private-services.js.
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
        // The button that sent the form, for elements whose buttons differ.
        this.submitter = event.submitter || null;
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
      if (this.hasAttribute("refresh") && tiny.navigate) {
        await tiny.navigate(globalThis.location?.href);
        return;
      }
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

  const {fromB64url} = tiny.util;

  // FileTools shares one stored file: copy its Blossom URI or share link,
  // send a random-key encrypted file as a NIP-17 message and decrypt a share
  // link whose key travels in the URL fragment.
  class FileTools extends HTMLElement {
    disconnectedCallback() {
      if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
    }

    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      // The encryption and messaging modules load on demand; the controls
      // render right away and decryption waits for them.
      this.ready = Promise.resolve(tiny.require?.("files")).catch(() => {});
      this.innerHTML = '<h3>Share</h3><p><button type="button" data-copy>Copy Blossom URI</button> <button type="button" data-link>Copy share link</button> <share-link></share-link></p><form data-share><label>Send to <input name="recipient" inputmode="text" placeholder="npub or hex pubkey"></label><button>Send privately</button></form><output role="status"></output>';
      this.output = this.querySelector("output");
      this.querySelector("[data-copy]").addEventListener("click", () => this.copy(this.blossomURI()));
      this.querySelector("[data-link]").addEventListener("click", () => this.copy(this.shareLink()));
      this.querySelector("[data-share]").addEventListener("submit", event => { event.preventDefault(); this.share(event.currentTarget); });
      // Private delivery needs the random key from the share link fragment.
      const params = new URLSearchParams(location.hash.slice(1));
      this.querySelector("[data-share]").hidden = !(params.get("key") && params.get("iv"));
      this.updateFileControls();
      this.ready.then(() => this.decryptFromFragment());
    }

    hash() { return this.getAttribute("hash") || ""; }
    type() { return this.getAttribute("type") || "application/octet-stream"; }
    updateFileControls() {
      const available = Boolean(this.hash());
      this.querySelectorAll("[data-copy], [data-link], [data-share] button").forEach(button => { button.disabled = !available; });
    }
    blossomURI() {
      const params = new URLSearchParams(location.hash.slice(1));
      const extension = params.has("manifest") ? (["2", "3"].includes(params.get("node") || "2") ? "bdir" : params.get("node") === "1" ? "bfile" : "bin") : "bin";
      const uri = (params.get("enc") === "chk-v1" || params.has("manifest")) && params.get("key") ? tiny.blossom.encryption.createURI(this.hash(), extension, params.get("key")) : "blossom:" + this.hash();
      return tiny.root ? uri : uri + (uri.includes("?") ? "&" : "?") + "xs=" + encodeURIComponent(new URL(location.href).host);
    }
    shareLink() {
      const fragment = location.hash.slice(1);
      const params = new URLSearchParams(fragment);
      if ((params.get("key") && params.get("iv")) || ((params.get("enc") === "chk-v1" || params.has("manifest")) && params.get("key"))) return location.href.split("#", 1)[0] + "#" + fragment;
      return location.href.split("#", 1)[0] + "#blossom=" + encodeURIComponent(this.hash());
    }
    async copy(value) {
      try { await navigator.clipboard.writeText(value); this.say("Copied."); }
      catch { this.say(value); }
    }
    say(value, error) { this.output.textContent = value; if (error) this.output.dataset.error = ""; else delete this.output.dataset.error; }

    async decryptFromFragment() {
      const params = new URLSearchParams(location.hash.slice(1));
      if ((!params.get("key") || !params.get("iv")) && !(params.get("enc") === "chk-v1" && params.get("key"))) return;
      try {
        this.say("Decrypting…");
        const response = await fetch(tiny.localPath("/files/raw?hash=" + encodeURIComponent(this.hash())));
        if (!response.ok) throw Error("download failed (" + response.status + ")");
        const encrypted = new Uint8Array(await response.arrayBuffer());
        if (await tiny.sha256hex(encrypted) !== this.hash()) throw Error("ciphertext hash mismatch");
        let plain;
        if (params.get("enc") === "chk-v1") plain = await tiny.blossom.encryption.decryptCHK(encrypted, params.get("key"), this.hash());
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
      await this.ready;
      const recipient = form.elements.namedItem("recipient").value.trim();
      if (!recipient) { this.say("Enter a public key or npub.", true); return; }
      const params = new URLSearchParams(location.hash.slice(1));
      const signer = tiny.signer();
      if (!tiny.files.messages || !signer) { this.say("Connect a NIP-44 signer first. Use the encrypted link, which keeps its key in the URL fragment.", true); return; }
      if (!params.get("key") || !params.get("iv")) { this.say("NIP-17 sharing currently requires a random AES-GCM encrypted file.", true); return; }
      this.sharing = true;
      try {
        const key = fromB64url(params.get("key")), nonce = fromB64url(params.get("iv"));
        if (key.length !== 32 || nonce.length !== 12) throw Error("Invalid file key or nonce.");
        const {hex} = tiny.util;
        this.say("Preparing sealed NIP-17 delivery…");
        await tiny.files.messages.share({recipient, fileURL: new URL(tiny.localPath("/files/raw?hash=" + this.hash()), location.href).href, ciphertextHash: this.hash(), plaintextHash: params.get("ox") || undefined, key: hex(key), nonce: hex(nonce), mimeType: params.get("type") || this.type(), size: Number(this.getAttribute("size") || 0) || undefined}, {signer});
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

  // NostrCompose signs NIP-34 issue, pull request and conversation events.
  // The relay receives the signed event directly, so the same control works
  // with NIP-07 and the existing NIP-46 bridge.
  class NostrCompose extends FormElement {
    connectedCallback() {
      super.connectedCallback();
      // A comment link on a diff line opens the page at this compose.
      if (this.id && globalThis.location?.hash === "#" + this.id) {
        this.scrollIntoView?.({block: "center"});
        this.form?.elements.content?.focus?.();
      }
    }

    async submit(form) {
      if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
      const value = name => form.elements[name]?.value.trim() || "";
      const mode = this.getAttribute("kind");
      const kind = Number(mode === "status" ? value("status") : mode);
      if (![1621, 1618, 1111, 1630, 1631, 1632, 1633].includes(kind)) throw Error("Choose a valid event type.");
      const coordinate = this.getAttribute("coordinate") || "";
      // A reply to a request for a decision may sit outside any repository.
      if (coordinate ? !/^30617:[0-9a-f]{64}:.+$/.test(coordinate) : kind !== 1111) throw Error("The repository address is missing.");
      const tags = coordinate ? [["a", coordinate]] : [];
      if (kind === 1111 || kind >= 1630) {
        const root = this.getAttribute("root"), pubkey = this.getAttribute("root-pubkey");
        const rootKind = this.getAttribute("root-kind");
        const rootKinds = coordinate ? ["1617", "1618", "1621"] : ["1111", "9", "11"];
        if (!isHex64(root) || !isHex64(pubkey) || !rootKinds.includes(rootKind)) throw Error("The conversation address is missing.");
        if (kind === 1111) {
          const parent = this.getAttribute("parent") || root;
          const parentPubkey = this.getAttribute("parent-pubkey") || pubkey;
          const parentKind = this.getAttribute("parent-kind") || rootKind;
          if (!isHex64(parent) || !isHex64(parentPubkey) || !["1111", rootKind].includes(parentKind)) throw Error("The reply address is invalid.");
          tags.push(["E", root, "", pubkey], ["K", rootKind], ["P", pubkey], ["e", parent, "", parentPubkey], ["k", parentKind], ["p", parentPubkey]);
          const anchor = name => value(name) || this.getAttribute(name) || "";
          const file = anchor("file"), line = anchor("line"), side = anchor("side");
          if (file || line || side) {
            if (!["1617", "1618"].includes(rootKind)) throw Error("Line comments belong to a pull request or patch.");
            if (!file || !/^[1-9]\d*$/.test(line) || !["old", "new"].includes(side)) throw Error("The line reference is incomplete: it needs a file, a line number and a side.");
            tags.push(["file", file], ["line", line, side]);
          }
        } else {
          tags.push(["e", root, "", "root"], ["p", pubkey]);
          if (coordinate.split(":")[1] !== pubkey) tags.push(["p", coordinate.split(":")[1]]);
        }
      }
      if (kind === 1621 || kind === 1618) {
        if (!value("title")) throw Error("Enter a title.");
        tags.push(["p", coordinate.split(":")[1]], ["subject", value("title")]);
        const labels = [...new Set(value("labels").split(",").map(label => label.trim()).filter(Boolean))];
        labels.forEach(label => tags.push(["t", label]));
      }
      if (kind === 1618) {
        const commit = value("commit"), base = value("merge-base");
        if (!/^[0-9a-f]{40}$/.test(commit) || (base && !/^[0-9a-f]{40}$/.test(base))) throw Error("Enter a full Git commit ID.");
        let clone; try { clone = new URL(value("clone")); } catch { throw Error("Enter a clone URL."); }
        if (!["https:", "http:"].includes(clone.protocol) || clone.username || clone.password || clone.hash) throw Error("Enter an HTTP or HTTPS clone URL without credentials.");
        tags.push(["c", commit], ["clone", clone.href]);
        if (base) tags.push(["merge-base", base]);
      }
      const content = form.elements.content?.value || "";
      if ((kind === 1621 || kind === 1111) && !content.trim()) throw Error("Enter a message.");
      const unsigned = {kind, created_at: Math.floor(Date.now() / 1000), tags, content};
      // Keep a separate copy: extensions may mutate their input while signing.
      const expected = JSON.stringify(unsigned);
      const event = await window.nostr.signEvent(JSON.parse(expected));
      const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
      if (actual !== expected || !window.NostrSigner?.verifyEvent(event)) throw Error("The signer returned an invalid or changed event.");
      const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event), {contentType: "application/json"});
      const result = await response.json();
      if (!response.ok || result.accepted !== true) throw Error(result.error || result.message || "The relay rejected the event.");
      this.report("Published.");
      form.reset();
      // Refresh through the shared navigation so the thread shows the new
      // event without a full reload.
      await tiny.navigate?.(globalThis.location?.href);
    }
  }

  // ProfileForm publishes the signer's kind 0 profile: the fields shown here
  // are merged over the profile the relay already holds, so a field another
  // client set is kept. The event goes to this relay first, then to every
  // relay listed, and the browser's name cache forgets the old name.
  class ProfileForm extends FormElement {
    static content(values, existing) {
      let merged = {};
      try { merged = typeof existing === "string" ? JSON.parse(existing || "{}") : {...(existing || {})}; } catch { merged = {}; }
      if (!merged || typeof merged !== "object" || Array.isArray(merged)) merged = {};
      for (const key of ["name", "display_name", "about", "picture", "banner", "website", "nip05", "lud16"]) {
        if (!(key in values)) continue;
        const value = String(values[key] ?? "").trim();
        if (value) merged[key] = value; else delete merged[key];
      }
      for (const key of ["picture", "banner", "website"]) {
        if (merged[key] && !/^https?:\/\//.test(merged[key])) throw Error("Use an http or https link for " + key.replace("_", " ") + ".");
      }
      return merged;
    }

    static relays(text) {
      const seen = new Set();
      for (const raw of String(text || "").split(/[\s,]+/)) {
        const url = tiny.util.relayURL(raw);
        if (url) seen.add(url);
      }
      if (seen.size > 12) throw Error("List at most 12 relays.");
      return [...seen];
    }

    async submit(form) {
      if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
      const value = name => form.elements[name]?.value ?? "";
      const values = Object.fromEntries(["name", "display_name", "about", "picture", "banner", "website", "nip05", "lud16"].map(key => [key, value(key)]));
      const content = ProfileForm.content(values, this.getAttribute("existing") || "{}");
      const relays = ProfileForm.relays(value("relays"));
      const unsigned = {kind: 0, created_at: Math.floor(Date.now() / 1000), tags: [], content: JSON.stringify(content)};
      const expected = JSON.stringify(unsigned);
      this.report("Signing…");
      const event = await window.nostr.signEvent(JSON.parse(expected));
      const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
      if (actual !== expected || !window.NostrSigner?.verifyEvent(event)) throw Error("The signer returned an invalid or changed event.");
      const pubkey = this.getAttribute("pubkey");
      if (pubkey && event.pubkey !== pubkey) throw Error("The signer holds a different key than the one signed in.");
      const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event), {contentType: "application/json"});
      const result = await response.json().catch(() => ({}));
      if (!response.ok || result.accepted !== true) throw Error(result.error || result.message || "The relay rejected the profile.");
      this.forget(event.pubkey);
      const lines = ["Published here."];
      if (relays.length) {
        if (!window.NostrSigner?.SimplePool) throw Error("Published here; the signer bundle is still loading, so other relays were skipped.");
        const pool = new window.NostrSigner.SimplePool();
        const outcomes = await Promise.allSettled(pool.publish(relays, event).map(p => Promise.race([p, new Promise((_, reject) => setTimeout(() => reject(Error("timed out")), 8000))])));
        outcomes.forEach((outcome, index) => { lines.push(relays[index] + ": " + (outcome.status === "fulfilled" ? "ok" : "failed, " + (outcome.reason?.message || outcome.reason || "no answer"))); });
        try { pool.close(relays); } catch {}
      }
      this.report(lines.join("\n"));
    }

    // forget drops the cached name so nostr-name shows the new one on the
    // next page.
    forget(pubkey) {
      try {
        const key = window.tinyNames?.cacheKey || "tiny:names:v1";
        const cache = JSON.parse(localStorage.getItem(key) || "{}") || {};
        delete cache[pubkey];
        localStorage.setItem(key, JSON.stringify(cache));
      } catch {}
    }
  }

  // AgentGrant signs a kind 30392 agent grant: the agent's key, name, scope
  // and expiry, signed by the owner or a moderator. The agent key is made
  // here and its nsec shown once, or pasted as a public key. The secret never
  // leaves the page; the relay only receives the signed grant.
  class AgentGrant extends FormElement {
    connectedCallback() {
      super.connectedCallback();
      if (this.toggled || !this.form) return;
      this.toggled = true;
      const mode = this.form.elements.key;
      const toggle = () => { const label = this.form.elements.pubkey?.closest("label"); if (label) label.hidden = mode.value !== "paste"; };
      mode?.addEventListener("change", toggle);
      if (mode) toggle();
    }

    // tags builds the grant tags from the field values; it throws on any
    // value the relay would refuse so nothing reaches the signer.
    static tags(values, pubkey, now = Math.floor(Date.now() / 1000)) {
      const text = name => String(values[name] ?? "").trim();
      const split = name => text(name).split(/[\s,]+/).map(item => item.trim()).filter(Boolean);
      if (!isHex64(pubkey)) throw Error("Enter the agent's public key as 64 lowercase hex characters.");
      const name = text("name");
      if (name.length > 64) throw Error("Keep the name to 64 characters.");
      const tags = [["d", pubkey], ["p", pubkey]];
      if (name) tags.push(["name", name]);
      const expires = Math.floor(Date.parse(text("expires") + "T23:59:59Z") / 1000);
      if (!Number.isFinite(expires) || expires <= now) throw Error("Choose an expiry date after today.");
      if (expires > now + 365 * 86400) throw Error("Grants last at most 365 days.");
      tags.push(["expiration", String(expires)]);
      for (const room of [...new Set(split("rooms"))]) {
        if (room.length > 128) throw Error("Room names are at most 128 characters.");
        tags.push(["room", room]);
      }
      const seenRepos = new Set();
      for (const line of text("repos").split(/\r?\n/).map(line => line.trim()).filter(Boolean)) {
        const match = /^([0-9a-f]{64}):(.+):(read|maintain)$/.exec(line);
        if (!match || match[2].length > 256) throw Error("Repositories are owner pubkey:identifier:read or maintain, one per line.");
        if (seenRepos.has(match[1] + ":" + match[2])) continue;
        seenRepos.add(match[1] + ":" + match[2]);
        tags.push(["repo", line]);
      }
      for (const kind of [...new Set(split("kinds"))]) {
        if (!/^\d{1,5}$/.test(kind) || Number(kind) > 65535) throw Error("Kinds are whole numbers from 0 to 65535.");
        tags.push(["k", String(Number(kind))]);
      }
      const wiki = text("wiki");
      if (wiki && wiki !== "propose" && wiki !== "edit") throw Error("Wiki access is propose or edit.");
      if (wiki) tags.push(["wiki", wiki]);
      // Sites: a label under the agent's key or *, then ttl=<days> and
      // encrypted, one entry per line. The relay checks the label's key.
      const seenSites = new Set();
      for (const line of text("sites").split(/\r?\n/).map(line => line.trim()).filter(Boolean)) {
        const [label, ...flags] = line.split(/\s+/);
        if (label !== "*" && !/^npub1[a-z0-9]{58}$/.test(label) && !/^[0-9a-z]{50}[a-z0-9-]{1,13}$/.test(label))
          throw Error("Sites are a site label or *, then ttl=<days> and encrypted, one per line.");
        const tag = ["sites", label];
        for (const flag of flags) {
          const ttl = /^ttl=(\d{1,3})$/.exec(flag);
          if (ttl && Number(ttl[1]) >= 1 && Number(ttl[1]) <= 365) tag.push("ttl=" + Number(ttl[1]));
          else if (flag === "encrypted") tag.push("encrypted");
          else throw Error("Sites take ttl=<days> from 1 to 365 and encrypted after the label.");
        }
        if (seenSites.has(label)) continue;
        seenSites.add(label);
        tags.push(tag);
      }
      const rate = text("rate");
      if (rate) {
        if (!/^\d+$/.test(rate) || Number(rate) < 1 || Number(rate) > 600) throw Error("Rate is 1 to 600 events per minute.");
        tags.push(["rate", String(Number(rate))]);
      }
      return tags;
    }

    async submit(form) {
      if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
      const value = name => form.elements[name]?.value ?? "";
      const values = {name: value("name"), expires: value("expires"), rooms: value("rooms"), repos: value("repos"), kinds: value("kinds"), wiki: value("wiki"), sites: value("sites"), rate: value("rate")};
      let secret = null, pubkey = value("pubkey").trim().toLowerCase();
      if (value("key") !== "paste") {
        if (!window.NostrSigner?.generateSecretKey) throw Error("The signer bundle is still loading.");
        secret = window.NostrSigner.generateSecretKey();
        pubkey = window.NostrSigner.getPublicKey(secret);
      }
      const tags = AgentGrant.tags(values, pubkey);
      const unsigned = {kind: 30392, created_at: Math.floor(Date.now() / 1000), tags, content: ""};
      // Keep a separate copy: extensions may mutate their input while signing.
      const expected = JSON.stringify(unsigned);
      this.report("Signing…");
      const event = await window.nostr.signEvent(JSON.parse(expected));
      const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
      if (actual !== expected || !window.NostrSigner?.verifyEvent(event)) throw Error("The signer returned an invalid or changed event.");
      if (event.pubkey === pubkey) throw Error("The agent needs its own key, not yours.");
      const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event), {contentType: "application/json"});
      const result = await response.json();
      if (!response.ok || result.accepted !== true) throw Error(result.error || result.message || "The relay rejected the grant.");
      this.report("Granted.");
      if (secret) {
        this.reveal(pubkey, secret);
        return;
      }
      form.reset();
      await tiny.navigate?.(globalThis.location?.href);
    }

    // reveal shows the generated secret once. The page is not reloaded so
    // it stays on screen until the person leaves.
    reveal(pubkey, secret) {
      const nsec = tiny.util.bech32Encode("nsec", secret);
      const warning = el("p", "Copy the agent's secret key now. It is not stored anywhere and will not be shown again.");
      warning.setAttribute("role", "alert");
      const secretLabel = el("label", "Agent secret key ");
      secretLabel.setAttribute("data-secret", "");
      const secretInput = el("input");
      secretInput.readOnly = true;
      secretInput.name = "nsec";
      secretInput.value = nsec;
      secretInput.addEventListener("focus", () => secretInput.select());
      secretLabel.append(secretInput);
      const keyLabel = el("label", "Agent public key ");
      keyLabel.setAttribute("data-secret", "");
      const keyInput = el("input");
      keyInput.readOnly = true;
      keyInput.name = "agent";
      keyInput.value = pubkey;
      keyLabel.append(keyInput);
      const link = el("a", "Show the new agent");
      link.href = (globalThis.location?.pathname || "") + "?agent=" + pubkey;
      const note = el("p");
      note.append(link);
      this.append(warning, secretLabel, keyLabel, note);
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

  // Rooms: the compose bar, room creation, room administration and the live
  // stream. Each signed event is built here, signed by the connected signer,
  // verified unchanged and published once to /events. room-message markup
  // matches the roomMessage template so streamed messages read the same.
  const roomKinds = [9, 11, 12, 40002, 44100, 44101];
  const mentionPattern = /(^|[\s(])@(npub1[02-9ac-hj-np-z]{58}|[0-9a-f]{64})\b/g;
  const roomLinkPattern = /https?:\/\/[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+/g;
  const now = () => Math.floor(Date.now() / 1000);
  // keyHex accepts a hex key or an npub and returns the hex key, or null.
  const keyHex = value => {
    const text = String(value || "").trim().replace(/^nostr:/, "");
    if (isHex64(text)) return text;
    if (!/^npub1[02-9ac-hj-np-z]{58}$/.test(text)) return null;
    try { const decoded = window.NostrSigner?.decodeNpub?.(text); return isHex64(decoded) ? decoded : null; } catch { return null; }
  };
  // roomMentions lists the keys named as @npub or @hex in a message, once each.
  const roomMentions = text => {
    const keys = [];
    for (const match of String(text).matchAll(mentionPattern)) {
      const key = keyHex(match[2]);
      if (key && !keys.includes(key)) keys.push(key);
    }
    return keys;
  };
  // roomID derives a room id from a name: lowercase letters, digits, hyphen and underscore.
  const roomID = name => String(name || "").toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 64);
  const tagValue = (event, name) => (event.tags || []).find(tag => tag[0] === name)?.[1] || "";
  const clock = seconds => {
    const stamp = new Date(seconds * 1000).toISOString();
    if (stamp.slice(0, 10) === new Date().toISOString().slice(0, 10)) return stamp.slice(11, 16);
    return ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][Number(stamp.slice(5, 7)) - 1] + " " + Number(stamp.slice(8, 10)) + " " + stamp.slice(11, 16);
  };
  const roomPath = (room, rest = "") => tiny.localPath("/rooms/" + encodeURIComponent(room) + rest);
  // linkify fills a node with escaped text, turning http(s) URLs and nostr
  // links into anchors; nostr links open through /open.
  const linkify = (node, text) => {
    let last = 0;
    for (const match of String(text).matchAll(roomLinkPattern)) {
      node.append(text.slice(last, match.index));
      const trimmed = match[0].replace(/[.,;:!?)]+$/, "");
      const link = el("a", trimmed);
      link.href = trimmed.startsWith("http") ? trimmed : tiny.localPath("/open?target=" + encodeURIComponent(trimmed));
      if (trimmed.startsWith("http")) link.rel = "noopener";
      node.append(link, match[0].slice(trimmed.length));
      last = match.index + match[0].length;
    }
    node.append(text.slice(last));
    return node;
  };
  // chatMarkdown renders the same Markdown subset the page renders for chat
  // messages: paragraphs with line breaks, fenced code, headings, lists,
  // inline code, emphasis, links, and bare http(s) or nostr: references. It
  // builds nodes rather than markup, so message text never becomes HTML.
  const inlinePattern = /(`[^`]+`)|\[([^\]]+)\]\(([^)\s]+)\)|\*\*([^*]+)\*\*|(^|[^*\w])\*([^*]+)\*|(https?:\/\/[^\s<>"']+|(?:web\+)?nostr:[a-z0-9]+)/g;
  const safeLink = target => /^(https?:\/\/|\/(?!\/)|\.\.?\/|#|mailto:)/.test(target) || (!target.includes(":") && !target.startsWith("//"));
  const chatInline = (node, text) => {
    let last = 0;
    for (const match of String(text).matchAll(inlinePattern)) {
      node.append(text.slice(last, match.index));
      last = match.index + match[0].length;
      if (match[1]) { node.append(el("code", match[1].slice(1, -1))); continue; }
      if (match[2] !== undefined) {
        if (!safeLink(match[3])) { node.append(match[0]); continue; }
        const link = el("a", match[2]);
        link.href = match[3];
        if (match[3].startsWith("http")) link.rel = "noopener";
        node.append(link);
        continue;
      }
      if (match[4] !== undefined) { node.append(chatInline(el("strong"), match[4])); continue; }
      if (match[6] !== undefined) { node.append(match[5], chatInline(el("em"), match[6])); continue; }
      const trimmed = match[7].replace(/[.,;:!?)]+$/, "");
      const link = el("a", trimmed);
      link.href = trimmed.startsWith("http") ? trimmed : tiny.localPath("/open?target=" + encodeURIComponent(trimmed));
      if (trimmed.startsWith("http")) link.rel = "noopener";
      node.append(link, match[7].slice(trimmed.length));
    }
    node.append(text.slice(last));
    return node;
  };
  const listItem = line => /^\s*(?:[-*+]|\d+[.)])\s+/.test(line);
  const chatMarkdown = (node, text) => {
    const lines = String(text).replace(/\r\n/g, "\n").split("\n");
    let paragraph = [];
    const flush = () => {
      if (!paragraph.length) return;
      const p = el("p");
      paragraph.forEach((line, index) => { if (index) p.append(el("br")); chatInline(p, line); });
      node.append(p);
      paragraph = [];
    };
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (line.startsWith("```")) {
        flush();
        const code = [];
        for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
        const pre = el("pre"); pre.append(el("code", code.join("\n"))); node.append(pre);
      } else if (line.startsWith("#")) {
        flush();
        const level = Math.min(6, line.length - line.replace(/^#+/, "").length);
        node.append(chatInline(el("h" + level), line.slice(level).trim()));
      } else if (listItem(line)) {
        flush();
        const list = el(/^\s*\d/.test(line) ? "ol" : "ul");
        for (; i < lines.length && listItem(lines[i]); i++) list.append(chatInline(el("li"), lines[i].replace(/^\s*(?:[-*+]|\d+[.)])\s+/, "")));
        i--;
        node.append(list);
      } else if (!line.trim()) {
        flush();
      } else {
        paragraph.push(line.trim());
      }
    }
    flush();
    return node;
  };
  const keyNode = hex => { const node = el("nostr-key", hex.slice(0, 12)); node.setAttribute("hex", hex); node.title = hex; return node; };
  // nameNode shows a person: the vendored nostr-name element replaces the short
  // id with the profile name published on this relay.
  const nameNode = hex => { const node = el("nostr-name", hex.slice(0, 12)); node.setAttribute("pubkey", hex); node.title = hex; return node; };
  // panelMembers reads the members list in the panel: role and agent marker by key.
  const panelMembers = () => {
    const members = {};
    document.querySelectorAll("#members li").forEach(item => {
      const hex = item.querySelector("nostr-name")?.getAttribute("pubkey") || item.querySelector("nostr-key")?.getAttribute("hex");
      if (hex) members[hex] = {role: (item.querySelector("small")?.textContent || "").split("|")[0].trim(), agent: item.hasAttribute("data-agent")};
    });
    return members;
  };
  const messageNode = (event, {members = {}, room = "", inThread = false} = {}) => {
    const node = el("room-message");
    const notice = event.kind === 44100 ? "joined the room" : event.kind === 44101 ? "left the room" : "";
    const pubkey = notice ? tagValue(event, "p") : event.pubkey;
    node.id = "msg-" + event.id;
    node.dataset.id = event.id;
    node.dataset.kind = String(event.kind);
    node.dataset.pubkey = pubkey;
    const member = members[pubkey];
    if (member?.agent) node.dataset.agent = "";
    if (notice) node.dataset.notice = "";
    const header = el("header"), name = el("b"), time = el("time", clock(event.created_at)), small = el("small");
    name.append(nameNode(pubkey));
    time.dateTime = new Date(event.created_at * 1000).toISOString().replace(/\.\d+Z$/, "Z");
    time.title = time.dateTime.slice(0, 16).replace("T", " ") + " UTC";
    small.append(time);
    header.append(name, member?.role ? " | " + member.role : "", small);
    const body = notice ? el("p", notice) : chatMarkdown(el("div"), event.content || "");
    node.append(header, body);
    const footer = el("footer");
    const mentions = notice ? [] : (event.tags || []).filter(tag => tag[0] === "p" && isHex64(tag[1]) && tag[1] !== pubkey).map(tag => tag[1]);
    if (mentions.length) { const span = el("span", "to "); mentions.forEach(key => span.append(nameNode(key), " ")); footer.append(span); }
    if (!inThread && room && event.kind === 11) { const link = el("a", "thread"); link.href = roomPath(room, "/thread/" + event.id); footer.append(link); }
    const root = tagValue(event, "e");
    if (!inThread && room && event.kind === 12 && isHex64(root)) { const link = el("a", "in thread"); link.href = roomPath(room, "/thread/" + root); footer.append(link); }
    if (footer.childNodes.length) node.append(footer);
    return node;
  };
  // The timeline scrolls inside the content column on desktop and with the
  // page body on phones; roomBox finds whichever holds the overflow.
  const roomBox = () => [document.getElementById("content"), document.body, document.documentElement].find(node => node && node.scrollHeight > node.clientHeight + 1 && /auto|scroll/.test(getComputedStyle(node).overflowY)) || null;
  const roomNearBottom = () => {
    const box = roomBox();
    return !box || box.scrollHeight - box.scrollTop - box.clientHeight < 120;
  };
  const roomScroll = () => {
    const box = roomBox();
    if (box) box.scrollTop = box.scrollHeight;
  };
  // roomAppend adds one message to the timeline unless it is already there,
  // keeps the list bounded and follows the newest message when the viewer
  // is already reading the end of it.
  const roomAppend = (event, {room = "", inThread = false, own = false} = {}) => {
    const list = document.getElementById("messages");
    if (!list || !isHex64(event?.id) || document.getElementById("msg-" + event.id)) return null;
    const follow = own || roomNearBottom();
    list.querySelector("#empty")?.remove();
    const node = messageNode(event, {members: panelMembers(), room, inThread});
    list.append(node);
    while (list.children.length > 500) list.firstElementChild.remove();
    const root = tagValue(event, "e");
    if (event.kind === 12 && !inThread && isHex64(root)) {
      const link = document.querySelector("#msg-" + root + " > footer > a");
      if (link) { const count = Number((link.textContent.match(/(\d+) repl/) || [])[1] || 0) + 1; link.textContent = "thread | " + count + (count === 1 ? " reply" : " replies"); }
    }
    if (follow) roomScroll();
    return node;
  };
  // roomReact adds a reaction to its target's summary line.
  const roomReact = event => {
    const targets = (event.tags || []).filter(tag => tag[0] === "e");
    const target = targets.length && document.getElementById("msg-" + targets[targets.length - 1][1]);
    if (!target) return;
    let footer = target.querySelector(":scope > footer");
    if (!footer) { footer = el("footer"); target.append(footer); }
    const content = (event.content || "").trim() === "" || (event.content || "").trim() === "+" ? "+1" : event.content.trim();
    let span = [...footer.querySelectorAll("span[data-reaction]")].find(node => node.dataset.reaction === content);
    if (span) span.textContent = content + " " + (Number(span.textContent.slice(content.length)) + 1);
    else { span = el("span", content + " 1"); span.dataset.reaction = content; footer.append(span); }
  };
  // roomEdit applies a kind 40003 edit to the message it names when the
  // editor is that message's author, replacing the text and marking it.
  const roomEdit = event => {
    const targets = (event.tags || []).filter(tag => tag[0] === "e");
    const target = targets.length && document.getElementById("msg-" + targets[targets.length - 1][1]);
    if (!target || target.dataset.pubkey !== event.pubkey || target.dataset.notice !== undefined) return;
    const body = target.querySelector(":scope > div");
    if (!body) return;
    body.replaceWith(chatMarkdown(el("div"), event.content || ""));
    if (target.dataset.edited !== undefined) return;
    target.dataset.edited = "";
    let footer = target.querySelector(":scope > footer");
    if (!footer) { footer = el("footer"); target.append(footer); }
    const mark = el("span", "edited");
    mark.dataset.edited = "";
    footer.append(mark);
  };
  // signAndPublish signs one room event, checks it came back unchanged and
  // valid, and publishes it once.
  const signAndPublish = async unsigned => {
    if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
    const expected = JSON.stringify(unsigned);
    const event = await window.nostr.signEvent(JSON.parse(expected));
    const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
    if (actual !== expected || !window.NostrSigner?.verifyEvent(event)) throw Error("The signer returned an invalid or changed event.");
    const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event), {contentType: "application/json"});
    const result = await response.json();
    if (!response.ok || result.accepted !== true) throw Error(result.error || result.message || "The relay rejected the event.");
    return event;
  };

  // RoomCompose sends a chat message (kind 9) or a thread reply (kind 12).
  // Enter sends and Shift+Enter starts a new line.
  class RoomCompose extends FormElement {
    connectedCallback() {
      super.connectedCallback();
      if (this.keys) return;
      this.keys = true;
      this.addEventListener("keydown", event => {
        if (event.key !== "Enter" || event.shiftKey || event.isComposing || !event.target.matches?.("textarea")) return;
        event.preventDefault();
        this.form?.requestSubmit();
      });
    }

    event(content) {
      const room = this.getAttribute("room") || "";
      if (!/^[a-z0-9_-]{1,64}$/.test(room)) throw Error("The room id is missing.");
      const kind = Number(this.getAttribute("kind") || 9);
      if (kind !== 9 && kind !== 12) throw Error("Unsupported message kind.");
      const tags = [["h", room]];
      if (kind === 12) {
        const root = this.getAttribute("root"), author = this.getAttribute("root-pubkey");
        if (!isHex64(root)) throw Error("The thread root is missing.");
        tags.push(["e", root]);
        if (isHex64(author) && author !== this.getAttribute("pubkey")) tags.push(["p", author]);
      }
      roomMentions(content).forEach(key => { if (!tags.some(tag => tag[0] === "p" && tag[1] === key)) tags.push(["p", key]); });
      return {kind, created_at: now(), tags, content};
    }

    async submit(form) {
      const content = (form.elements.content?.value || "").trim();
      if (!content) return;
      this.report("Signing…");
      const event = await signAndPublish(this.event(content));
      form.reset();
      this.report("");
      roomAppend(event, {room: this.getAttribute("room"), inThread: event.kind === 12, own: true});
    }
  }

  // RoomCreate signs a NIP-29 create (kind 9007) with an id derived from the
  // name, then opens the new room.
  class RoomCreate extends FormElement {
    // event builds the kind 9007 room creation. The id comes from the name
    // unless one is given, so a client that wants a fixed id, such as a Buzz
    // gateway that expects a UUID, can ask for it.
    event({name = "", id = "", about = "", access = "open"} = {}) {
      const title = String(name).trim(), given = String(id).trim().toLowerCase();
      if (given && !/^[a-z0-9_-]{1,64}$/.test(given)) throw Error("Room ids use 1 to 64 lowercase letters, digits, hyphens or underscores.");
      id = given || roomID(title);
      if (!id) throw Error("Enter a name with letters or digits.");
      const tags = [["h", id], ["name", title]];
      if (String(about).trim()) tags.push(["about", String(about).trim()]);
      tags.push(["visibility", access === "members" ? "members" : "open"]);
      return {id, event: {kind: 9007, created_at: now(), tags, content: ""}};
    }

    async submit(form) {
      const value = name => form.elements[name]?.value || "";
      const {id, event} = this.event({name: value("name"), id: value("id"), about: value("about"), access: value("access")});
      this.report("Signing…");
      await signAndPublish(event);
      this.report("Created #" + id + ".");
      form.reset();
      await tiny.navigate?.(roomPath(id), true);
    }
  }

  // RoomAction signs one NIP-29 management event for a room: add a member
  // (9000), remove one (9001), edit the room (9002), join (9021) or leave (9022).
  class RoomAction extends FormElement {
    event(values = {}) {
      const room = this.getAttribute("room") || "";
      if (!/^[a-z0-9_-]{1,64}$/.test(room)) throw Error("The room id is missing.");
      const kind = Number(this.getAttribute("kind"));
      const tags = [["h", room]];
      switch (kind) {
        case 9000: case 9001: {
          const key = keyHex(values.pubkey);
          if (!key) throw Error("Enter an npub or a 64-character hex key.");
          const tag = ["p", key];
          if (kind === 9000) tag.push(["owner", "admin"].includes(values.role) ? values.role : "member");
          tags.push(tag);
          break;
        }
        case 9002: {
          const name = String(values.name || "").trim();
          if (!name) throw Error("Enter a name.");
          tags.push(["name", name], ["about", String(values.about || "").trim()], ["picture", String(values.picture || "").trim()], ["visibility", values.access === "members" ? "members" : "open"]);
          break;
        }
        case 9021: case 9022: break;
        default: throw Error("Unsupported room action.");
      }
      return {kind, created_at: now(), tags, content: ""};
    }

    async submit(form) {
      const values = {};
      for (const field of form.elements) if (field.name) values[field.name] = field.value;
      this.report("Signing…");
      await signAndPublish(this.event(values));
      this.report("Done.");
      await tiny.navigate?.(location.href);
    }
  }

  // RoomLive follows a room's stream: new messages join the timeline, and
  // reactions update their targets. It reconnects with backoff.
  class RoomLive extends HTMLElement {
    static get observedAttributes() { return ["room", "root"]; }
    connectedCallback() { this.open(); roomScroll(); }
    disconnectedCallback() { this.close(); }
    attributeChangedCallback() { if (this.isConnected) { this.close(); this.open(); } }

    open() {
      const room = this.getAttribute("room");
      if (!room || this.source || typeof EventSource !== "function") return;
      this.delay = this.delay || 1000;
      const source = new EventSource(roomPath(room, "/stream"), {withCredentials: true});
      this.source = source;
      source.addEventListener("open", () => { this.delay = 1000; this.textContent = ""; });
      source.addEventListener("message", event => {
        let parsed;
        try { parsed = JSON.parse(event.data); } catch { return; }
        this.receive(parsed);
      });
      source.addEventListener("error", () => {
        if (this.source !== source) return;
        this.close();
        this.textContent = "reconnecting";
        this.timer = setTimeout(() => { this.timer = null; this.open(); }, this.delay);
        this.delay = Math.min(this.delay * 2, 30000);
      });
    }

    close() {
      this.source?.close();
      this.source = null;
      clearTimeout(this.timer);
      this.timer = null;
    }

    receive(event) {
      if (!event || typeof event !== "object") return;
      if (event.kind === 7) { roomReact(event); return; }
      if (event.kind === 40003) { roomEdit(event); return; }
      if (!roomKinds.includes(event.kind)) return;
      const root = this.getAttribute("root");
      if (root && (event.kind !== 12 || tagValue(event, "e") !== root)) return;
      roomAppend(event, {room: this.getAttribute("room"), inThread: Boolean(root)});
    }
  }
  tiny.rooms = Object.freeze({keyHex, roomMentions, roomID, messageNode, roomAppend, roomReact, roomEdit, linkify, chatMarkdown});

  // JsonView renders any JSON value. Arrays of objects become tables, objects
  // become definition lists, scalar arrays become lists, and deep nesting
  // falls back to compact JSON so a response never explodes the page.
  // ShareLink offers the device's own share sheet for the current page, or
  // the url and title attributes when set. It stays empty where the browser
  // has no share support, so the page reads the same without it.
  class ShareLink extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      if (!navigator.share) return;
      const button = el("button", "Share…");
      button.type = "button";
      button.addEventListener("click", () => {
        const data = {url: this.getAttribute("url") || location.href.split("#", 1)[0], title: this.getAttribute("title") || document.title};
        navigator.share(data).catch(() => {});
      });
      this.replaceChildren(button);
    }
  }

  // publishSigned asks the signer for a signature, checks that what came
  // back is the event that was requested, since extensions may edit input,
  // then posts it once and reports the relay's answer.
  const publishSigned = async unsigned => {
    if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
    const expected = JSON.stringify(unsigned);
    const event = await window.nostr.signEvent(JSON.parse(expected));
    const actual = event && JSON.stringify({kind: event.kind, created_at: event.created_at, tags: event.tags, content: event.content});
    if (actual !== expected || !window.NostrSigner?.verifyEvent(event)) throw Error("The signer returned an invalid or changed event.");
    const response = await tiny.signedFetch("/events", "POST", JSON.stringify(event), {contentType: "application/json"});
    let result = null;
    try { result = await response.json(); } catch {}
    if (!response.ok || result?.accepted !== true) throw Error(result?.error || result?.message || "The relay rejected the event.");
    return event;
  };
  const unixNow = () => Math.floor(Date.now() / 1000);

  // NostrReact signs a NIP-25 reaction to one event: "+" or "-" from the
  // pressed button. The reaction names the event with e and its author with
  // p, so the author's own subscription sees the answer. A request for a
  // decision is approved or denied this way and a wiki merge request
  // accepted or rejected. Nothing is signed without a press: the buttons
  // submit the form, and prompt() asks once more before signing on behalf of
  // a notification action.
  const decisions = {"+": "approve", "-": "deny"};
  const reactionWords = {"+": "Approved.", "-": "Denied.", accept: "Accepted.", approve: "Approved.", reject: "Rejected.", deny: "Denied."};
  class NostrReact extends FormElement {
    connectedCallback() {
      if (this.form) return;
      super.connectedCallback();
      if (this.listening) return;
      this.listening = true;
      this.addEventListener("click", event => {
        const button = event.target.closest("button[name=reaction]");
        if (button) this.choice = button.value;
      });
    }

    async submit(form, choice = this.choice ?? this.submitter?.value ?? form?.elements?.reaction?.value) {
      const id = this.getAttribute("event"), pubkey = this.getAttribute("pubkey");
      if (!isHex64(id) || !isHex64(pubkey)) throw Error("The request address is missing.");
      if (!(choice in decisions)) throw Error("Choose Approve or Deny.");
      const tags = [["e", id, "", pubkey], ["p", pubkey]];
      const kind = this.getAttribute("kind");
      if (/^\d+$/.test(kind || "")) tags.push(["k", kind]);
      this.report("Signing…");
      await publishSigned({kind: 7, created_at: unixNow(), tags, content: choice});
      const label = (this.submitter?.textContent || "").trim().toLowerCase();
      this.choice = null;
      this.submitter = null;
      this.report(reactionWords[label] || reactionWords[choice]);
      await tiny.navigate?.((globalThis.location?.href || "").split("?", 1)[0]);
    }

    // prompt shows the confirmation for one answer; the signature waits for
    // the confirm button. Without a signer it waits for one to connect.
    prompt(answer) {
      const choice = Object.keys(decisions).find(key => decisions[key] === answer);
      if (!choice) return;
      if (!window.nostr?.signEvent) {
        this.report("Connect a signer to answer this request.");
        document.addEventListener("tiny:signer", () => this.prompt(answer), {once: true});
        return;
      }
      const question = el("span", (answer === "approve" ? "Approve" : "Deny") + " this request? ");
      const confirm = el("button", "Confirm");
      confirm.type = "button";
      const cancel = el("button", "Cancel");
      cancel.type = "button";
      confirm.addEventListener("click", () => {
        this.busy(true);
        confirm.disabled = cancel.disabled = true;
        this.submit(this.form, choice).catch(err => this.report("Error: " + err.message, true)).finally(() => this.busy(false));
      });
      cancel.addEventListener("click", () => this.report(""));
      this.output.replaceChildren(question, confirm, " ", cancel);
      delete this.output.dataset.error;
      confirm.focus();
    }
  }

  // WikiCompose publishes a NIP-54 article. When the signer is not the
  // author of the version in view, the article is a fork of that version,
  // and "propose" also signs a kind 818 merge request to that author. The
  // name follows the title on a new page until it is edited by hand.
  class WikiCompose extends FormElement {
    connectedCallback() {
      super.connectedCallback();
      const title = this.form?.elements?.title, name = this.form?.elements?.name;
      if (this.wired === this.form || !title || !name) return;
      this.wired = this.form;
      let follow = !this.getAttribute("name");
      name.addEventListener("input", () => { follow = name.value.trim() === ""; });
      name.addEventListener("change", () => { name.value = tiny.util.wikiName(name.value); });
      title.addEventListener("input", () => { if (follow) name.value = tiny.util.wikiName(title.value); });
    }

    async submit(form) {
      const value = key => form.elements[key]?.value.trim() || "";
      const title = value("title"), d = tiny.util.wikiName(value("name") || title), summary = value("summary");
      const content = form.elements.content?.value || "";
      if (!title) throw Error("Enter a title.");
      if (!d) throw Error("Enter a name with at least one letter or digit.");
      if (!content.trim()) throw Error("Enter the content.");
      const action = this.submitter?.value || "publish";
      const author = this.getAttribute("author") || "", coordinate = this.getAttribute("coordinate") || "", base = this.getAttribute("event") || "";
      if (!window.nostr?.signEvent) throw Error("Connect a signer first.");
      const pubkey = typeof window.nostr.getPublicKey === "function" ? await window.nostr.getPublicKey() : "";
      const forking = isHex64(author) && author !== pubkey;
      if (action === "propose" && !forking) throw Error("This is your own version; publish it instead.");
      const tags = [["d", d], ["title", title]];
      if (summary) tags.push(["summary", summary]);
      if (forking) {
        if (!/^30818:[0-9a-f]{64}:.+$/.test(coordinate) || !isHex64(base)) throw Error("The version to fork is missing.");
        tags.push(["a", coordinate, "", "fork"], ["e", base, "", "fork"]);
      }
      const version = await publishSigned({kind: 30818, created_at: unixNow(), tags, content});
      if (action === "propose") {
        const request = {kind: 818, created_at: unixNow(), tags: [["a", coordinate], ["e", version.id, "", "source"], ["e", base], ["p", author]], content: summary || "Proposed version of " + title};
        await publishSigned(request);
        this.report("Published and proposed to " + author.slice(0, 12) + ".");
      } else {
        this.report("Published.");
      }
      const path = "/wiki/" + encodeURIComponent(d);
      const href = tiny.localPath ? tiny.localPath(path) : path;
      if (tiny.navigate) {
        globalThis.history?.pushState?.({}, "", href);
        await tiny.navigate(href);
      } else {
        globalThis.location?.assign?.(href);
      }
    }
  }

  // ApprovalItem reacts to the query a notification action opens the page
  // with: id names the request and answer is approve, deny or reply.
  class ApprovalItem extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      const params = new URLSearchParams(location.search);
      if (this.id !== "approval-" + params.get("id")) return;
      this.setAttribute("data-focus", "");
      this.scrollIntoView?.({block: "center"});
      const answer = params.get("answer");
      if (answer === "reply") {
        const details = this.querySelector("details");
        if (details) details.open = true;
        this.querySelector("textarea")?.focus();
      } else if (answer === "approve" || answer === "deny") {
        // The reaction element must have wrapped its form first.
        queueMicrotask(() => this.querySelector("nostr-react")?.prompt(answer));
      }
    }
  }

  // PushToggle opts this browser into relay notifications. Nothing happens
  // until the button is pressed: permission is requested then, and the
  // device subscription is registered with a signed request.
  const pushCategories = [["messages", "private messages"], ["replies", "replies to you"], ["mentions", "mentions"], ["approvals", "requests for a decision"], ["relay", "relay notices"]];
  class PushToggle extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.innerHTML = '<p><button type="button">Enable on this device</button></p><form hidden>' + pushCategories.map(([name, label]) => '<label><input type="checkbox" name="' + name + '" checked> ' + label + '</label>').join("") + '</form><output role="status"></output>';
      this.button = this.querySelector("button");
      this.form = this.querySelector("form");
      this.output = this.querySelector("output");
      const saved = (localStorage.getItem("tiny.push.categories") || "").split(",").filter(Boolean);
      if (saved.length) this.form.querySelectorAll("input").forEach(input => { input.checked = saved.includes(input.name); });
      this.form.addEventListener("change", () => this.register().catch(err => this.say("error: " + err.message, true)));
      if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
        this.button.disabled = true;
        this.say("This browser does not support notifications.");
        return;
      }
      this.button.addEventListener("click", () => this.toggle().catch(err => this.say("error: " + err.message, true)));
      this.refresh().catch(() => {});
    }
    say(text, error) { this.output.textContent = text; if (error) this.output.dataset.error = ""; else delete this.output.dataset.error; }
    async subscription() { return (await navigator.serviceWorker.ready).pushManager.getSubscription(); }
    async refresh() {
      const current = await this.subscription();
      this.enabled = Boolean(current) && localStorage.getItem("tiny.push") === current.endpoint;
      this.button.textContent = this.enabled ? "Disable on this device" : "Enable on this device";
      this.form.hidden = !this.enabled;
      if (!this.enabled && Notification.permission === "denied") this.say("Notifications are blocked in the browser settings.");
    }
    categories() { return [...this.form.querySelectorAll("input")].filter(input => input.checked).map(input => input.name); }
    // register sends the subscription with the chosen categories; it runs on
    // enable and again whenever a category changes.
    async register(current) {
      current = current || (await this.subscription());
      if (!current) return;
      const categories = this.categories();
      await tiny.signedFetch("/push/subscribe", "POST", JSON.stringify({subscription: current.toJSON(), categories}), {contentType: "application/json"});
      localStorage.setItem("tiny.push", current.endpoint);
      localStorage.setItem("tiny.push.categories", categories.join(","));
      this.say(categories.length ? "Notifications are on for this device." : "No categories chosen; nothing will arrive.");
    }
    async toggle() {
      this.button.disabled = true;
      try {
        if (this.enabled) {
          const current = await this.subscription();
          if (current) {
            await tiny.signedFetch("/push/unsubscribe", "POST", JSON.stringify(current.toJSON()), {contentType: "application/json"});
            await current.unsubscribe();
          }
          localStorage.removeItem("tiny.push");
          this.say("Notifications are off on this device.");
        } else {
          if ((await Notification.requestPermission()) !== "granted") throw Error("Permission was not granted.");
          const {key} = await (await fetch(tiny.localPath("/push/key"), {credentials: "same-origin"})).json();
          const registration = await navigator.serviceWorker.ready;
          const current = (await registration.pushManager.getSubscription()) || (await registration.pushManager.subscribe({userVisibleOnly: true, applicationServerKey: tiny.util.fromB64url(key)}));
          await this.register(current);
        }
        await this.refresh();
      } finally { this.button.disabled = false; }
    }
  }

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
      this.load().catch(err => this.say("error: " + err.message, true));
    }

    // whenSigned runs once a signer is available; the bridge announces a
    // connected or resumed remote signer with the tiny:signer event.
    whenSigned(fn) {
      if (window.nostr?.signEvent) return fn();
      this.say("connect a signer to load this list");
      document.addEventListener("tiny:signer", () => fn(), {once: true});
    }

    say(text, error) {
      this.output.textContent = text;
      if (error) this.output.dataset.error = "";
      else delete this.output.dataset.error;
    }

    // load reads with the browser session first so showing the page never
    // asks the signer; a signed call is the fallback without a session.
    async load() {
      this.say("loading…");
      const response = await fetch(tiny.localPath("/connect.json?catalog=1"), {credentials: "same-origin", headers: {Accept: "application/json"}});
      if (response.status === 401) {
        this.whenSigned(() => this.loadSigned().catch(err => this.say("error: " + err.message, true)));
        return;
      }
      if (!response.ok) throw Error("Request failed (" + response.status + ")");
      const data = await response.json();
      this.rows = data.connections;
      this.catalog = data.catalog;
      this.render();
      this.say("");
    }
    async loadSigned() {
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

  // ensureModules loads the feature bundle for any custom element on the
  // page whose definition has not arrived, such as after an in-place
  // navigation to the Files or Connect pages.
  const bundlesFor = {"file-upload": "files", "file-workspace-root": "files", "private-services": "private"};
  // Opening the inbox records the visit for the badge and clears it.
  const markInboxSeen = () => {
    if (!/\/inbox$/.test(location.pathname) || !document.getElementById("session-logout")) return;
    navigator.clearAppBadge?.().catch?.(() => {});
    fetch(tiny.localPath("/inbox/seen"), {method: "POST", credentials: "same-origin"}).catch(() => {});
  };
  document.addEventListener("tiny:navigation", markInboxSeen);
  markInboxSeen();

  const ensureModules = () => {
    if (!tiny.require || typeof customElements.get !== "function") return;
    const wanted = new Set();
    for (const [tag, bundle] of Object.entries(bundlesFor)) {
      if (!customElements.get(tag) && document.querySelector(tag)) wanted.add(bundle);
    }
    wanted.forEach(bundle => tiny.require(bundle).catch(() => {}));
  };
  ensureModules();

  customElements.define("rpc-form", RpcForm);
  customElements.define("connect-card", ConnectCard);
  customElements.define("connect-list", ConnectList);
  customElements.define("relay-lists", RelayLists);
  customElements.define("signed-form", SignedForm);
  customElements.define("file-mirror", FileMirror);
  customElements.define("file-tools", FileTools);
  customElements.define("publish-list", PublishList);
  customElements.define("nostr-compose", NostrCompose);
  customElements.define("agent-grant", AgentGrant);
  customElements.define("profile-form", ProfileForm);
  customElements.define("nostr-react", NostrReact);
  customElements.define("approval-item", ApprovalItem);
  customElements.define("wiki-compose", WikiCompose);
  customElements.define("room-compose", RoomCompose);
  customElements.define("room-create", RoomCreate);
  customElements.define("room-action", RoomAction);
  customElements.define("room-live", RoomLive);
  customElements.define("push-toggle", PushToggle);
  customElements.define("share-link", ShareLink);
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
    const allowedRoute = path => /^(?:\/(?:inbox|approvals|profile|outbox|search|articles|private|chat|media|sites|marmot|grasp|terms|signin|connect|tools|repo|repos|file|files|wiki|rooms)?\/?|\/manage(?:\/(?:people|agents|moderation|rules|identity|connect|data|sync|views|health|owner|status))?\/?|\/(?:invite|e|a|wiki)\/.+|\/rooms\/[a-z0-9_-]{1,64}(?:\/thread\/[0-9a-f]{64})?\/?)$/.test(path || "");
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
      document.querySelectorAll("rpc-form,signed-form,publish-list,agent-grant,profile-form,nostr-react,wiki-compose,room-compose,room-create,room-action").forEach(node => {
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
      ensureModules();
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
    // navigate re-requests a page in place; components use it after a
    // publish. With push set it opens a new address as a history entry.
    tiny.navigate = (href, push = false) => load(new URL(href, location.href), push);
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
