// Signer bridge: session sign-in and sign-out, NIP-07 and NIP-46 signers,
// Nostr Connect, and the NIP-98 signed fetch that every management call uses.
// The browser never receives a relay private key; it only asks the signer.
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
  const status = document.getElementById("signer-status") || document.getElementById("session-status");
  const say = text => { if (status) status.textContent = text; };
  const hex = bytes => Array.from(bytes, b => b.toString(16).padStart(2, "0")).join("");
  const sha256hex = async bytes => hex(new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)));
  const randomHex = () => { const b = new Uint8Array(32); crypto.getRandomValues(b); return hex(b); };
  const toBytes = body => body instanceof ArrayBuffer ? new Uint8Array(body) : body instanceof Uint8Array ? body : new TextEncoder().encode(body || "");

  // authorization builds a NIP-98 header for one request body. The body is
  // hashed exactly once and the same bytes must be sent.
  async function authorization(url, method, bytes) {
    if (!window.nostr?.signEvent) throw Error("Connect a Nostr signer first.");
    const event = await window.nostr.signEvent({
      kind: 27235,
      created_at: Math.floor(Date.now() / 1000),
      tags: [["u", url], ["method", method], ["payload", await sha256hex(bytes)]],
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
  window.tiny = {root, localPath, sha256hex, authorization, signedFetch};
  window.tinySignedFetch = signedFetch;

  const signIn = async () => {
    say("Signing in…");
    await signedSession("/session");
    say("Signed in.");
    location.assign(localPath("/"));
  };

  const forgetRemote = () => {
    Object.values(storage).forEach(name => sessionStorage.removeItem(name));
    sessionStorage.removeItem("tiny.bunker");
  };

  const rememberRemote = async (uri, key, signer) => {
    sessionStorage.setItem(storage.uri, uri);
    sessionStorage.setItem(storage.key, window.NostrSigner.bytesToHex(key));
    sessionStorage.setItem(storage.pubkey, await signer.getPublicKey());
    sessionStorage.setItem(storage.remote, JSON.stringify(signer.bp));
  };

  const setSigner = signer => {
    window.tinySigner = signer;
    window.nostr = {signEvent: event => window.tinySigner.signEvent(event)};
    if (document.getElementById("signer-status")) {
      say(document.getElementById("session-logout") ? "Signer connected." : "Signer connected. Choose Sign in with connected signer to continue.");
    }
  };

  document.getElementById("session-login")?.addEventListener("click", async () => {
    try { await signIn(); } catch (err) { say("Sign-in error: " + err.message); }
  });

  document.getElementById("session-logout")?.addEventListener("click", async () => {
    try {
      say("Signing out…");
      const response = await fetch(localPath("/session/logout"), {method: "POST", credentials: "same-origin"});
      if (!response.ok) throw Error(await response.text());
      forgetRemote();
      say("Signed out.");
      location.reload();
    } catch (err) {
      say("Sign-out error: " + err.message);
    }
  });

  // Nostr Connect: show a QR code and wait for a phone signer such as Amber.
  document.getElementById("nostrconnect")?.addEventListener("click", async ev => {
    ev.preventDefault();
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
    try {
      const signer = await window.NostrSigner.BunkerSigner.fromURI(key, uri);
      setSigner(signer);
      await rememberRemote(uri, key, signer);
      await signIn();
    } catch (err) {
      say("Nostr Connect error: " + err.message);
    }
  });

  // Bunker URL: connect to a remote signer the user pastes in.
  document.getElementById("bunker")?.addEventListener("submit", async ev => {
    ev.preventDefault();
    const value = document.getElementById("bunker-url").value.trim();
    if (!value) return;
    try {
      const secret = window.NostrSigner.generateSecretKey();
      const pointer = await window.NostrSigner.parseBunkerInput(value);
      const signer = window.NostrSigner.BunkerSigner.fromBunker(secret, pointer);
      await signer.connect();
      setSigner(signer);
      await rememberRemote(value, secret, signer);
      await signIn();
    } catch (err) {
      say("Remote signer error: " + err.message);
    }
  });

  // Resume a remote signer stored for this tab so page loads keep working.
  (async () => {
    const uri = sessionStorage.getItem(storage.uri);
    const keyHex = sessionStorage.getItem(storage.key);
    const expected = sessionStorage.getItem(storage.pubkey);
    const remote = sessionStorage.getItem(storage.remote);
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
      say("Remote signer resume error: " + err.message);
    }
  })();
})();
