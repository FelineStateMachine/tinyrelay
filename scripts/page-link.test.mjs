// The crumb copies the page's public address on a tap and says so briefly.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const linkSource = source.slice(source.indexOf("  // PageLink is the crumb"), source.indexOf("  // Rooms: the compose bar"));

function setup({url = "https://relay.example/wiki/notes", clipboard = true} = {}) {
  const written = [];
  class HTMLElement {
    constructor() { this.dataset = {}; this.listeners = {}; }
    getAttribute(name) { return name === "url" ? url : null; }
    addEventListener(name, fn) { this.listeners[name] = fn; }
  }
  const timers = [];
  const sandbox = {HTMLElement, navigator: clipboard ? {clipboard: {writeText: async value => { written.push(value); }}} : {}, location: {href: "https://tailnet.example/wiki/notes#x"},
    setTimeout: (fn, ms) => { timers.push({fn, ms}); return timers.length; }, clearTimeout: () => {}};
  const Constructor = vm.runInNewContext(`${linkSource}\nPageLink`, sandbox);
  const link = new Constructor();
  link.connectedCallback();
  return {link, written, timers};
}

test("a tap copies the public address from the server, not the browser's location", async () => {
  const s = setup();
  s.link.listeners.click();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(s.written, ["https://relay.example/wiki/notes"]);
  assert.equal(s.link.dataset.copied, "");
  assert.equal(s.timers[0].ms, 1500);
  s.timers[0].fn();
  assert.equal("copied" in s.link.dataset, false);
});

test("without a clipboard nothing happens and nothing throws", async () => {
  const s = setup({clipboard: false});
  s.link.listeners.click();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(s.written, []);
  assert.equal("copied" in s.link.dataset, false);
});
