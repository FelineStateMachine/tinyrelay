/* NIP-17 file messages. This module never publishes to ordinary relays. */
(() => {
  "use strict";
  const {relayURL} = globalThis.tiny.util;
  const {signer: activeSigner, nip44Encrypt} = globalThis.tiny;

  const MAX_RELAYS = 8;
  const DAY = 86400;

  function api() {
    const signer = globalThis.NostrSigner;
    if (!signer || !signer.nip44 || !signer.finalizeEvent || !signer.generateSecretKey) {
      throw Error("NIP-44 support is unavailable");
    }
    return signer;
  }

  function relayList(event, pubkey) {
    if (Array.isArray(event)) {
      event = event
        .filter(candidate => candidate?.kind === 10050 && candidate.pubkey === pubkey)
        .sort((a, b) => b.created_at - a.created_at)[0];
    }
    if (!event || event.kind !== 10050 || event.pubkey !== pubkey) {
      throw Error("recipient has no valid kind 10050 relay list");
    }
    const signer = api();
    if (!signer.verifyEvent(event)) throw Error("invalid kind 10050 relay list signature");
    const relays = [
      ...new Set(
        event.tags
          .filter(tag => tag[0] === "relay")
          .map(tag => relayURL(tag[1]))
          .filter(Boolean)
      )
    ];
    if (!relays.length) throw Error("kind 10050 relay list is empty");
    return relays.slice(0, MAX_RELAYS);
  }

  function normalizePubkey(value) {
    const text = String(value || "").trim();
    if (/^[0-9a-f]{64}$/i.test(text)) return text.toLowerCase();
    const decoded = api().decodeNpub?.(text);
    if (typeof decoded === "string" && /^[0-9a-f]{64}$/i.test(decoded)) return decoded.toLowerCase();
    throw Error("public key must be a 64 character hex key or npub");
  }

  async function bounded(value, timeout, label) {
    const ms = Number.isFinite(timeout) && timeout > 0 ? timeout : 10000;
    let timer;
    try {
      return await Promise.race([
        Promise.resolve(value),
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(Error(label + " timed out")), ms);
        })
      ]);
    } finally {
      clearTimeout(timer);
    }
  }

  function sent(result) {
    if (result === false || result == null) return false;
    if (Array.isArray(result)) return result.some(sent);
    if (typeof result === "object" && "ok" in result) return result.ok === true;
    return true;
  }

  function past(now) {
    return Math.max(0, Math.floor(now) - Math.floor(Math.random() * (2 * DAY + 1)));
  }

  async function publicKey(signer, getPublicKey) {
    const key = getPublicKey ? await getPublicKey() : await signer.getPublicKey();
    if (typeof key !== "string" || !/^[0-9a-f]{64}$/i.test(key)) throw Error("invalid signer public key");
    return key.toLowerCase();
  }

  async function queryRelayList(pubkey, options) {
    if (!globalThis.tiny?.signedFetch) throw Error("signed local query is unavailable");
    const response = await globalThis.tiny.signedFetch(
      "/query",
      "POST",
      JSON.stringify([{kinds: [10050], authors: [pubkey], limit: 1}]),
      {contentType: "application/json"}
    );
    return response.json();
  }

  function publishEvent(event, relays, options) {
    const signer = options.signer || activeSigner();
    const WebSocketImpl = options.WebSocket || globalThis.WebSocket;
    if (typeof WebSocketImpl !== "function") throw Error("WebSocket support is unavailable");
    const timeout = Number.isFinite(options.timeoutMs) && options.timeoutMs > 0 ? options.timeoutMs : 10000;
    return Promise.all(
      relays.map(
        url =>
          new Promise(resolve => {
            let socket;
            let timer;
            let settled = false;
            let authID = "";
            let authAttempts = 0;
            let eventSent = false;
            const finish = ok => {
              if (settled) return;
              settled = true;
              clearTimeout(timer);
              try {
                socket?.close();
              } catch {}
              resolve(ok);
            };
            const sendEvent = () => {
              if (!settled && !eventSent) {
                eventSent = true;
                socket.send(JSON.stringify(["EVENT", event]));
              }
            };
            try {
              socket = new WebSocketImpl(url);
              timer = setTimeout(() => finish(false), timeout);
              socket.onopen = sendEvent;
              socket.onmessage = async message => {
                let frame;
                try {
                  frame = JSON.parse(typeof message.data === "string" ? message.data : "");
                } catch {
                  return;
                }
                if (settled || !Array.isArray(frame)) return;
                if (frame[0] === "OK" && frame[1] === authID) {
                  if (frame[2] === true) {
                    eventSent = false;
                    sendEvent();
                  } else finish(false);
                  return;
                }
                if (frame[0] === "OK" && frame[1] === event.id) {
                  if (
                    frame[2] !== true &&
                    String(frame[3] || "")
                      .toLowerCase()
                      .startsWith("auth-required")
                  ) {
                    eventSent = false;
                    return;
                  }
                  finish(frame[2] === true);
                  return;
                }
                if (frame[0] !== "AUTH" || typeof signer?.signEvent !== "function") return;
                if (typeof frame[1] !== "string" || frame[1].length > 4096) {
                  finish(false);
                  return;
                }
                if (authAttempts++ >= 1) {
                  finish(false);
                  return;
                }
                try {
                  const auth = await signer.signEvent({
                    kind: 22242,
                    created_at: Math.floor(Date.now() / 1000),
                    tags: [
                      ["relay", url],
                      ["challenge", frame[1]],
                      ["method", "AUTH"]
                    ],
                    content: ""
                  });
                  if (
                    auth &&
                    !settled &&
                    auth.kind === 22242 &&
                    auth.content === "" &&
                    auth.tags.length === 3 &&
                    auth.tags[0][0] === "relay" &&
                    auth.tags[0][1] === url &&
                    auth.tags[1][0] === "challenge" &&
                    auth.tags[1][1] === frame[1] &&
                    auth.tags[2][0] === "method" &&
                    auth.tags[2][1] === "AUTH" &&
                    api().verifyEvent(auth) &&
                    auth.pubkey === (await publicKey(signer))
                  ) {
                    authID = auth.id;
                    socket.send(JSON.stringify(["AUTH", auth]));
                  } else finish(false);
                } catch {
                  finish(false);
                }
              };
              socket.onerror = () => finish(false);
              socket.onclose = () => finish(false);
            } catch {
              finish(false);
            }
          })
      )
    );
  }

  function unsignedRumor(input, sender, now) {
    if (!input.fileURL || !/^https?:\/\//i.test(input.fileURL)) throw Error("fileURL must be an HTTP(S) URL");
    if (!/^[0-9a-f]{64}$/i.test(input.ciphertextHash || "")) throw Error("ciphertextHash must be SHA-256 hex");
    if (!input.key || !input.nonce) throw Error("encrypted file messages require a decryption key and nonce");
    const tags = [["p", input.recipient]];
    if (input.mimeType) tags.push(["file-type", input.mimeType]);
    tags.push(["encryption-algorithm", "aes-gcm"]);
    if (input.key) tags.push(["decryption-key", input.key]);
    if (input.nonce) tags.push(["decryption-nonce", input.nonce]);
    tags.push(["x", input.ciphertextHash.toLowerCase()]);
    if (input.plaintextHash) {
      if (!/^[0-9a-f]{64}$/i.test(input.plaintextHash)) throw Error("plaintextHash must be SHA-256 hex");
      tags.push(["ox", input.plaintextHash.toLowerCase()]);
    }
    if (input.size != null) tags.push(["size", String(input.size)]);
    const rumor = {
      pubkey: sender,
      created_at: Math.floor(now),
      kind: 15,
      tags,
      content: input.fileURL
    };
    rumor.id = api().getEventHash(rumor);
    return rumor;
  }

  const encryptFor = (signer, recipient, plaintext) => nip44Encrypt(recipient, plaintext, signer);

  async function seal(rumor, recipient, signer, now) {
    const content = await encryptFor(signer, recipient, JSON.stringify(rumor));
    const unsigned = {
      kind: 13,
      tags: [],
      content,
      created_at: past(now),
      pubkey: rumor.pubkey
    };
    const event = await signer.signEvent(unsigned);
    if (
      !event ||
      event.kind !== 13 ||
      event.pubkey !== rumor.pubkey ||
      event.tags.length ||
      event.content !== unsigned.content ||
      event.created_at !== unsigned.created_at
    )
      throw Error("signer returned an invalid NIP-17 seal");
    if (!api().verifyEvent(event)) throw Error("signer returned an invalid seal signature");
    return event;
  }

  function wrap(sealEvent, recipient, now) {
    const signer = api();
    const secret = signer.generateSecretKey();
    const pubkey = signer.getPublicKey(secret);
    const key = signer.nip44.getConversationKey(secret, recipient);
    const content = signer.nip44.encrypt(JSON.stringify(sealEvent), key);
    const event = signer.finalizeEvent({kind: 1059, tags: [["p", recipient]], content, created_at: past(now)}, secret);
    if (!signer.verifyEvent(event)) throw Error("failed to verify gift wrap");
    return {event, secret, pubkey};
  }

  async function build(input, options = {}) {
    const signer = options.signer || activeSigner();
    if (
      !signer ||
      typeof signer.signEvent !== "function" ||
      (typeof signer.nip44Encrypt !== "function" && !signer.nip44?.encrypt)
    )
      throw Error("a NIP-44 signer is required");
    const recipient = normalizePubkey(input.recipient);
    const sender = await publicKey(signer, options.getPublicKey);
    const now = options.now || Date.now() / 1000;
    const rumor = unsignedRumor({...input, recipient}, sender, now);
    const receiverSeal = await seal(rumor, recipient, signer, now);
    const senderSeal = await seal(rumor, sender, signer, now);
    const receiver = wrap(receiverSeal, recipient, now).event;
    const own = wrap(senderSeal, sender, now).event;
    return {
      rumor,
      events: [
        {recipient, event: receiver},
        {recipient: sender, event: own}
      ]
    };
  }

  async function share(input, options = {}) {
    const signer = options.signer || activeSigner();
    const query = options.queryRelayList || (pubkey => queryRelayList(pubkey, options));
    const publish = options.publish || ((event, relays) => publishEvent(event, relays, options));
    if (typeof query !== "function" || typeof publish !== "function")
      throw Error("private NIP-17 delivery is unavailable");
    const sender = await publicKey(signer, options.getPublicKey);
    const recipient = normalizePubkey(input.recipient);
    const timeout = options.timeoutMs;
    const receiverList = relayList(await bounded(query(recipient), timeout, "recipient relay lookup"), recipient);
    const senderList = relayList(await bounded(query(sender), timeout, "sender relay lookup"), sender);
    const built = await build(input, options);
    if (built.rumor.pubkey !== sender) throw Error("signer changed during file sharing; try again");
    const deliveries = [];
    for (const target of built.events) {
      const relays = target.recipient === recipient ? receiverList : senderList;
      const delivery = await bounded(publish(target.event, relays), timeout, "NIP-17 delivery");
      if (!sent(delivery)) throw Error("NIP-17 delivery failed on every inbox relay");
      deliveries.push(delivery);
    }
    return {
      ...built,
      relays: {recipient: receiverList, sender: senderList},
      deliveries
    };
  }

  const apiObject = Object.freeze({
    build,
    share,
    relayList,
    queryRelayList,
    publishEvent
  });
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.files = {...globalThis.tiny.files, messages: apiObject};
})();
