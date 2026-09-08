// Custom elements for the plain HTML pages. Every element renders in the
// light DOM with document APIs, takes plain lowercase attributes, and carries
// no styling of its own; page CSS targets the element name directly.
//
//   <rpc-form method="…" [compose="…"] [action="…"] [terms="…"] [json-params]>
//     One signed NIP-86 management call. Children become the form body; the
//     element appends an <output> status line and a <json-view> result.
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
        this.busy(true);
        try {
          await this.submit(form);
        } catch (err) {
          this.report("Error: " + err.message, true);
        } finally {
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
      const body = JSON.stringify({method, params});
      this.report("Signing…");
      const endpoint = tiny.localPath(this.getAttribute("action")||"/manage/rpc");
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
      const result = payload && "result" in payload ? payload.result : payload ?? text;
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

  customElements.define("rpc-form", RpcForm);
  customElements.define("signed-form", SignedForm);
  customElements.define("publish-list", PublishList);
  customElements.define("nostr-key", NostrKey);
  customElements.define("json-view", JsonView);
})();
