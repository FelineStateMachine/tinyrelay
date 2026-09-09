import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { finalizeEvent, generateSecretKey, verifyEvent } from "nostr-tools";

const root = "a".repeat(64), author = "b".repeat(64), parent = "c".repeat(64), parentAuthor = "d".repeat(64);
const coordinate = `30617:${author}:test`;
const source = fs.readFileSync("internal/webui/components.js", "utf8");
const composeSource = source.slice(source.indexOf("  class NostrCompose "), source.indexOf("  class NostrKey "));
function setup(attributes, values = {}, options = {}) {
  const sent = [], signed = [];
  let resets = 0;
  class FormElement {
    getAttribute(name) { return attributes[name] ?? null; }
    report(message) { this.message = message; }
  }
  const window = {NostrSigner: {verifyEvent}, nostr: {signEvent: async event => {
    signed.push(structuredClone(event));
    if (options.change) event.content = "changed";
    return finalizeEvent(structuredClone(event), generateSecretKey());
  }}};
  const tiny = {signedFetch: async (path, method, body) => {
    sent.push({path, method, event: JSON.parse(body)});
    return Response.json(options.result ?? {accepted: true});
  }};
  const Constructor = vm.runInNewContext(`${composeSource}\nNostrCompose`, {FormElement, window, tiny, URL, isHex64: value => /^[0-9a-f]{64}$/.test(value || "")});
  const component = new Constructor();
  const form = {elements: Object.fromEntries(Object.entries(values).map(([key, value]) => [key, {value}])), reset: () => { resets++; }};
  return {component, form, sent, signed, resets: () => resets};
}
const rootAttrs = {coordinate, root, "root-pubkey": author, "root-kind": "1621"};

test("issue composition signs subject, deduplicated labels and body", async () => {
  const s = setup({kind: "1621", coordinate}, {title: " Bug ", labels: "ui, bug, ui", content: "Markdown **body**"});
  await s.component.submit(s.form);
  assert.deepEqual(s.sent[0].event.tags, [["a", coordinate], ["p", author], ["subject", "Bug"], ["t", "ui"], ["t", "bug"]]);
  assert.equal(s.sent[0].path, "/events");
  assert.equal(s.sent[0].event.kind, 1621);
  assert.equal(verifyEvent(s.sent[0].event), true);
  assert.equal(s.resets(), 1);
});

test("nested NIP-22 replies preserve the root and address the immediate parent", async () => {
  const s = setup({...rootAttrs, kind: "1111", parent, "parent-pubkey": parentAuthor, "parent-kind": "1111"}, {content: "A reply"});
  await s.component.submit(s.form);
  assert.deepEqual(s.sent[0].event.tags, [["a", coordinate], ["E", root, "", author], ["K", "1621"], ["P", author], ["e", parent, "", parentAuthor], ["k", "1111"], ["p", parentAuthor]]);
});

test("review comments carry file and line tags from hidden inputs or attributes", async () => {
  const pullAttrs = {coordinate, root, "root-pubkey": author, "root-kind": "1618"};
  const fromInputs = setup({...pullAttrs, kind: "1111"}, {content: "Prefer two", file: "README", line: "2", side: "new"});
  await fromInputs.component.submit(fromInputs.form);
  assert.deepEqual(fromInputs.sent[0].event.tags.slice(-2), [["file", "README"], ["line", "2", "new"]]);
  const fromAttributes = setup({...pullAttrs, kind: "1111", file: "src/app.js", line: "14", side: "old"}, {content: "Keep this"});
  await fromAttributes.component.submit(fromAttributes.form);
  assert.deepEqual(fromAttributes.sent[0].event.tags.slice(-2), [["file", "src/app.js"], ["line", "14", "old"]]);
  const plain = setup({...pullAttrs, kind: "1111"}, {content: "General note"});
  await plain.component.submit(plain.form);
  assert.equal(plain.sent[0].event.tags.some(tag => tag[0] === "file" || tag[0] === "line"), false);
});

test("incomplete or misplaced line references are rejected before signing", async () => {
  const pullAttrs = {coordinate, root, "root-pubkey": author, "root-kind": "1618"};
  for (const [attrs, values] of [
    [{...pullAttrs, kind: "1111"}, {content: "x", file: "README", line: "0", side: "new"}],
    [{...pullAttrs, kind: "1111"}, {content: "x", file: "README", line: "2", side: "left"}],
    [{...pullAttrs, kind: "1111"}, {content: "x", line: "2", side: "new"}],
    [{...pullAttrs, kind: "1111"}, {content: "x", file: "README"}],
    [{...rootAttrs, kind: "1111"}, {content: "x", file: "README", line: "2", side: "new"}]
  ]) {
    const s = setup(attrs, values);
    await assert.rejects(s.component.submit(s.form), /line|pull request/);
    assert.equal(s.signed.length, 0);
    assert.equal(s.sent.length, 0);
  }
});

test("status selector signs a NIP-34 root marker with an optional empty body", async () => {
  const s = setup({...rootAttrs, kind: "status"}, {status: "1632", content: ""});
  await s.component.submit(s.form);
  assert.equal(s.sent[0].event.kind, 1632);
  assert.deepEqual(s.sent[0].event.tags, [["a", coordinate], ["e", root, "", "root"], ["p", author]]);
});

test("PRs publish the commit, clone and merge-base tags", async () => {
  const commit = "1".repeat(40), base = "2".repeat(40);
  const s = setup({kind: "1618", coordinate}, {title: "Change", commit, "merge-base": base, clone: "https://example.test/project.git", content: "Why"});
  await s.component.submit(s.form);
  assert.deepEqual(s.sent[0].event.tags.slice(-3), [["c", commit], ["clone", "https://example.test/project.git"], ["merge-base", base]]);
});

test("relay rejection and signer substitution retain the draft", async () => {
  for (const options of [{result: {accepted: false, message: "blocked"}}, {change: true}]) {
    const s = setup({kind: "1621", coordinate}, {title: "Bug", content: "Details"}, options);
    await assert.rejects(s.component.submit(s.form), /blocked|changed/);
    assert.equal(s.resets(), 0);
    if (options.change) assert.equal(s.sent.length, 0);
  }
});

test("invalid conversation addresses and PR commit IDs are rejected before signing", async () => {
  for (const [attrs, values] of [[{...rootAttrs, kind: "1111", parent: "bad"}, {content: "Reply"}], [{kind: "1618", coordinate}, {title: "PR", commit: "main", clone: "https://example.test/repo"}]]) {
    const s = setup(attrs, values);
    await assert.rejects(s.component.submit(s.form));
    assert.equal(s.signed.length, 0);
    assert.equal(s.sent.length, 0);
  }
});
