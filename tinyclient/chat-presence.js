(() => {
  "use strict";
  const tiny = globalThis.tiny;
  const PRESENCE = 20001, TYPING = 20002;
  const hex = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
  const tag = (event, name) => (event.tags || []).find(item => item[0] === name)?.[1] || "";
  const seconds = () => Math.floor(Date.now() / 1000);
  const parse = (event, room, now = seconds()) => {
    if (!event || ![PRESENCE, TYPING].includes(event.kind) || !hex(event.pubkey)) return null;
    const scope = tag(event, "h");
    if (scope !== room && !(event.kind === PRESENCE && !scope)) return null;
    const ttl = event.kind === PRESENCE ? 180 : 5;
    if (!Number.isSafeInteger(event.created_at) || event.created_at > now + 5 || event.created_at + ttl <= now) return null;
    let status = String(event.content || "").trim();
    try { const data = JSON.parse(status); if (typeof data?.status === "string") status = data.status; if (typeof data?.typing === "boolean") status = data.typing ? "typing" : "stopped"; } catch {}
    if (event.kind === TYPING) status = ["stopped", "false", "idle", "offline"].includes(status.toLowerCase()) ? "stopped" : "typing";
    return {pubkey:event.pubkey, kind:event.kind, status:status.slice(0,128) || "online", at:event.created_at, expires:event.created_at+ttl};
  };
  const readPreference = async signal => {
    const response = await fetch(tiny.localPath("/chat/preferences"), {credentials:"same-origin",cache:"no-store",signal});
    if (!response.ok) throw Error("Chat preferences could not be loaded.");
    return (await response.json()).share_presence === true;
  };
  class ChatPreferences extends HTMLElement {
    connectedCallback() {
      this.actor = this.getAttribute("actor"); this.input = this.querySelector("input"); this.output = this.querySelector("output");
      if (!hex(this.actor) || !this.input) return;
      this.controller = new AbortController(); this.input.disabled = true;
      this.onChange = () => this.save();
      this.input.addEventListener("change",this.onChange);
      this.onLogout = () => { this.controller.abort(); this.input.disabled = true; };
      document.addEventListener("tiny:logout",this.onLogout);
      this.load();
    }
    disconnectedCallback() { this.controller?.abort(); this.input?.removeEventListener("change",this.onChange); document.removeEventListener("tiny:logout",this.onLogout); }
    async load() {
      try {
        this.saved = await readPreference(this.controller.signal);
        if (!this.isConnected || this.controller.signal.aborted) return;
        this.input.checked = this.saved; this.input.disabled = false;
      } catch(error) { if (error.name !== "AbortError") this.output.textContent = error.message; }
    }
    async save() {
      if (this.saving) return;
      this.saving = true; this.input.disabled = true;
      try {
        const signer = tiny.signer();
        if (!signer || await signer.getPublicKey() !== this.actor || tiny.signer() !== signer) throw Error("Connect the signer for this account.");
        if (!this.isConnected || this.controller.signal.aborted) return;
        const response = await tiny.signedFetch("/chat/preferences","POST",JSON.stringify({share_presence:this.input.checked}),{contentType:"application/json",signal:this.controller.signal});
        this.saved = (await response.json()).share_presence === true;
        if (!this.isConnected || this.controller.signal.aborted) return;
        this.input.checked = this.saved; this.output.textContent = "Saved.";
        document.dispatchEvent(new CustomEvent("tiny:chat-preferences",{detail:{actor:this.actor,share_presence:this.saved}}));
      } catch(error) {
        this.input.checked = this.saved;
        if (error.name !== "AbortError") this.output.textContent = error.message;
      } finally { this.saving = false; this.input.disabled = this.controller.signal.aborted; }
    }
  }
  class ChatPresence extends HTMLElement {
    connectedCallback() {
      this.room = this.getAttribute("room"); this.actor = this.getAttribute("actor");
      if (!/^[a-z0-9_-]{1,64}$/.test(this.room || "")) return;
      this.values = new Map(); this.entries = new Map(); this.last = {}; this.pending = new Set(); this.generation = 0;
      this.status = this.querySelector("output"); this.controller = new AbortController();
      this.enabled = false; this.signerReadyVersion = 0; this.preferenceReload = false;
      this.onEvent = event => { if (event.detail?.room === this.room) this.receive(event.detail.event); };
      this.onInput = event => { if (event.target.matches('room-compose textarea')) { this.inputAt = seconds(); this.publish(TYPING, event.target.value ? "typing" : "stopped"); } };
      this.onVisibility = () => { this.generation++; this.values.clear(); this.paint(); if (document.hidden) { this.publish(TYPING,"stopped",true); this.publish(PRESENCE,"offline",true); } else { this.loadPreference(); this.tick(); } };
      this.onIdentity = () => { this.enabled = false; this.generation++; this.values.clear(); this.paint(); };
      this.clearFailure = () => {
        this.failed = false;
        if (this.status?.textContent.includes("Presence could not be shared")) {
          this.status.textContent = "";
          this.paint();
        }
      };
      this.onSigner = () => { this.onIdentity(); this.signerReadyVersion++; this.clearFailure(); this.loadPreference(); };
      this.onSignerStatus = event => {
        if (event.detail?.status !== "ready") return;
        this.signerReadyVersion++; this.clearFailure();
        this.loadPreference();
      };
      this.onPreference = event => { if (event.detail?.actor === this.actor) this.applyPreference(event.detail.share_presence === true); };
      document.addEventListener("tiny:room-event", this.onEvent);
      document.addEventListener("input", this.onInput);
      document.addEventListener("visibilitychange", this.onVisibility);
      document.addEventListener("tiny:signer", this.onSigner);
      document.addEventListener("tiny:signer-status", this.onSignerStatus);
      document.addEventListener("tiny:logout", this.onIdentity);
      document.addEventListener("tiny:chat-preferences",this.onPreference);
      this.loadPreference();
      this.timer = setInterval(() => this.tick(),1000);
    }
    disconnectedCallback() {
      this.publish(TYPING,"stopped",true); this.publish(PRESENCE,"offline",true);
      this.enabled = false; this.generation++; this.controller?.abort(); clearInterval(this.timer);
      document.removeEventListener("tiny:room-event",this.onEvent); document.removeEventListener("input",this.onInput);
      document.removeEventListener("visibilitychange",this.onVisibility); document.removeEventListener("tiny:signer",this.onSigner); document.removeEventListener("tiny:signer-status",this.onSignerStatus); document.removeEventListener("tiny:logout",this.onIdentity);
      document.removeEventListener("tiny:chat-preferences",this.onPreference); this.values?.clear();
    }
    applyPreference(enabled) {
      if (this.enabled === enabled) return;
      this.enabled = enabled; this.generation++;
      if (enabled) this.tick();
      else { this.publish(TYPING,"stopped",true); this.publish(PRESENCE,"offline",true); }
    }
    async loadPreference() {
      if (!hex(this.actor) || this.failed || this.controller.signal.aborted) return;
      if (this.loadingPreference) { this.preferenceReload = true; return; }
      this.loadingPreference = true; const generation = this.generation;
      try {
        const enabled = await readPreference(this.controller.signal);
        if (this.isConnected && !this.controller.signal.aborted && generation === this.generation) this.applyPreference(enabled);
      } catch { this.applyPreference(false); }
      finally {
        this.loadingPreference = false; this.preferenceCheckedAt = seconds();
        if (this.preferenceReload && this.isConnected && !this.controller.signal.aborted) {
          this.preferenceReload = false;
          this.loadPreference();
        }
      }
    }
    receive(event) {
      const value = parse(event,this.room); if (!value || value.pubkey === this.actor) return;
      const id = value.kind+":"+value.pubkey, previous = this.values.get(id);
      if (previous && previous.at > value.at) return;
      this.values.set(id,value);
      while (this.values.size > 256) this.values.delete(this.values.keys().next().value);
      this.paint();
    }
    tick() {
      if (document.hidden) return;
      this.paint();
      if (seconds()-(this.preferenceCheckedAt || 0) >= 60) this.loadPreference();
      if (this.enabled) this.publish(PRESENCE,"online");
    }
    async publish(kind, content, leaving = false) {
      if ((!this.enabled && !leaving) || (!this.last[kind] && leaving) || (document.hidden && !leaving) || !hex(this.actor) || this.pending.has(kind)) return;
      const at = seconds(), interval = kind === PRESENCE ? 60 : 3;
      if (!leaving && at-(this.last[kind] || 0) < interval) return;
      const signer = tiny.signer?.(); if (!signer?.signEvent) return;
      const generation = this.generation, readyVersion = this.signerReadyVersion;
      this.pending.add(kind);
      try {
        if (await signer.getPublicKey() !== this.actor || tiny.signer() !== signer) return;
        if (!leaving && (generation !== this.generation || !this.isConnected || !this.enabled || document.hidden)) return;
        await tiny.signing.publish({kind, created_at:at, tags:[["h",this.room]], content});
        this.last[kind] = at;
      } catch {
        const stale = generation !== this.generation || readyVersion !== this.signerReadyVersion || signer !== tiny.signer?.();
        if (stale || leaving || !this.isConnected) return;
        this.enabled = false; this.failed = true;
        if (this.status) this.status.textContent = "Presence could not be shared. Reconnect your signer to try again.";
      } finally { this.pending.delete(kind); }
    }
    paint() {
      if (!this.status) return;
      const now = seconds(); for (const [id,value] of this.values) if (value.expires <= now) this.values.delete(id);
      const values = [...this.values.values()].filter(v => v.status !== "offline" && v.status !== "stopped"), typers = new Set(values.filter(v => v.kind === TYPING).map(v => v.pubkey));
      const visible = values.filter(v => v.kind === TYPING || !typers.has(v.pubkey)).sort((a,b) => a.pubkey.localeCompare(b.pubkey)).slice(0,8);
      const active = new Set(visible.map(value => value.pubkey));
      for (const [key, entry] of this.entries) if (!active.has(key)) { entry.node.remove(); this.entries.delete(key); }
      visible.forEach((value,index) => {
        let entry = this.entries.get(value.pubkey);
        if (!entry) {
          const node = document.createElement("span"), name = document.createElement("nostr-name"), status = document.createElement("span");
          name.setAttribute("pubkey",value.pubkey); name.textContent = value.pubkey.slice(0,12);
          node.append(name,status); entry = {node,status}; this.entries.set(value.pubkey,entry);
        }
        const text = " "+(value.kind === TYPING ? "is typing…" : value.status);
        if (entry.status.textContent !== text) entry.status.textContent = text;
        if (this.status.children[index] !== entry.node) this.status.insertBefore(entry.node,this.status.children[index] || null);
      });
    }
  }
  tiny.roomPresence = Object.freeze({parse, PRESENCE,TYPING});
  customElements.define("chat-presence",ChatPresence);
  customElements.define("chat-preferences",ChatPreferences);
})();
