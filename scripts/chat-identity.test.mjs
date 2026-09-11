import test from "node:test";
import assert from "node:assert/strict";
import {setup, FakeNode} from "./rooms.test.mjs";

test("live room and thread messages identify the viewer and render avatars", () => {
  for (const page of ["room", "thread"]) {
    const s = setup(), section = new FakeNode("section");
    section.id = page;
    section.dataset.viewer = s.pubkey;
    s.sandbox.document.body.append(section);
    const event = {id: "e".repeat(64), pubkey: s.pubkey, kind: 9, created_at: 1789041600, tags: [], content: "My message"};
    const own = s.rooms.roomAppend(event, {room: "build", inThread: page === "thread"});
    assert.equal(own.hasAttribute("data-own"), true);
    assert.equal(own.querySelector("header [data-own-label]").textContent, "you");
    const avatar = own.querySelector(":scope > nostr-avatar");
    assert.equal(avatar.getAttribute("pubkey"), s.pubkey);
    assert.equal(avatar.getAttribute("aria-hidden"), "true");
    assert.equal(avatar.textContent, "", "the avatar component supplies an image rather than initials");
    const other = s.rooms.messageNode({...event, pubkey: "b".repeat(64)});
    assert.equal(other.hasAttribute("data-own"), false);
    assert.equal(other.querySelector("[data-own-label]"), null);
    const notice = s.rooms.messageNode({...event, kind: 44100, tags: [["p", s.pubkey]]});
    assert.equal(notice.hasAttribute("data-own"), false);
    s.rooms.roomEdit({...event, kind: 40003, tags: [["e", event.id]], content: "Edited message"});
    assert.equal(own.hasAttribute("data-own"), true);
    assert.equal(own.querySelector("nostr-avatar"), avatar);
  }
});

test("guest messages do not claim a viewer identity", () => {
  const s = setup();
  const message = s.rooms.messageNode({id: "e".repeat(64), pubkey: s.pubkey, kind: 9, created_at: 1789041600, tags: [], content: "Hello"});
  assert.equal(message.hasAttribute("data-own"), false);
  assert.equal(message.querySelector("[data-own-label]"), null);
});
