import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/file-catalog.js", "utf8");
const pubkey = "a".repeat(64);
const hash = "b".repeat(64);

function setup({path = "/r/test/files", actor = pubkey} = {}) {
  const values = new Map();
  const rows = [];
  const document = {
    querySelector(selector) {
      if (selector === "[data-pubkey]") return {getAttribute: () => actor};
      if (selector === "#file-library") return {querySelectorAll: () => rows};
      return null;
    },
    createElement() { return {textContent: "", append() {}}; }
  };
  const location = {href: "https://relay.test" + path, origin: "https://relay.test", pathname: path};
  const history = {replaceState(_state, _title, value) { location.href = "https://relay.test" + value; location.pathname = new URL(location.href).pathname; location.hash = new URL(location.href).hash; }};
  const context = {
    location, history, document,
    localStorage: {getItem: key => values.get(key) || null, setItem: (key, value) => values.set(key, value)},
    URL, URLSearchParams, Date, Number, String, Array, Boolean, Object, RegExp, JSON,
    globalThis: null, tiny: {localPath: value => "/r/test" + value}
  };
  context.globalThis = context;
  vm.runInNewContext(source, context);
  return {context, values, rows};
}

const link = () => `https://relay.test/r/test/file?hash=${hash}#key=${"c".repeat(64)}&iv=${"d".repeat(16)}&name=video.mp4&type=video%2Fmp4`;

test("saves encrypted metadata per tenant and account and restores it after reload", () => {
  const first = setup({path: "/r/test/file?hash=" + hash});
  assert.equal(first.context.tiny.files.catalog.save({hash, link: link(), name: "video.mp4", type: "video/mp4", size: 104857600}, pubkey), true);
  assert.equal(first.context.tiny.files.catalog.get(hash, pubkey).name, "video.mp4");
  assert.equal(first.context.tiny.files.catalog.get(hash, "e".repeat(64)), null);
  assert.equal(first.context.tiny.files.catalog.restore(pubkey).name, "video.mp4");
  assert.match(first.context.location.href, /#key=/);
});

test("rejects unsafe links, names and cross-tenant links", () => {
  const {context} = setup();
  assert.equal(context.tiny.files.catalog.save({hash, link: "https://evil.test/r/test/file?hash=" + hash + "#key=x", name: "x"}, pubkey), false);
  assert.equal(context.tiny.files.catalog.save({hash, link: link(), name: "../secret"}, pubkey), false);
  assert.equal(context.tiny.files.catalog.save({hash, link: link().replace("/r/test/", "/r/other/"), name: "x"}, pubkey), false);
});
