import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const avatarSource = source.slice(source.indexOf("  class NostrAvatar "), source.indexOf("  // PageLink"));
const key = "a".repeat(64);

function setup(userPromise) {
  const images = [];
  const document = {
    createTextNode: text => ({textContent: text}),
    createElement: name => {
      const image = {nodeName: name, addEventListener: (event, callback) => { image.handlers ??= {}; image.handlers[event] = callback; }, setAttribute() {}};
      images.push(image);
      return image;
    }
  };
  const window = {tinyNames: {load: () => typeof userPromise === "function" ? userPromise() : userPromise}};
  const HTMLElement = class {
    constructor() { this.attributes = {}; this.children = []; }
    getAttribute(name) { return this.attributes[name] ?? null; }
    replaceChildren(...children) { this.children = children; }
    querySelector(name) { return this.children.find(child => child.nodeName === name) || null; }
  };
  const el = (name, text) => { const node = document.createElement(name); if (text !== undefined) node.textContent = text; return node; };
  const Constructor = vm.runInNewContext(`${avatarSource}\nNostrAvatar`, {HTMLElement, window, document, URL, Promise, String, setTimeout, el});
  const component = new Constructor();
  component.attributes.pubkey = key;
  return {component, images};
}

test("loads a safe profile image with privacy attributes", async () => {
  const {component, images} = setup(Promise.resolve({image: "https://cdn.example/avatar.png"}));
  component.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  const image = component.children[0];
  assert.equal(image.src, "https://cdn.example/avatar.png");
  assert.equal(image.loading, "lazy");
  assert.equal(image.referrerPolicy, "no-referrer");
  assert.equal(image.alt, "");
});

test("keeps the two character fallback for missing or unsafe pictures", async () => {
  for (const image of [undefined, "javascript:alert(1)", "/avatar.png"]) {
    const {component} = setup(Promise.resolve(image === undefined ? {} : {image}));
    component.connectedCallback();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(component.children[0].textContent, "AA");
  }
});

test("restores the fallback after an image load failure", async () => {
  const {component, images} = setup(Promise.resolve({image: "https://cdn.example/avatar.png"}));
  component.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  images[0].handlers.error();
  assert.equal(component.children[0].textContent, "AA");
});

test("ignores a stale profile result after the pubkey changes", async () => {
  let resolve, calls = 0;
  const {component} = setup(() => calls++ === 0 ? new Promise(done => { resolve = done; }) : Promise.resolve({}));
  component.connectedCallback();
  component.attributes.pubkey = "b".repeat(64);
  component.attributeChangedCallback();
  resolve({image: "https://cdn.example/avatar.png"});
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(component.children[0].textContent, "BB");
});

test("replaces a prior image when the pubkey becomes invalid", async () => {
  const {component} = setup(Promise.resolve({image: "https://cdn.example/avatar.png"}));
  component.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  component.attributes.pubkey = "bad";
  component.attributeChangedCallback();
  assert.equal(component.children[0].textContent, "BA");
});
