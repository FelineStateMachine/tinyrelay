(() => {
  "use strict";
  const tiny=globalThis.tiny;
  class ChatActivity extends HTMLElement {
    connectedCallback() {
      if (this.running) return;
      this.running=true; this.stopped=false; this.version="";
      this.onEvent=e=>{ if (e.detail?.room===this.getAttribute("room")) this.schedule(); };
      this.onVisibility=()=>{ if (!document.hidden) this.refresh(); };
      this.onLogout=()=>{ this.stopped=true; this.abort?.(); this.replaceChildren(); };
      this.onConfig=e=>{
        const cfg=e.detail?.cfg;
        if(e.target!==this || cfg?.trigger?.target!==this) return;
        const force=this.forceRefresh;
        cfg.credentials="same-origin"; cfg.cache="no-store"; cfg.transition=false;
        cfg.headers={...cfg.headers,Accept:"text/html"};
        this.abort=cfg.abort;
        cfg.swap=()=>this.swap(cfg,force);
      };
      this.onFinally=e=>{ if(e.target===this) { this.loading=false; this.abort=null; queueMicrotask(()=>{this.resolveRefresh?.();this.resolveRefresh=null;}); } };
      this.onError=e=>{ if(e.target===this && e.detail?.error?.name!=="AbortError") this.setAttribute("title","Chat activity could not be refreshed."); };
      document.addEventListener("tiny:room-event",this.onEvent);
      document.addEventListener("visibilitychange",this.onVisibility);
      document.addEventListener("tiny:logout",this.onLogout);
      this.addEventListener("fx:config",this.onConfig);
      this.addEventListener("fx:finally",this.onFinally);
      this.addEventListener("fx:error",this.onError);
      this.timer=setInterval(()=>this.refresh(),15000);
      queueMicrotask(()=>{if(this.isConnected && !this.stopped){this.dispatchEvent(new CustomEvent("fx:process",{bubbles:true}));this.refresh();}});
    }
    disconnectedCallback() {
      this.running=false;this.stopped=true;this.abort?.();clearInterval(this.timer);clearTimeout(this.debounce);
      document.removeEventListener("tiny:room-event",this.onEvent);document.removeEventListener("visibilitychange",this.onVisibility);document.removeEventListener("tiny:logout",this.onLogout);
      this.removeEventListener("fx:config",this.onConfig);this.removeEventListener("fx:finally",this.onFinally);this.removeEventListener("fx:error",this.onError);
      this.resolveRefresh?.();this.resolveRefresh=null;this.loading=false;
    }
    schedule() { if(!this.debounce)this.debounce=setTimeout(()=>{this.debounce=null;this.refresh();},400); }
    async read() {
      const url=new URL(tiny.localPath(this.getAttribute("endpoint")),location.href);
      if(url.origin!==location.origin)throw Error("Invalid activity address.");
      const response=await fetch(url,{credentials:"same-origin",cache:"no-store"});
      if(!response.ok)throw Error("Chat activity could not be refreshed.");
      return response.text();
    }
    async refresh(force=false) {
      if(this.loading || this.stopped || document.hidden || (!force && this.contains(document.activeElement)) || !this.__fixi)return;
      this.loading=true;this.forceRefresh=force;
      return new Promise(resolve=>{this.resolveRefresh=resolve;this.dispatchEvent(new CustomEvent("tiny:activity-refresh"));});
    }
    swap(cfg,force=false) {
      if(this.stopped || !this.isConnected || !cfg.response.ok || cfg.text===this.version || (!force && this.contains(document.activeElement)))return;
      const opened=new Set([...this.querySelectorAll("details[open][data-task]")].map(el=>el.dataset.task));
      morph(cfg.target,cfg.text);
      cfg.target.querySelectorAll("details[data-task]").forEach(el=>{el.open=opened.has(el.dataset.task);});
      cfg.target.querySelectorAll("chat-decision").forEach(el=>{if(!el.form?.isConnected){el.form=null;el.output=null;el.connectedCallback();}});
      this.version=cfg.text;
    }
  }
  class ChatDecision extends tiny.ui.FormElement {
    connectedCallback() { if(this.form) return; super.connectedCallback(); }
    async submit(form) {
      const host=this.closest("chat-activity"),id=this.getAttribute("event"),author=this.getAttribute("pubkey"),actor=this.getAttribute("actor"),room=this.getAttribute("room");
      const signer=tiny.signer();
      if(!signer || await signer.getPublicKey()!==actor || tiny.signer()!==signer) throw Error("Connect the signer for this account.");
      const fresh=document.createElement("template"); fresh.innerHTML=await host.read();
      const allowed=[...fresh.content.querySelectorAll("chat-decision")].find(el=>el.getAttribute("event")===id && el.getAttribute("actor")===actor && el.getAttribute("pubkey")===author);
      if(!allowed) { host.schedule(); throw Error("This request is no longer waiting for your answer."); }
      let unsigned;
      if(this.hasAttribute("question")) {
        const content=(form.elements.content?.value || "").trim(); if(!content) throw Error("Write an answer first.");
        unsigned={kind:1111,created_at:Math.floor(Date.now()/1000),tags:[["h",room],["E",id,"",author],["K",this.getAttribute("kind")],["P",author],["e",id,"",author],["k",this.getAttribute("kind")],["p",author]],content};
      } else {
        const choice=this.submitter?.value;
        if(!["+","-"].includes(choice)) throw Error("Choose Approve or Deny.");
        unsigned={kind:7,created_at:Math.floor(Date.now()/1000),tags:[["h",room],["e",id,"",author],["p",author],["k",this.getAttribute("kind")]],content:choice};
      }
      this.report("Signing…"); await tiny.signing.publish(unsigned);
      this.report("Answer sent."); this.querySelectorAll("button,textarea").forEach(el=>el.disabled=true);
      await host.refresh(true);
    }
  }
  customElements.define("chat-activity",ChatActivity); customElements.define("chat-decision",ChatDecision);
})();
