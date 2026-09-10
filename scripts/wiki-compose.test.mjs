import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, getPublicKey, verifyEvent } from "nostr-tools";
import { sharedSigning } from "./test-signing.mjs";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const tinySource = fs.readFileSync("internal/webui/tiny.js", "utf8");
const wikiSource = source.slice(source.indexOf("  const publishSigned = "), source.indexOf("  // ApprovalItem reacts"));
const isHex64 = value => /^[0-9a-f]{64}$/.test(value || "");

const util = (() => {
  const sandbox = {TextEncoder, TextDecoder, URL, crypto: {subtle: {}}, document: {}};
  sandbox.globalThis = sandbox;
  vm.runInNewContext(tinySource, sandbox);
  return sandbox.tiny.util;
})();

test("wikiName follows the relay's NIP-54 normalization", () => {
  for (const [title, name] of [
    ["What's Up?", "whats-up"],
    ["日本語 Article", "日本語-article"],
    ["Москва", "москва"],
    ["  --Hello__World--  ", "hello-world"],
    ["Release notes 1.4", "release-notes-14"],
    ["Files & Private Repositories", "files-private-repositories"],
    ["café au lait", "café-au-lait"],
    ["***", ""],
    ["", ""]
  ]) {
    assert.equal(util.wikiName(title), name, title);
  }
  assert.equal(util.wikiName(undefined), "");
});

// setup runs one element class from components.js against fakes for the
// signer, the relay and the form, the way collaboration-compose.test does.
function setup(name, attributes, values = {}, options = {}) {
  const secret = generateSecretKey(), pubkey = getPublicKey(secret);
  const sent = [], signed = [];
  class FormElement {
    getAttribute(key) { return attributes[key] ?? null; }
    report(message) { this.message = message; }
  }
  const window = {NostrSigner: {verifyEvent}, nostr: {
    getPublicKey: async () => pubkey,
    signEvent: async event => {
      signed.push(structuredClone(event));
      if (options.change) event.content = "changed";
      return finalizeEvent(structuredClone(event), secret);
    }
  }};
  const tiny = {util, navigated: [], signedFetch: async (path, method, body) => {
    sent.push({path, method, event: JSON.parse(body)});
    return Response.json(options.results?.[sent.length - 1] ?? options.result ?? {accepted: true});
  }, navigate: async href => { tiny.navigated.push(href); }};
  tiny.signing = sharedSigning(window, tiny);
  const Constructor = vm.runInNewContext(`${wikiSource}\n${name}`, {FormElement, window, tiny, URL, isHex64, Date, JSON, encodeURIComponent});
  const component = new Constructor();
  component.submitter = options.submitter ? {value: options.submitter, textContent: options.label || ""} : null;
  const form = {elements: Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value}]))};
  return {component, form, sent, signed, tiny, pubkey};
}

const author = "a".repeat(64), base = "b".repeat(64), mergeID = "c".repeat(64);
const coordinate = `30818:${author}:release-notes-1-4`;
const page = {name: "release-notes-1-4", author, coordinate, event: base};

test("a new page signs d, title and summary and opens the page", async () => {
  const s = setup("WikiCompose", {name: "", author: "", coordinate: "", event: ""}, {title: "What's Up?", name: "", summary: "A greeting", content: "# Hi\n\nHello."});
  await s.component.submit(s.form);
  assert.equal(s.sent.length, 1);
  const event = s.sent[0].event;
  assert.equal(s.sent[0].path, "/events");
  assert.equal(event.kind, 30818);
  assert.deepEqual(event.tags, [["d", "whats-up"], ["title", "What's Up?"], ["summary", "A greeting"]]);
  assert.equal(event.content, "# Hi\n\nHello.");
  assert.equal(verifyEvent(event), true);
  assert.equal(s.component.message, "Published.");
  assert.deepEqual(s.tiny.navigated, ["/wiki/whats-up"]);
});

test("the name field wins over the title and is normalized", async () => {
  const s = setup("WikiCompose", {name: "", author: "", coordinate: "", event: ""}, {title: "Bitcoin", name: "  BTC Coin ", content: "Money."});
  await s.component.submit(s.form);
  assert.deepEqual(s.sent[0].event.tags, [["d", "btc-coin"], ["title", "Bitcoin"]]);
});

test("another signer publishes a fork of the version in view", async () => {
  const s = setup("WikiCompose", page, {title: "Release notes 1.4", name: "release-notes-1-4", summary: "", content: "Rewritten."});
  await s.component.submit(s.form);
  assert.equal(s.sent.length, 1);
  assert.deepEqual(s.sent[0].event.tags, [["d", "release-notes-1-4"], ["title", "Release notes 1.4"], ["a", coordinate, "", "fork"], ["e", base, "", "fork"]]);
  assert.notEqual(s.sent[0].event.pubkey, author);
});

test("propose publishes the fork and then a merge request to the author", async () => {
  const s = setup("WikiCompose", page, {title: "Release notes 1.4", name: "release-notes-1-4", summary: "Fix the upload section", content: "Rewritten."}, {submitter: "propose"});
  await s.component.submit(s.form);
  assert.equal(s.sent.length, 2);
  const [version, request] = s.sent.map(item => item.event);
  assert.equal(version.kind, 30818);
  assert.equal(request.kind, 818);
  assert.deepEqual(request.tags, [["a", coordinate], ["e", version.id, "", "source"], ["e", base], ["p", author]]);
  assert.equal(request.content, "Fix the upload section");
  assert.equal(verifyEvent(request), true);
  assert.match(s.component.message, /proposed to aaaaaaaaaaaa/);
  assert.deepEqual(s.tiny.navigated, ["/wiki/release-notes-1-4"]);
});

test("the author's own edit carries no fork tags and cannot be proposed", async () => {
  const s = setup("WikiCompose", page, {title: "Release notes 1.4", name: "release-notes-1-4", content: "Edited."});
  s.component.getAttribute = key => ({...page, author: s.pubkey, coordinate: `30818:${s.pubkey}:release-notes-1-4`})[key] ?? null;
  await s.component.submit(s.form);
  assert.deepEqual(s.sent[0].event.tags, [["d", "release-notes-1-4"], ["title", "Release notes 1.4"]]);
  const own = setup("WikiCompose", page, {title: "Release notes 1.4", name: "release-notes-1-4", content: "Edited."}, {submitter: "propose"});
  own.component.getAttribute = key => ({...page, author: own.pubkey})[key] ?? null;
  await assert.rejects(own.component.submit(own.form), /your own version/);
  assert.equal(own.sent.length, 0);
});

test("missing fields, a changed signature and a relay refusal stop before or after one event", async () => {
  for (const [values, message] of [[{title: "", name: "", content: "x"}, /title/], [{title: "***", name: "", content: "x"}, /letter or digit/], [{title: "T", name: "", content: "  "}, /content/]]) {
    const s = setup("WikiCompose", {name: "", author: "", coordinate: "", event: ""}, values);
    await assert.rejects(s.component.submit(s.form), message);
    assert.equal(s.signed.length, 0);
  }
  const changed = setup("WikiCompose", page, {title: "T", name: "t", content: "x"}, {change: true});
  await assert.rejects(changed.component.submit(changed.form), /changed/);
  assert.equal(changed.sent.length, 0);
  const refused = setup("WikiCompose", page, {title: "T", name: "t", content: "x"}, {submitter: "propose", results: [{accepted: false, message: "blocked"}]});
  await assert.rejects(refused.component.submit(refused.form), /blocked/);
  assert.equal(refused.sent.length, 1);
  assert.deepEqual(refused.tiny.navigated, []);
});

test("nostr-react signs the reaction the pressed button carries", async () => {
  for (const [reaction, message] of [["+", "Accepted."], ["-", "Rejected."]]) {
    const s = setup("NostrReact", {event: mergeID, pubkey: author, kind: "818"}, {}, {submitter: reaction, label: reaction === "+" ? "Accept" : "Reject"});
    await s.component.submit(s.form);
    const event = s.sent[0].event;
    assert.equal(event.kind, 7);
    assert.equal(event.content, reaction);
    assert.deepEqual(event.tags, [["e", mergeID, "", author], ["p", author], ["k", "818"]]);
    assert.equal(verifyEvent(event), true);
    assert.equal(s.component.message, message);
  }
  const plain = setup("NostrReact", {event: mergeID, pubkey: author}, {}, {submitter: "+"});
  await plain.component.submit(plain.form);
  assert.deepEqual(plain.sent[0].event.tags, [["e", mergeID, "", author], ["p", author]]);
});

test("nostr-react refuses unknown reactions and bad addresses before signing", async () => {
  for (const [attributes, submitter, message] of [
    [{event: mergeID, pubkey: author}, "meh", /Approve or Deny/],
    [{event: "short", pubkey: author}, "+", /missing/],
    [{event: mergeID, pubkey: ""}, "-", /missing/]
  ]) {
    const s = setup("NostrReact", attributes, {}, {submitter});
    await assert.rejects(s.component.submit(s.form), message);
    assert.equal(s.signed.length, 0);
    assert.equal(s.sent.length, 0);
  }
});
