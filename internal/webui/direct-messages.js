/* NIP-17 direct messages: kind 14 rumors, NIP-59 seals and gift wraps. */
(() => {
  "use strict";

  // Leave room for the rumor JSON envelope under NIP-44's 65535-byte limit.
  const MAX_CONTENT = 60 * 1024;
  const MAX_TAGS = 32;
  const contentBytes = value => typeof TextEncoder === "function" ? new TextEncoder().encode(value).length : unescape(encodeURIComponent(value)).length;
  const hex64 = value => typeof value === "string" && /^[0-9a-f]{64}$/i.test(value);
  const signerAPI = () => {
    const value = globalThis.NostrSigner;
    if (!value?.finalizeEvent || !value.generateSecretKey || !value.verifyEvent || !value.getEventHash)
      throw Error("Nostr signing support is unavailable");
    return value;
  };
  const signerPublicKey = async signer => {
    const key = await signer.getPublicKey();
    if (!hex64(key)) throw Error("invalid signer public key");
    return key.toLowerCase();
  };
  const normalizePubkey = value => {
    if (globalThis.tiny?.files?.messages?.normalizePubkey)
      return globalThis.tiny.files.messages.normalizePubkey(value);
    if (hex64(value)) return value.toLowerCase();
    const decoded = signerAPI().decodeNpub?.(String(value || ""));
    if (hex64(decoded)) return decoded.toLowerCase();
    throw Error("public key must be a 64 character hex key or npub");
  };
  const nip44Encrypt = async (signer, pubkey, text) => {
    if (typeof globalThis.tiny?.nip44Encrypt === "function") return globalThis.tiny.nip44Encrypt(pubkey, text, signer);
    if (typeof signer.nip44Encrypt === "function") return signer.nip44Encrypt(pubkey, text);
    throw Error("NIP-44 encryption is unavailable");
  };
  const nip44Decrypt = async (signer, pubkey, text) => {
    if (typeof globalThis.tiny?.nip44Decrypt === "function") return globalThis.tiny.nip44Decrypt(pubkey, text, signer);
    if (typeof signer.nip44Decrypt === "function") return signer.nip44Decrypt(pubkey, text);
    throw Error("NIP-44 decryption is unavailable");
  };
  const eventHash = (api, event) => api.getEventHash(event);
  const past = now => Math.max(0, Math.floor(now) - Math.floor(Math.random() * 172801));

  async function build(input = {}, options = {}) {
    const signer = options.signer || globalThis.tiny?.signer?.();
    if (!signer?.signEvent || !signer.getPublicKey) throw Error("a NIP-44 signer is required");
    const recipient = normalizePubkey(input.recipient);
    const sender = await signerPublicKey(signer);
    const content = String(input.content ?? "");
    if (contentBytes(content) > MAX_CONTENT) throw Error("message content is empty or too large");
    const tags = [["p", recipient]];
    if (input.reply) {
      if (!hex64(input.reply.id) || !hex64(input.reply.pubkey)) throw Error("reply must include valid id and pubkey");
      tags.push(["e", input.reply.id.toLowerCase(), input.reply.relay || "", "reply", input.reply.pubkey.toLowerCase()]);
    }
    const now = options.now ?? Date.now() / 1000;
    let kind = 14;
    if (input.reaction) {
      const target = input.reaction;
      if (!hex64(target.id) || !hex64(target.pubkey) || (target.kind != null && (!Number.isSafeInteger(target.kind) || target.kind < 0)))
        throw Error("reaction target must include valid id and pubkey");
      kind = 7;
      tags.push(["e", target.id.toLowerCase(), target.relay || "", "root", target.pubkey.toLowerCase()]);
      tags.push(["p", target.pubkey.toLowerCase()]);
      if (target.kind != null) tags.push(["k", String(target.kind)]);
    }
    const rumorContent = input.reaction ? String(input.reaction.content ?? content) : content;
    if (!rumorContent || contentBytes(rumorContent) > MAX_CONTENT) throw Error("message content is empty or too large");
    const rumor = {pubkey: sender, created_at: Math.floor(now), kind, tags, content: rumorContent};
    rumor.id = eventHash(signerAPI(), rumor);
    // Keep both encryption layers within the original NIP-44 payload limit.
    if (contentBytes(JSON.stringify(rumor)) > 40 * 1024) throw Error("Message is too long to send. Split it into shorter messages.");
    const sealFor = async target => {
      const encrypted = await nip44Encrypt(signer, target, JSON.stringify(rumor));
      const sealInput = {kind: 13, created_at: past(now), tags: [], content: encrypted};
      const seal = await signer.signEvent(sealInput);
      if (!seal || seal.kind !== sealInput.kind || seal.tags.length || seal.content !== encrypted || seal.created_at !== sealInput.created_at || seal.pubkey !== sender || !signerAPI().verifyEvent(seal)) throw Error("invalid NIP-59 seal");
      return seal;
    };
    const wrap = async (target, seal) => {
      const api = signerAPI();
      const secret = api.generateSecretKey();
      const ephemeral = api.getPublicKey(secret);
      const key = api.nip44.getConversationKey(secret, target);
      const encrypted = api.nip44.encrypt(JSON.stringify(seal), key);
      const event = api.finalizeEvent({kind: 1059, created_at: past(now), tags: [["p", target]], content: encrypted}, secret);
      if (!api.verifyEvent(event)) throw Error("invalid NIP-59 gift wrap");
      return {recipient: target, event};
    };
    const [receiverSeal, senderSeal] = await Promise.all([sealFor(recipient), sealFor(sender)]);
    return {rumor, events: [await wrap(recipient, receiverSeal), await wrap(sender, senderSeal)]};
  }

  async function decrypt(wrap, options = {}) {
    const signer = options.signer || globalThis.tiny?.signer?.();
    if (!wrap || wrap.kind !== 1059 || !hex64(wrap.pubkey) || !Array.isArray(wrap.tags) || wrap.tags.some(tag => !Array.isArray(tag) || tag.some(value => typeof value !== "string")))
      throw Error("malformed gift wrap");
    const actor = await signerPublicKey(signer);
    const recipients = wrap.tags.filter(tag => tag[0] === "p");
    if (recipients.length !== 1 || !hex64(recipients[0][1])) throw Error("malformed gift wrap recipient");
    const recipient = normalizePubkey(recipients[0][1]);
    if (recipient !== actor || (options.actor && normalizePubkey(options.actor) !== actor)) throw Error("gift wrap is not addressed to this signer");
    if (typeof wrap.content !== "string" || wrap.content.length > 256 * 1024) throw Error("gift wrap is too large");
    if (!signerAPI().verifyEvent(wrap) || wrap.id !== eventHash(signerAPI(), {...wrap, id: undefined})) throw Error("invalid gift wrap signature");
    let seal;
    try { seal = JSON.parse(await nip44Decrypt(signer, wrap.pubkey, wrap.content)); } catch { throw Error("unable to decrypt gift wrap"); }
    if (!seal || seal.kind !== 13 || !hex64(seal.pubkey) || !Array.isArray(seal.tags) || seal.tags.length > MAX_TAGS || seal.tags.some(tag => !Array.isArray(tag) || tag.length < 1 || tag.length > 32 || tag.some(value => typeof value !== "string")) || typeof seal.content !== "string" || seal.content.length > 256 * 1024 || !signerAPI().verifyEvent(seal)) throw Error("malformed seal");
    const expirations = seal.tags.filter(tag => tag[0] === "expiration");
    if (expirations.some(tag => tag.length !== 2 || !/^\d+$/.test(tag[1]) || Number(tag[1]) < Math.floor(Date.now() / 1000))) throw Error("gift wrap has expired");
    let rumor;
    try { rumor = JSON.parse(await nip44Decrypt(signer, seal.pubkey, seal.content)); } catch { throw Error("unable to decrypt seal"); }
    if (!rumor || ![5, 7, 14, 15].includes(rumor.kind) || !hex64(rumor.pubkey) || Object.prototype.hasOwnProperty.call(rumor, "sig") || !Number.isSafeInteger(rumor.created_at) || rumor.created_at < 0 || rumor.created_at > 8640000000000 || typeof rumor.content !== "string" || contentBytes(rumor.content) > MAX_CONTENT || !Array.isArray(rumor.tags) || rumor.tags.length > MAX_TAGS || rumor.tags.reduce((total, tag) => total + JSON.stringify(tag).length, 0) > 16 * 1024 || rumor.tags.some(tag => !Array.isArray(tag) || tag.length < 1 || tag.length > 32 || tag.some(value => typeof value !== "string")) || rumor.id !== eventHash(signerAPI(), {...rumor, id: undefined}) || seal.pubkey !== rumor.pubkey)
      throw Error("malformed direct message rumor");
    const addressed = rumor.tags.filter(tag => Array.isArray(tag) && tag[0] === "p" && hex64(tag[1])).map(tag => tag[1].toLowerCase());
    const selfCopy = rumor.pubkey === actor;
    const interactionTarget = [5, 7].includes(rumor.kind) && rumor.tags.some(tag => (tag[0] === "e" || tag[0] === "q") && hex64(tag[1]));
    if (![5, 7].includes(rumor.kind) && (!addressed.includes(actor) && !selfCopy || options.actor && !addressed.includes(normalizePubkey(options.actor)) && !selfCopy)) throw Error("rumor recipient mismatch");
    if ([5, 7].includes(rumor.kind) && !interactionTarget) throw Error("interaction target is missing");
    return {rumor, seal, wrap, sender: rumor.pubkey};
  }

  const files = () => globalThis.tiny?.files?.messages;
  const protocol = Object.freeze({
    normalizePubkey,
    build,
    decrypt,
    relayList: (...args) => files()?.relayList(...args),
    queryRelayList: (...args) => files()?.queryRelayList(...args),
    publishEvent: (...args) => files()?.publishEvent(...args)
  });
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.chat = {...globalThis.tiny.chat, protocol};
})();
