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

  customElements.define("rpc-form", RpcForm);
  customElements.define("relay-lists", RelayLists);
  customElements.define("signed-form", SignedForm);
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
