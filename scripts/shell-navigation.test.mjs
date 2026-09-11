// Navigation keeps in-place refreshes comfortable while making a new route
// start at a predictable position on small screens.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const start = source.indexOf("    const swapShell = (text, url, push) => {");
const end = source.indexOf("    const load = async (url, push) => {", start);
const swapSource = source.slice(start, end);

function setup({url = "https://relay.test/rooms/general", active = "content", contentScroll = 0, pageScroll = 0} = {}) {
  const calls = {scroll: [], history: [], focus: []};
  const elements = {
    "#topbar": {outerHTML: "<header id=topbar></header>"},
    "#railbox": {outerHTML: "<aside id=railbox></aside>"},
    "#content": {outerHTML: "<main id=content></main>", scrollTop: contentScroll, focus(options) { calls.focus.push({id: "content", options}); }},
    "#panel": {outerHTML: "<aside id=panel></aside>"},
    "#footer": {outerHTML: "<footer id=footer></footer>"}
  };
  const nav = {open: false, removeAttribute(name) { if (name === "open") this.open = false; }};
  const context = {open: false, removeAttribute(name) { if (name === "open") this.open = false; }};
  const draft = {id: "draft", focus(options) { calls.focus.push({id: this.id, options}); }};
  const document = {
    activeElement: active === "draft" ? draft : elements["#content"],
    title: "old",
    querySelector(selector) { return elements[selector] || null; },
    getElementById(id) { return id === "nav-menu" ? nav : id === "context-menu" ? context : id === "content" ? elements["#content"] : id === "draft" ? draft : null; },
    dispatchEvent() {}
  };
  const parsed = {
    title: "new",
    querySelector(selector) { return elements[selector] || null; }
  };
  const sandbox = {
    document,
    DOMParser: class { parseFromString() { return parsed; } },
    CustomEvent: class { constructor(type, init) { this.type = type; this.detail = init.detail; } },
    history: {pushState(_state, _title, href) { calls.history.push(href); }},
    window: {scrollY: pageScroll, scrollTo(...args) { calls.scroll.push(args); }},
    location: {href: url},
    morph() {},
    navTargets: ["#topbar", "#railbox", "#content", "#panel", "#footer"],
    navigationSerial: 0,
    closeStreams() {},
    repairComponents() {}
  };
  // Keep the production closure intact, while supplying only its DOM shell
  // dependencies. The returned function is the implementation under test.
  const swap = vm.runInNewContext(`(() => { let renderedURL = ${JSON.stringify(url)}; const navTargets = ${JSON.stringify(sandbox.navTargets)}; let navigationSerial = 0; const closeStreams = () => {}; const repairComponents = () => {}; const ensureModules = () => {}; const decorate = () => {}; const decorateForms = () => {}; ${swapSource.replace("    const swapShell", "const swapShell")} return swapShell; })()`, sandbox);
  nav.open = true;
  context.open = true;
  return {swap, nav, context, content: elements["#content"], document, calls};
}

test("a changed route resets both menu drawers and scroll", () => {
  const s = setup({contentScroll: 420, pageScroll: 880});
  s.swap("<html></html>", new URL("https://relay.test/rooms/general/thread/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), true);
  assert.equal(s.nav.open, false);
  assert.equal(s.context.open, false);
  assert.equal(s.content.scrollTop, 0);
  assert.deepEqual(s.calls.scroll, [[0, 0]]);
  assert.equal(s.calls.focus.length, 1);
  assert.equal(s.calls.focus[0].id, "content");
  assert.equal(s.calls.focus[0].options.preventScroll, true);
  assert.equal(s.calls.history.length, 1);
});

test("an in-place refresh preserves scroll and the active draft", () => {
  const s = setup({active: "draft", contentScroll: 315, pageScroll: 640});
  s.nav.open = false;
  s.context.open = false;
  s.swap("<html></html>", new URL("https://relay.test/rooms/general"), false);
  assert.equal(s.content.scrollTop, 315);
  assert.deepEqual(s.calls.scroll, []);
  assert.equal(s.calls.focus.length, 1);
  assert.equal(s.calls.focus[0].id, "draft");
  assert.equal(s.calls.focus[0].options.preventScroll, true);
  assert.equal(s.nav.open, false);
  assert.equal(s.context.open, false);
});

test("a browser-back route change resets position without pushing history", () => {
  const s = setup({url: "https://relay.test/rooms/general/thread/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", contentScroll: 190, pageScroll: 230});
  s.swap("<html></html>", new URL("https://relay.test/rooms/general"), false);
  assert.equal(s.content.scrollTop, 0);
  assert.deepEqual(s.calls.scroll, [[0, 0]]);
  assert.deepEqual(s.calls.history, []);
});
