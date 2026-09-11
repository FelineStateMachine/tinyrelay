// Signer bridge: session sign-in and sign-out, NIP-07 and NIP-46 signers,
// Nostr Connect and the NIP-98 signed fetch used by authenticated calls.
// It exposes tiny.localPath, tiny.authorization and tiny.signedFetch, and
// announces a connected signer with tiny:signer.
(() => {
  "use strict";

  const root = (location.pathname.match(/^\/r\/[^/]+/) || [""])[0];
  const localPath = path => path && path.startsWith("/") ? (path === root || path.startsWith(root + "/") ? path : root + path) : path;
  const storagePrefix = "tiny.bunker" + (root || "/");
  const storage = {
    uri: storagePrefix + ".uri",
    key: storagePrefix + ".sk",
    pubkey: storagePrefix + ".identity",
    remote: storagePrefix + ".remote"
  };
  const say = text => {
    const status = document.getElementById("signer-status") || document.getElementById("session-status");
    if (status) status.textContent = text;
  };
  const errorText = err => err instanceof Error ? err.message : String(err);
  const {hex, sha256} = window.tiny.util;
  const sha256hex = async bytes => hex(await sha256(bytes));
  const randomHex = () => { const b = new Uint8Array(32); crypto.getRandomValues(b); return hex(b); };
  const toBytes = body => body instanceof ArrayBuffer ? new Uint8Array(body) : body instanceof Uint8Array ? body : new TextEncoder().encode(body || "");
  const validRelay = value => {
    try { const parsed = new URL(value); return (parsed.protocol === "ws:" || parsed.protocol === "wss:") && parsed.hostname !== ""; } catch { return false; }
  };
  const validBunker = pointer => pointer && /^[0-9a-f]{64}$/i.test(pointer.pubkey || "") && Array.isArray(pointer.relays) && pointer.relays.length > 0 && pointer.relays.every(validRelay);

  // authorization builds a NIP-98 header for one request body. The body is
  // hashed exactly once and the same bytes must be sent.
  async function authorization(url, method, bytes) {
    if (!window.nostr?.signEvent) throw Error("Connect a Nostr signer first.");
    const event = await window.nostr.signEvent({
      kind: 27235,
      created_at: Math.floor(Date.now() / 1000),
      tags: [["u", url], ["method", method], ["payload", await sha256hex(bytes)], ["nonce", randomHex()]],
      content: ""
    });
    return "Nostr " + btoa(JSON.stringify(event));
  }

  async function signedFetch(path, method, body, options = {}) {
    path = localPath(path);
    const bytes = toBytes(body);
    const url = new URL(path, location.href).href;
    const request = {
      method,
      signal: options.signal,
      headers: {authorization: await authorization(url, method, bytes), "content-type": options.contentType || "application/octet-stream"}
    };
    if (method !== "GET" && method !== "HEAD") request.body = bytes;
    const response = await fetch(path, request);
    if (!response.ok) throw Error(await response.text());
    return response;
  }

  const signedSession = path => signedFetch(path, "POST", "");

  // Shell: theme toggle, the phone menu, and the installable app worker.
  const root_ = document.documentElement;
  const currentTheme = () => root_.dataset.theme || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  const savedTheme = localStorage.getItem("tiny.theme");
  if (savedTheme) root_.dataset.theme = savedTheme;
  // The installed app's title bar follows the theme-color meta, so keep it
  // in step with the rail colour of whichever theme is active.
  const paintChrome = () => {
    const rail = getComputedStyle(root_).getPropertyValue("--rail").trim();
    if (rail) document.querySelectorAll('meta[name="theme-color"]').forEach(meta => { meta.content = rail; });
    // The manifest is fetched at install and on later update checks, so
    // point it at the variant whose launch colours match the active theme.
    const manifest = document.querySelector('link[rel="manifest"]');
    if (manifest) manifest.href = localPath("/manifest.webmanifest") + (currentTheme() === "dark" ? "?theme=dark" : "");
  };
  paintChrome();
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", paintChrome);
  // Browsers that can install the app announce it; the footer then offers
  // a quiet link and nothing else changes until it is pressed.
  let installPrompt = null;
  const installButton = () => document.getElementById("install");
  window.addEventListener("beforeinstallprompt", event => {
    event.preventDefault();
    installPrompt = event;
    const button = installButton();
    if (button) button.hidden = false;
  });
  window.addEventListener("appinstalled", () => {
    installPrompt = null;
    const button = installButton();
    if (button) button.hidden = true;
  });
  const setMenu = open => {
    const menu = document.getElementById("nav-menu");
    if (menu) menu.open = open;
  };
  const shellBindings = new WeakSet();
  const bindShell = () => {
    const rail = document.getElementById("rail");
    const themeButton = document.getElementById("theme");
    if (rail && !shellBindings.has(rail)) {
      shellBindings.add(rail);
      rail.addEventListener("click", event => { if (event.target.closest("a")) setMenu(false); });
    }
    const install = installButton();
    if (install && !shellBindings.has(install)) {
      shellBindings.add(install);
      install.hidden = !installPrompt;
      install.addEventListener("click", async () => {
        if (!installPrompt) return;
        const prompt = installPrompt;
        installPrompt = null;
        install.hidden = true;
        try { await prompt.prompt(); } catch {}
      });
    }
    if (themeButton && !shellBindings.has(themeButton)) {
      shellBindings.add(themeButton);
      const label = () => { themeButton.textContent = currentTheme() === "dark" ? "light" : "dark"; };
      label();
      themeButton.addEventListener("click", () => {
        root_.dataset.theme = currentTheme() === "dark" ? "light" : "dark";
        localStorage.setItem("tiny.theme", root_.dataset.theme);
        label();
        paintChrome();
      });
    }
  };
  bindShell();
  document.addEventListener("keydown", event => {
    if (event.key !== "Escape" || !document.getElementById("nav-menu")?.open) return;
    setMenu(false);
    document.getElementById("menu")?.focus();
  });
  if ("serviceWorker" in navigator) navigator.serviceWorker.register(localPath("/sw.js")).catch(() => {});
  // An installed app asks to keep its cached shell and parked shares. Browsers
  // grant this silently for installed apps, so it never shows a prompt.
  if (matchMedia("(display-mode: standalone)").matches) navigator.storage?.persist?.().catch(() => {});
  Object.assign(window.tiny, {root, localPath, sha256hex, authorization, signedFetch});
  window.tinySignedFetch = signedFetch;

  const isSignIn = url => url.pathname.replace(/\/+$/, "") === localPath("/signin");
  const returnPath = value => {
    if (!value?.startsWith("/") || value.startsWith("//") || /[\\\u0000-\u001f\u007f]/.test(value)) return null;
    try {
      const url = new URL(value, location.origin);
      const tenant = (url.pathname.match(/^\/r\/[^/]+/) || [""])[0];
      if (url.origin !== location.origin || tenant !== root || isSignIn(url)) return null;
      return url.pathname + url.search + url.hash;
    } catch { return null; }
  };
  const signInDestination = () => {
    const current = new URL(location.href);
    if (!isSignIn(current)) return returnPath(current.pathname + current.search + current.hash) || localPath("/");
    const carried = new URLSearchParams(current.hash.slice(1)).get("return");
    const next = returnPath(carried) || returnPath(current.searchParams.get("next"));
    if (next) return next;
    try {
      const previous = new URL(document.referrer);
      if (previous.origin === location.origin) return returnPath(previous.pathname + previous.search) || localPath("/");
    } catch {}
    return localPath("/");
  };

  // Keep fragments client-side: file links may carry decryption keys. The
  // server-rendered next query is still a usable fallback without JavaScript.
  document.addEventListener("click", event => {
    const link = event.target?.closest?.("a[href]");
    if (!link) return;
    const current = new URL(location.href);
    let login;
    try { login = new URL(link.href, location.href); } catch { return; }
    if (login.origin !== location.origin || !isSignIn(login) || isSignIn(current)) return;
    const target = returnPath(current.pathname + current.search + current.hash);
    if (!target) return;
    login.searchParams.set("next", current.pathname + current.search);
    login.hash = current.hash ? "return=" + encodeURIComponent(target) : "";
    link.href = login.href;
    if (link.hasAttribute("fx-action")) link.setAttribute("fx-action", login.href);
  }, true);

  const signIn = async () => {
    const destination = signInDestination();
    say("Signing in…");
    await signedSession("/session");
    say("Signed in.");
    location.replace(destination);
  };

  // A remote signer session lives in this tab unless the person chose to
  // remember the device; then it survives the installed app being closed.
  const forgetRemote = () => {
    for (const store of [sessionStorage, localStorage]) {
      Object.values(storage).forEach(name => store.removeItem(name));
      store.removeItem("tiny.bunker");
    }
  };

  const rememberRemote = async (uri, key, signer) => {
    const store = document.getElementById("remember-signer")?.checked ? localStorage : sessionStorage;
    store.setItem(storage.uri, uri);
    store.setItem(storage.key, window.NostrSigner.bytesToHex(key));
    store.setItem(storage.pubkey, await signer.getPublicKey());
    store.setItem(storage.remote, JSON.stringify(signer.bp));
  };

  const setSigner = signer => {
    window.tinySigner = signer;
    window.nostr = {signEvent: event => window.tinySigner.signEvent(event)};
    if (typeof CustomEvent === "function") document.dispatchEvent?.(new CustomEvent("tiny:signer"));
    if (document.getElementById("signer-status")) {
      say(document.getElementById("session-logout") ? "Signer connected." : "Signer connected. Choose Sign in with connected signer to continue.");
    }
  };

  // Fixi can morph the content column without reloading this script. Bind
  // controls by element identity so navigation can safely discover new ones.
  const boundControls = new WeakSet();
  const bindControl = (id, event, handler) => {
    const element = document.getElementById(id);
    if (!element || boundControls.has(element)) return;
    boundControls.add(element);
    element.addEventListener(event, handler);
  };
  const busy = new WeakSet();
  const once = (element, action) => {
    if (busy.has(element)) return;
    busy.add(element);
    const button = element.matches?.("button, input[type=submit]") ? element : element.querySelector?.("button, input[type=submit]");
    if (button) button.disabled = true;
    return Promise.resolve().then(action).finally(() => { busy.delete(element); if (button) button.disabled = false; });
  };

  const bindControls = () => {
    bindControl("session-login", "click", event => once(event.currentTarget, async () => {
      try { await signIn(); } catch (err) { say("Sign-in error: " + errorText(err)); }
    }));

    bindControl("session-logout", "click", event => once(event.currentTarget, async () => {
      try {
        say("Signing out…");
        const response = await fetch(localPath("/session/logout"), {method: "POST", credentials: "same-origin"});
        if (!response.ok) throw Error(await response.text());
        forgetRemote();
        if (typeof CustomEvent === "function") document.dispatchEvent?.(new CustomEvent("tiny:logout"));
        say("Signed out.");
        location.reload();
      } catch (err) {
        say("Sign-out error: " + errorText(err));
      }
    }));

    // Nostr Connect: show a QR code and wait for a phone signer such as Amber.
    bindControl("nostrconnect", "click", event => {
      event.preventDefault();
      return once(event.currentTarget, async () => {
        try {
          const key = window.NostrSigner.generateSecretKey();
          const uri = window.NostrSigner.createNostrConnectURI({
            clientPubkey: window.NostrSigner.getPublicKey(key),
            relays: [location.origin.replace(/^http/,"ws")+root],
            secret: randomHex(),
            name: "tiny",
            url: location.origin,
            perms: ["sign_event:27235"]
          });
          const open = document.getElementById("nostrconnect-open");
          if (open) { open.href = uri; open.hidden = false; }
          const qr = document.getElementById("signer-qr");
          if (qr) { qr.src = root + "/qr.svg?text=" + encodeURIComponent(uri); qr.hidden = false; }
          navigator.clipboard?.writeText(uri).catch(() => {});
          say("Scan the QR code or open the link in your signer.");
          window.tinyConnect = {key, uri};
          const signer = await window.NostrSigner.BunkerSigner.fromURI(key, uri);
          setSigner(signer);
          await rememberRemote(uri, key, signer);
          await signIn();
        } catch (err) {
          say("Nostr Connect error: " + errorText(err));
        }
      });
    });

    // Bunker URL: connect to a remote signer the user pastes in.
    bindControl("bunker", "submit", event => {
      event.preventDefault();
      return once(event.currentTarget, async () => {
        const value = document.getElementById("bunker-url")?.value.trim();
        if (!value) return;
        try {
          const secret = window.NostrSigner.generateSecretKey();
          const pointer = await window.NostrSigner.parseBunkerInput(value);
          if (!validBunker(pointer)) throw Error("Enter a valid bunker URL with at least one relay.");
          const signer = window.NostrSigner.BunkerSigner.fromBunker(secret, pointer);
          await signer.connect();
          setSigner(signer);
          await rememberRemote(value, secret, signer);
          await signIn();
        } catch (err) {
          say("Remote signer error: " + errorText(err));
        }
      });
    });
  };
  bindControls();
  document.addEventListener("tiny:navigation", () => { bindShell(); bindControls(); });

  // Resume a remote signer stored for this tab so page loads keep working.
  (async () => {
    const store = sessionStorage.getItem(storage.key) ? sessionStorage : localStorage;
    const uri = store.getItem(storage.uri);
    const keyHex = store.getItem(storage.key);
    const expected = store.getItem(storage.pubkey);
    const remote = store.getItem(storage.remote);
    if (!uri || !keyHex) return;
    try {
      const key = window.NostrSigner.hexToBytes(keyHex);
      const pointer = remote ? JSON.parse(remote) : await window.NostrSigner.parseBunkerInput(uri);
      const signer = window.NostrSigner.BunkerSigner.fromBunker(key, pointer);
      await signer.connect();
      const pubkey = await signer.getPublicKey();
      if (expected && pubkey !== expected) throw Error("Remote signer identity changed");
      setSigner(signer);
    } catch (err) {
      forgetRemote();
      say("Remote signer resume error: " + errorText(err));
    }
  })();
})();
