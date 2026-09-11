/* Direct conversations use verified NIP-17 rumors. Plaintext lives only in this page's memory. */
(() => {
  "use strict";
  const tiny = globalThis.tiny;
  const protocol = () => tiny.chat.protocol;
  const hex = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
  const node = (name, text) => {
    const result = document.createElement(name);
    if (text !== undefined) result.textContent = text;
    return result;
  };
  const key = value => { try { return protocol().normalizePubkey(String(value || "").trim()); } catch { return ""; } };
  const participants = rumor => [...new Set([rumor.pubkey, ...rumor.tags.filter(tag => tag[0] === "p" && hex(tag[1])).map(tag => tag[1])])];
  const states = new Map();
  const listeners = new Set();
  let generation = 0;
  const stateFor = actor => {
    if (!states.has(actor)) states.set(actor, {actor, generation, events: new Map(), wraps: new Map(), cursor: "", loaded: false, pending: null, version: 0});
    return states.get(actor);
  };
  const current = value => value.generation === generation && states.get(value.actor) === value;
  const changed = value => { if (current(value)) { value.version++; for (const host of listeners) if (host.actor === value.actor) host.render(); } };
  const clear = () => {
    generation++;
    for (const value of states.values()) { value.events.clear(); value.wraps.clear(); }
    states.clear();
    for (const host of listeners) { host.renderVersion = ""; host.replaceChildren(node("p", "Connect your signer to open direct chats.")); }
  };
  const activeSigner = async actor => {
    const signer = tiny.signer?.();
    if (!signer || key(await signer.getPublicKey()) !== actor) throw Error("Connect the signer for this account to open direct chats.");
    return signer;
  };
  const checkSession = async (value, signer) => {
    if (!current(value) || tiny.signer() !== signer || key(await signer.getPublicKey()) !== value.actor) throw Error("The signer changed. Reopen this conversation.");
    if (!current(value)) throw Error("The session ended.");
  };
  const cacheEvents = async (value, rows, signer) => {
    for (const packet of rows) {
      if (!current(value)) return;
      let pending = value.wraps.get(packet.id);
      if (!pending) {
        pending = protocol().decrypt(packet, {actor: value.actor, signer});
        value.wraps.set(packet.id, pending);
      }
      try {
        const {rumor} = await pending;
        await checkSession(value, signer);
        if (value.events.has(rumor.id)) continue;
        const members = participants(rumor);
        if ([14, 15].includes(rumor.kind) && (members.length !== 2 || !members.includes(value.actor))) continue;
        if (![5, 7, 14, 15].includes(rumor.kind)) continue;
        value.events.set(rumor.id, rumor);
      } catch (error) {
        if (!current(value)) return;
        // A signer refusal can be retried. Structurally invalid events stay cached.
        if (/cancel|reject|denied|support|connect|unable to decrypt/i.test(error.message)) {
          value.wraps.delete(packet.id);
          throw error;
        }
      }
    }
  };
  const fetchPage = async (value, cursor, signer) => {
    const response = await tiny.signedFetch("/chat/events" + (cursor ? "?cursor=" + encodeURIComponent(cursor) : ""), "GET", new Uint8Array());
    const page = await response.json();
    if (!Array.isArray(page?.events) || typeof page.next_cursor !== "string") throw Error("Could not read the chat history.");
    await checkSession(value, signer);
    await cacheEvents(value, page.events, signer);
    return page;
  };
  const load = async (actor, older = false) => {
    const value = stateFor(actor);
    if (value.pending) return value.pending;
    if (older && !value.cursor) return value;
    value.pending = (async () => {
      const signer = await activeSigner(actor);
      await checkSession(value, signer);
      const wasLoaded = value.loaded, count = value.events.size;
      let page = await fetchPage(value, older ? value.cursor : "", signer);
      if (!current(value)) return value;
      if (older || !wasLoaded) value.cursor = page.next_cursor;
      // Gift wraps deliberately use older dates. Refresh the entire two-day
      // overlap so a new message cannot hide behind the first page of history.
      if (wasLoaded && !older) {
        const cutoff = Math.floor(Date.now() / 1000) - 172860;
        const seen = new Set();
        while (page.next_cursor && page.events.length && page.events.at(-1).created_at >= cutoff && !seen.has(page.next_cursor)) {
          seen.add(page.next_cursor);
          page = await fetchPage(value, page.next_cursor, signer);
          if (!current(value)) return value;
        }
      }
      value.loaded = true;
      if (count !== value.events.size || !wasLoaded || older) changed(value);
      return value;
    })().finally(() => { value.pending = null; });
    return value.pending;
  };
  const chronological = value => [...value.events.values()].sort((a, b) => a.created_at - b.created_at || a.id.localeCompare(b.id));
  const peerFor = (rumor, actor) => participants(rumor).find(member => member !== actor);
  const deleted = (value, rumor) => {
    if (value.deletedVersion !== value.events.size) {
      value.deletedIDs = new Set();
      for (const deletion of value.events.values()) {
        if (deletion.kind !== 5) continue;
        for (const tag of deletion.tags) {
          const target = tag[0] === "e" && value.events.get(tag[1]);
          if (target && target.pubkey === deletion.pubkey) value.deletedIDs.add(target.id);
        }
      }
      value.deletedVersion = value.events.size;
    }
    return value.deletedIDs.has(rumor.id);
  };
  const messages = (value, peer) => chronological(value).filter(rumor => [14, 15].includes(rumor.kind) && !deleted(value, rumor) && (!peer || peerFor(rumor, value.actor) === peer));
  const profile = pubkey => {
    const author = node("span"); author.setAttribute("data-author", "");
    const avatar = node("nostr-avatar", pubkey.slice(0, 2)); avatar.setAttribute("pubkey", pubkey); avatar.setAttribute("aria-hidden", "true");
    const name = node("nostr-name", pubkey.slice(0, 8)); name.setAttribute("pubkey", pubkey);
    author.append(avatar, name); return author;
  };
  const time = timestamp => {
    const date = new Date(timestamp * 1000);
    const result = node("time", date.toLocaleString()); result.dateTime = date.toISOString(); return result;
  };
  const textBody = content => {
    const body = node("p");
    if (tiny.rooms?.linkify) tiny.rooms.linkify(body, content); else body.textContent = content;
    return body;
  };
  const targetID = rumor => rumor.tags.filter(tag => ["e", "q"].includes(tag[0]) && hex(tag[1])).at(-1)?.[1];
  const reactions = (value, target) => {
    if (value.reactionVersion !== value.events.size) {
      const grouped = new Map();
      for (const rumor of value.events.values()) {
        if (rumor.kind !== 7 || deleted(value, rumor)) continue;
        const target = value.events.get(targetID(rumor));
        if (!target || ![14, 15].includes(target.kind)) continue;
        const allowed = new Set(participants(target));
        if (!allowed.has(rumor.pubkey) || participants(rumor).some(pubkey => !allowed.has(pubkey))) continue;
        const label = !rumor.content || rumor.content === "+" ? "♥" : rumor.content;
        if (!grouped.has(target.id)) grouped.set(target.id, new Map());
        grouped.get(target.id).set(rumor.pubkey + ":" + label, label);
      }
      value.reactionIndex = new Map();
      for (const [id, unique] of grouped) {
        const counts = new Map();
        for (const label of unique.values()) counts.set(label, (counts.get(label) || 0) + 1);
        value.reactionIndex.set(id, counts);
      }
      value.reactionVersion = value.events.size;
    }
    return value.reactionIndex.get(target.id) || new Map();
  };
  const accepted = values => Array.isArray(values) ? values.some(value => value === true) : values === true;
  const deliver = async (value, input, previous, report) => {
    const signer = await activeSigner(value.actor);
    await checkSession(value, signer);
    const pending = previous || {built: await protocol().build(input, {signer}), delivered: [false, false]};
    await checkSession(value, signer);
    if (pending.built.rumor.pubkey !== value.actor) throw Error("The signer changed.");
    if (!pending.relays) {
      const lists = await Promise.all([protocol().queryRelayList(input.recipient), protocol().queryRelayList(value.actor)]);
      pending.relays = lists.map((list, index) => protocol().relayList(list, index ? value.actor : input.recipient));
    }
    await checkSession(value, signer);
    // Retain the exact gift wraps before network publication, including failure.
    report(pending);
    for (let index = 0; index < 2; index++) {
      if (pending.delivered[index]) continue;
      const result = await protocol().publishEvent(pending.built.events[index].event, pending.relays[index], {signer});
      await checkSession(value, signer);
      if (!accepted(result)) throw Error(index ? "Sent to the recipient's relay. Your history copy failed; press Send again to retry it." : "The recipient's relay did not accept the message. Press Send to retry.");
      pending.delivered[index] = true;
      if (index === 0) { value.events.set(pending.built.rumor.id, pending.built.rumor); changed(value); }
    }
    return pending;
  };
  class ConversationView extends HTMLElement {
    connectedCallback() {
      this.actor = key(this.getAttribute("actor")); this.peer = key(this.getAttribute("peer"));
      this.renderVersion = ""; listeners.add(this); this.refresh();
      this.timer = setInterval(() => { if (document.visibilityState !== "hidden") this.refresh(); }, 10000);
    }
    disconnectedCallback() { listeners.delete(this); clearInterval(this.timer); }
    async refresh(older = false) {
      if (!this.actor || !tiny.signer?.()) return;
      const epoch = generation;
      try { await load(this.actor, older); if (epoch === generation && this.isConnected) this.render(); }
      catch (error) { if (epoch === generation && this.isConnected) { let status = this.querySelector(":scope > output"); if (!status) { status = node("output"); status.setAttribute("role", "status"); this.append(status); } status.textContent = error.message; } }
    }
    unchanged(value) {
      const version = [value.version, value.cursor, this.peer].join(":");
      if (this.renderVersion === version) return true;
      this.renderVersion = version; return false;
    }
    older(value) {
      if (!value.cursor) return;
      const button = node("button", "Load older conversations"); button.type = "button";
      button.addEventListener("click", async () => { button.disabled = true; try { await this.refresh(true); } finally { button.disabled = false; } });
      this.append(button);
    }
  }
  class DirectChats extends ConversationView {
    render() {
      const value = stateFor(this.actor); if (this.unchanged(value)) return;
      const groups = new Map(); for (const rumor of messages(value)) groups.set(peerFor(rumor, this.actor), rumor);
      this.replaceChildren();
      for (const [peer, latest] of [...groups].sort((a, b) => b[1].created_at - a[1].created_at || a[0].localeCompare(b[0]))) {
        const link = node("a"); link.href = tiny.localPath("/chat/dm/" + peer);
        if (location.pathname.endsWith("/chat/dm/" + peer)) link.setAttribute("aria-current", "page");
        link.append(profile(peer), node("p", latest.kind === 15 ? "Encrypted attachment" : latest.content.slice(0, 120)), time(latest.created_at)); this.append(link);
      }
      if (!groups.size) this.append(node("p", "No direct conversations yet."));
      if (!this.hasAttribute("compact")) this.older(value);
    }
  }
  class DirectThread extends ConversationView {
    render() {
      const value = stateFor(this.actor); if (this.unchanged(value)) return;
      const near = !this.childElementCount || this.scrollHeight - this.scrollTop - this.clientHeight < 100;
      const top = this.scrollTop, height = this.scrollHeight;
      const rows = messages(value, this.peer), existing = new Map([...this.querySelectorAll(":scope > article")].map(article => [article.id, article]));
      const result = [];
      if (value.cursor) {
        const button = node("button", "Load older messages"); button.type = "button"; button.addEventListener("click", () => this.refresh(true)); result.push(button);
      }
      for (const rumor of rows) {
        let article = existing.get("chat-message-" + rumor.id);
        if (!article) {
          article = node("article"); article.id = "chat-message-" + rumor.id;
          if (rumor.pubkey === this.actor) article.dataset.own = "";
          const header = node("header"); header.append(profile(rumor.pubkey), time(rumor.created_at)); article.append(header);
          const parent = targetID(rumor);
          if (parent) { const quote = node("blockquote"); quote.dataset.quote = parent; article.append(quote); }
          if (rumor.kind === 15 && tiny.chat.files) article.append(tiny.chat.files.render(rumor)); else article.append(textBody(rumor.content));
          const footer = node("footer");
          const reply = node("button", "Reply"); reply.type = "button";
          reply.addEventListener("click", () => document.querySelector(`direct-compose[peer="${this.peer}"]`)?.replyTo(rumor)); footer.append(reply);
          const react = node("button", "♥"); react.type = "button"; react.title = "Like this message";
          react.addEventListener("click", async () => {
            react.disabled = true;
            try { await deliver(value, {recipient: this.peer, reaction: {id: rumor.id, pubkey: rumor.pubkey, kind: rumor.kind, content: "+"}}, react.pending, pending => { react.pending = pending; }); react.pending = null; }
            catch (error) { let status = footer.querySelector("output"); if (!status) { status = node("output"); footer.append(status); } status.textContent = error.message; }
            finally { react.disabled = false; }
          }); footer.append(react, node("span")); article.append(footer);
        }
        const quote = article.querySelector("blockquote[data-quote]");
        if (quote) {
          const parent = value.events.get(quote.dataset.quote);
          quote.textContent = parent && [14, 15].includes(parent.kind) && peerFor(parent, this.actor) === this.peer
            ? deleted(value, parent) ? "Message deleted" : parent.content.slice(0, 220)
            : "Reply to an earlier message";
        }
        const summary = article.querySelector("footer > span"); summary.textContent = [...reactions(value, rumor)].map(([label, count]) => `${label} ${count}`).join(" | ");
        result.push(article);
      }
      if (!rows.length) result.push(node("p", "Start the conversation with a private message."));
      // Move existing message nodes instead of rebuilding media players.
      let before = this.firstChild;
      for (const child of result) { if (before === child) before = before.nextSibling; else this.insertBefore(child, before); }
      while (before) { const next = before.nextSibling; before.remove(); before = next; }
      if (near) this.scrollTop = this.scrollHeight;
      else this.scrollTop = top + Math.max(0, this.scrollHeight - height);
    }
  }
  class DirectCompose extends tiny.ui.FormElement {
    connectedCallback() {
      if (this.form) return;
      super.connectedCallback();
      this.querySelector("textarea")?.addEventListener("keydown", event => {
        if (event.key === "Enter" && !event.shiftKey && !event.isComposing) { event.preventDefault(); if (!this.submitting) this.form.requestSubmit(); }
      });
    }
    replyTo(rumor) {
      this.reply = {id: rumor.id, pubkey: rumor.pubkey};
      this.report("Replying to: " + rumor.content.slice(0, 100));
      let cancel = this.querySelector("[data-cancel-reply]");
      if (!cancel) { cancel = node("button", "Cancel reply"); cancel.type = "button"; cancel.setAttribute("data-cancel-reply", ""); cancel.addEventListener("click", () => { this.reply = null; cancel.remove(); this.report(""); }); this.append(cancel); }
      this.querySelector("textarea")?.focus();
    }
    async submit(form) {
      const actor = key(this.getAttribute("actor")), peer = key(this.getAttribute("peer")), content = String(form.elements.content.value || "");
      if (!actor || !peer || actor === peer || !content.trim()) throw Error("Choose another person and write a message.");
      const value = stateFor(actor), pendingKey = JSON.stringify([actor, peer, content, this.reply?.id]);
      if (this.pendingKey !== pendingKey) { this.pending = null; this.pendingKey = pendingKey; }
      this.report(this.pending ? "Retrying delivery…" : "Encrypting and sending…");
      await deliver(value, {recipient: peer, content, reply: this.reply}, this.pending, pending => { this.pending = pending; });
      if (!current(value) || !this.isConnected) return;
      this.pending = null;
      if (form.elements.content.value === content && pendingKey === JSON.stringify([actor, peer, content, this.reply?.id])) {
        this.reply = null; this.querySelector("[data-cancel-reply]")?.remove(); form.reset();
      }
      this.report("Sent.");
    }
  }
  class DirectStart extends tiny.ui.FormElement {
    async submit(form) {
      const peer = key(form.elements.peer.value); if (!peer) throw Error("Enter an npub or a 64-character public key.");
      await tiny.navigate(tiny.localPath("/chat/dm/" + peer));
    }
  }
  class DirectSettings extends HTMLElement {
    connectedCallback() {
      const button = node("button", "Receive chats here"); button.type = "button";
      const status = node("output"); status.setAttribute("role", "status"); this.replaceChildren(button, status);
      button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          const actor = key(this.getAttribute("actor")), value = stateFor(actor), signer = await activeSigner(actor);
          const url = tiny.util.relayURL(this.getAttribute("relay")); if (!url) throw Error("This relay's address is unavailable.");
          const rows = await protocol().queryRelayList(actor);
          const events = Array.isArray(rows) ? rows : rows?.events || [];
          const prior = events.filter(event => event.kind === 10050 && event.pubkey === actor && NostrSigner.verifyEvent(event)).sort((a, b) => b.created_at - a.created_at || a.id.localeCompare(b.id))[0];
          const tags = (prior?.tags || []).filter(tag => tag[0] !== "relay" || tiny.util.relayURL(tag[1]));
          if (!tags.some(tag => tag[0] === "relay" && tiny.util.relayURL(tag[1]) === url)) tags.push(["relay", url]);
          await checkSession(value, signer);
          await tiny.signing.publish({kind: 10050, created_at: Math.max(Math.floor(Date.now() / 1000), (prior?.created_at || 0) + 1), tags, content: prior?.content || ""});
          status.textContent = "This relay now receives your direct messages.";
        } catch (error) { status.textContent = error.message; } finally { button.disabled = false; }
      });
    }
  }
  document.addEventListener("tiny:signer", () => { clear(); for (const host of listeners) host.refresh(); });
  document.addEventListener("tiny:logout", clear);
  document.addEventListener("visibilitychange", () => { if (document.visibilityState !== "hidden") for (const host of listeners) host.refresh(); });
  for (const [name, element] of [["direct-chats", DirectChats], ["direct-thread", DirectThread], ["direct-compose", DirectCompose], ["direct-start", DirectStart], ["direct-settings", DirectSettings]]) customElements.define(name, element);
  tiny.chat.ui = Object.freeze({state: states, stateFor, clear, cacheEvents, participants, reactions, load, deliver, messages});
})();
