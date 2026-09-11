import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

class Node {
  constructor(tag = "") { this.tagName = tag.toUpperCase(); this.children = []; this.attributes = {}; this.listeners = {}; this.textContent = ""; this.dataset = {}; }
  append(...nodes) { for (const node of nodes) if (node && typeof node !== "string") { this.children.push(node); node.parentNode = this; } }
  remove() { this.parentNode?.children.splice(this.parentNode.children.indexOf(this), 1); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  hasAttribute(name) { return Object.hasOwn(this.attributes, name); }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  click() {}
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  querySelectorAll(selector) {
    const selectors = selector.split(",").map(value => value.trim());
    const match = node => selectors.some(value => {
      const attr = value.match(/^([^[]+)?\[([^=\]]+)(?:="([^"]+)")?\]$/);
      if (attr) return (!attr[1] || node.tagName === attr[1].toUpperCase()) && node.hasAttribute(attr[2]) && (attr[3] === undefined || node.getAttribute(attr[2]) === attr[3]);
      return node.tagName === value.toUpperCase();
    });
    const found = [];
    const walk = node => { for (const child of node.children) { if (match(child)) found.push(child); walk(child); } };
    walk(this); return found;
  }
  set innerHTML(value) {
    this.children = [];
    const stack = [this];
    for (const token of value.match(/<[^>]+>|[^<]+/g) || []) {
      if (!token.startsWith("<")) continue;
      if (/^<\//.test(token)) { stack.pop(); continue; }
      const match = token.match(/^<([\w-]+)([^>]*)>/); if (!match) continue;
      const child = new Node(match[1]);
      for (const [, name, quote, attrValue] of match[2].matchAll(/([\w-]+)(?:=(['"])(.*?)\2)?/g)) child.setAttribute(name, attrValue ?? "");
      stack.at(-1).append(child);
      if (!/^(input|br|meta|img)$/.test(match[1])) stack.push(child);
    }
  }
}

const setup = () => {
  const document = new Node("document");
  document.readyState = "complete";
  document.documentElement = new Node("html");
  document.body = new Node("body");
  document.createElement = name => new Node(name);
  document.addEventListener = () => {};
  document.dispatchEvent = () => {};
  document.querySelectorAll = () => [];
  const customElements = {registry: {}, define(name, ctor) { this.registry[name] = ctor; }};
  const location = {href: "https://relay.test/file?hash=cipher", origin: "https://relay.test", pathname: "/file", hash: ""};
  const window = {document, customElements, location, addEventListener: () => {}, tiny: {util: {}, blossom: {encryption: {}}}};
  class HTMLElement extends Node {}
  Object.assign(globalThis, {window, document, customElements, HTMLElement, location, Blob, URL, CustomEvent: class {}});
  globalThis.tiny = window.tiny;
  vm.runInThisContext(fs.readFileSync("internal/webui/tiny.js", "utf8"));
  window.tiny.localPath = path => "/r/test" + path;
  window.tiny.require = async () => {};
  window.tiny.sha256hex = async () => "cipher";
  window.tiny.blossom.encryption.decryptCHK = async () => new TextEncoder().encode("video bytes");
  vm.runInThisContext(fs.readFileSync("internal/webui/components.js", "utf8"));
  return {FileTools: customElements.registry["file-tools"], window};
};

const makeTool = (FileTools, window, type = "video/mp4") => {
  window.location.hash = "#enc=chk-v1&key=secret&name=clip.mp4&type=" + encodeURIComponent(type);
  const tool = new FileTools();
  tool.setAttribute("hash", "cipher");
  tool.setAttribute("type", "application/octet-stream");
  return tool;
};

test("encrypted MP4 renders a local video preview and replaces it on retry", async () => {
  const {FileTools, window} = setup();
  const revoked = [];
  const originalCreate = URL.createObjectURL, originalRevoke = URL.revokeObjectURL;
  URL.createObjectURL = blob => "blob:preview-" + blob.type + "-" + Math.random();
  URL.revokeObjectURL = url => revoked.push(url);
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(new Uint8Array([1, 2, 3]));
  try {
    const tool = makeTool(FileTools, window); tool.connectedCallback();
    await new Promise(resolve => setTimeout(resolve, 0));
    const first = tool.querySelector("[data-decrypted-preview]");
    assert.ok(first);
    assert.equal(first.querySelector("video").controls, true);
    assert.equal(first.querySelector("video").preload, "metadata");
    assert.equal(first.querySelector("video").src.startsWith("blob:preview-video/mp4-"), true);
    await tool.decryptFromFragment();
    assert.equal(tool.querySelectorAll("[data-decrypted-preview]").length, 1);
    assert.equal(revoked.length, 1, "retry releases the previous object URL");
    tool.disconnectedCallback();
    assert.equal(revoked.length, 2, "disconnect releases the current object URL");
  } finally {
    globalThis.fetch = originalFetch;
    URL.createObjectURL = originalCreate;
    URL.revokeObjectURL = originalRevoke;
  }
});

test("HTML and SVG render as inert bounded source text", async () => {
  for (const type of ["image/svg+xml", "text/html"]) {
    const {FileTools, window} = setup();
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async () => new Response(new Uint8Array([1, 2, 3]));
    try {
      const tool = makeTool(FileTools, window, type); tool.connectedCallback();
      await tool.decryptFromFragment();
      const preview = tool.querySelector("[data-decrypted-preview]");
      assert.ok(preview, type + " should have a source preview");
      assert.equal(preview.querySelector("pre").textContent, "video bytes");
      assert.equal(preview.querySelector("img"), null);
      assert.equal(preview.querySelector("iframe"), null);
      assert.ok(tool.querySelector("a[download]") || tool.querySelector("a"));
    } finally { globalThis.fetch = originalFetch; }
  }
});

test("plain text preview is bounded while download remains complete", async () => {
  const {FileTools, window} = setup();
  const originalFetch = globalThis.fetch;
  const originalDecrypt = window.tiny.blossom.encryption.decryptCHK;
  const source = "A".repeat(256 * 1024 + 20);
  window.tiny.blossom.encryption.decryptCHK = async () => new TextEncoder().encode(source);
  globalThis.fetch = async () => new Response(new Uint8Array([1, 2, 3]));
  try {
    const tool = makeTool(FileTools, window, "text/plain"); tool.connectedCallback();
    await new Promise(resolve => setTimeout(resolve, 0));
    const pre = tool.querySelector("pre");
    assert.ok(pre);
    assert.equal(pre.textContent.startsWith("A".repeat(256 * 1024)), true);
    assert.match(pre.textContent, /Preview truncated/);
    const downloaded = await (await originalFetch(tool.downloadURL)).text();
    assert.equal(downloaded, source);
  } finally {
    window.tiny.blossom.encryption.decryptCHK = originalDecrypt;
    globalThis.fetch = originalFetch;
  }
});
