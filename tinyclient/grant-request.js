(() => {
  "use strict";
  const tiny = globalThis.tiny;
  const stable = value => JSON.stringify(value, function(key,item) { return item && !Array.isArray(item) && typeof item === "object" ? Object.fromEntries(Object.keys(item).sort().map(name=>[name,item[name]])) : item; });
  class GrantRequest extends HTMLElement {
    connectedCallback() {
      if (!this.onClick) {
        this.onClick = event => {
          const button = event.target.closest("[data-grant-approve], [data-grant-deny]");
          if (button && this.contains(button)) this.decide(button, button.hasAttribute("data-grant-approve"));
        };
        this.addEventListener("click", this.onClick);
      }
      this.setBusy(false);
    }
    disconnectedCallback() { this.controller?.abort(); }
    setBusy(busy) {
      this.busy = busy;
      this.querySelectorAll("button").forEach(button => {
        button.disabled = busy || (button.hasAttribute("data-grant-approve") && this.hasAttribute("data-stale"));
      });
    }
    report(text) {
      let output = this.querySelector("output");
      if (!output) { output = document.createElement("output"); output.setAttribute("role", "status"); this.append(output); }
      output.textContent = text;
    }
    async latest(id) {
      const query = new URLSearchParams({method: "browseapproval", params: JSON.stringify([{id}])});
      const response = await fetch(tiny.localPath("/webmcp/query") + "?" + query, {credentials: "same-origin", cache: "no-store", signal: this.controller.signal});
      const value = await response.json();
      if (!response.ok || value?.error) throw Error(value?.error?.message || value?.error || "Could not refresh this request.");
      return value.result || value;
    }
    async checkSigner(signer, operator) {
      if (!signer?.getPublicKey || await signer.getPublicKey() !== operator || tiny.signer() !== signer) throw Error("Connect the requested operator's signer.");
      if (!this.isConnected || this.controller.signal.aborted) throw Error("This request is no longer open.");
    }
    async decide(button, approve) {
      if (this.busy) return;
      this.setBusy(true);
      this.controller = new AbortController();
      try {
        const review = JSON.parse(this.getAttribute("data-review") || "{}");
        const signer = tiny.signer();
        await this.checkSigner(signer, review.operator);
        this.report("Checking current permissions…");
        const {item} = await this.latest(review.request_id), fresh = item?.grant;
        if (!item || item.id !== review.request_id || item.type !== "grant" || item.state !== "open" || item.asker !== review.agent || item.asked?.length !== 1 || item.asked[0] !== review.operator) throw Error("This request is no longer waiting for your decision.");
        if (approve && (!fresh || item.grant_error || fresh.request_id !== review.request_id || fresh.base !== review.base || fresh.agent !== review.agent || fresh.operator !== review.operator || stable(fresh.before) !== stable(review.before) || stable(fresh.after) !== stable(review.after))) throw Error("Permissions changed. Reload and review this request again.");
        await this.checkSigner(signer, review.operator);
        const unsigned = approve ? fresh.unsigned : {kind: 7, created_at: Math.floor(Date.now()/1000), tags: [["e", review.request_id, "", review.agent], ["p", review.agent], ["k", "1111"]], content: "-"};
        if (approve && (unsigned?.kind !== 30392 || unsigned.pubkey !== review.operator || !unsigned.tags.some(tag => tag[0] === "grant-request" && tag[1] === review.request_id) || !unsigned.tags.some(tag => tag[0] === "grant-base" && tag[1] === review.base))) throw Error("The grant review is incomplete.");
        this.report(approve ? "Signing grant…" : "Signing denial…");
        await tiny.signing.publish({kind:unsigned.kind,created_at:unsigned.created_at,tags:unsigned.tags,content:unsigned.content});
        this.report(approve ? "Grant updated." : "Request denied.");
        if (this.isConnected) await tiny.navigate(location.href);
      } catch (error) {
        if (this.isConnected) { this.report(error.message || String(error)); this.setBusy(false); }
      }
    }
  }
  customElements.define("agent-grant-request", GrantRequest);
})();
