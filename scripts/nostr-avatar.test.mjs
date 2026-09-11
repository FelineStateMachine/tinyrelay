import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const avatarSource = source.slice(source.indexOf("  class NostrAvatar "), source.indexOf("  // PageLink"));
const key = "a".repeat(64);
const location = {href: "https://relay.example/r/demo/chat", origin: "https://relay.example"};

function setup(userPromise, tag = "nostr-avatar", children = []) {
  const images = [], requests = [];
  const document = {
    createElement: name => {
      const node = {nodeName: name, children: [], setAttribute(name, value) { this[name] = value; }, append(...nodes) { this.children.push(...nodes); }, addEventListener(event, callback) { this.handlers ??= {}; this.handlers[event] = callback; }};
      if (name === "img") images.push(node);
      return node;
    }
  };
  const window = {tinyNames: {load: request => { requests.push(request); return typeof userPromise === "function" ? userPromise() : userPromise; }}};
  const HTMLElement = class {
    constructor() { this.attributes = {}; this.children = children; }
    getAttribute(name) { return this.attributes[name] ?? null; }
    setAttribute(name, value) { this.attributes[name] = value; }
    replaceChildren(...nodes) { this.children = nodes; }
    append(...nodes) { this.children.push(...nodes); }
    querySelector(name) { return this.children.find(child => child.nodeName === name) || null; }
  };
  const el = name => document.createElement(name);
  const tiny = {localPath: path => "/r/demo" + path};
  const Constructor = vm.runInNewContext(`${avatarSource}\n${tag === "room-avatar" ? "RoomAvatar" : "NostrAvatar"}`, {HTMLElement, window, document, URL, Promise, String, setTimeout, el, tiny, location});
  const component = new Constructor();
  component.attributes.pubkey = key;
  return {component, images, requests};
}

test("loads a profile image with a tenant scoped generated fallback", async () => {
  const {component, requests} = setup(Promise.resolve({image: "https://cdn.example/avatar.png"}));
  component.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  const image = component.children[0];
  assert.equal(image.src, "https://cdn.example/avatar.png");
  assert.equal(image.loading, "lazy");
  assert.equal(image.referrerPolicy, "no-referrer");
  assert.equal(image.alt, "");
  assert.equal(requests[0].pubkey, key);
  assert.equal(requests[0].refresh, true);
});

test("uses generated avatars for missing or unsafe pictures", async () => {
  for (const image of [undefined, "javascript:alert(1)", "/avatar.png"]) {
    const {component} = setup(Promise.resolve(image === undefined ? {} : {image}));
    component.connectedCallback();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(component.children[0].src, "/r/demo/avatars/v1/" + key + ".svg");
  }
  const {component} = setup(Promise.resolve({}));
  component.attributes.pubkey = "bad";
  component.connectedCallback();
  assert.equal(component.children[0].src, "/r/demo/avatars/v1/" + "0".repeat(64) + ".svg");
});

test("restores the generated fallback after an image load failure", async () => {
  const {component, images} = setup(Promise.resolve({image: "https://cdn.example/avatar.png"}));
  component.connectedCallback();
  await new Promise(resolve => setTimeout(resolve, 0));
  images.at(-1).handlers.error();
  assert.equal(component.children[0].src, "/r/demo/avatars/v1/" + key + ".svg");
});

test("ignores stale profile results after the pubkey changes", async () => {
  let resolve, calls = 0;
  const {component} = setup(() => calls++ === 0 ? new Promise(done => { resolve = done; }) : Promise.resolve({}));
  component.connectedCallback();
  component.attributes.pubkey = "b".repeat(64);
  component.attributeChangedCallback();
  resolve({image: "https://cdn.example/avatar.png"});
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(component.children[0].src, "/r/demo/avatars/v1/" + "b".repeat(64) + ".svg");
});

test("room avatars prefer safe pictures and reject unsafe or cross origin fallbacks", () => {
  const fallback = "/r/demo/avatars/v1/" + key + ".svg";
  const {component} = setup(undefined, "room-avatar");
  component.attributes.picture = "https://cdn.example/room.png";
  component.attributes.fallback = fallback;
  component.connectedCallback();
  assert.equal(component.children[0].src, "https://cdn.example/room.png");
  component.attributes.picture = "javascript:alert(1)";
  component.attributeChangedCallback();
  assert.equal(component.children[0].src, "https://relay.example/r/demo/avatars/v1/" + key + ".svg");
  component.attributes.fallback = "https://evil.example/avatars/v1/" + key + ".svg";
  component.attributeChangedCallback();
  assert.equal(component.children.length, 0);
});

test("room avatars accept the root fallback path", () => {
  const fallback = "/avatars/v1/" + key + ".svg";
  const {component} = setup(undefined, "room-avatar");
  component.attributes.fallback = fallback;
  component.connectedCallback();
  assert.equal(component.children[0].src, "https://relay.example" + fallback);
});

test("room avatars restore their fallback after a picture fails", () => {
  const fallback = "/r/demo/avatars/v1/" + key + ".svg";
  const {component, images} = setup(undefined, "room-avatar");
  component.attributes.picture = "https://cdn.example/room.png";
  component.attributes.fallback = fallback;
  component.connectedCallback();
  images[0].handlers.error();
  assert.equal(component.children[0].src, "https://relay.example" + fallback);
  assert.equal(images.at(-1).handlers?.error, undefined);
});
