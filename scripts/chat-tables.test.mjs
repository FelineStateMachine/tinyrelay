import test from "node:test";
import assert from "node:assert/strict";
import { FakeNode, setup } from "./rooms.test.mjs";

test("chat tables render semantic headers, alignment and uneven rows", () => {
  const s = setup();
  const body = s.rooms.chatMarkdown(new FakeNode("div"), "| Name | Notes | Score |\n| :--- | :---: | ---: |\n| Ada | **ready** | 10 |\n| Grace | `a|b` |\n| Alan | left");
  const region = body.children[0], table = region.querySelector("table");
  assert.equal(region.getAttribute("data-markdown-table"), "");
  assert.equal(region.getAttribute("role"), "region");
  assert.equal(region.getAttribute("aria-label"), "Table");
  assert.equal(region.getAttribute("tabindex"), "0");
  assert.deepEqual(table.querySelectorAll("th").map(cell => [cell.getAttribute("scope"), cell.getAttribute("data-align")]), [["col", "left"], ["col", "center"], ["col", "right"]]);
  assert.equal(table.querySelectorAll("tbody tr").length, 3);
  assert.equal(table.querySelectorAll("tbody tr")[1].children.length, 3);
  assert.equal(table.querySelectorAll("tbody tr")[1].children[1].querySelector("code").textContent, "a|b");
  assert.equal(table.querySelectorAll("tbody tr")[2].children[2].textContent, "");
  assert.equal(table.querySelector("strong").textContent, "ready");
});

test("chat tables accept escaped pipes and keep inline content safe", () => {
  const s = setup();
  const body = s.rooms.chatMarkdown(new FakeNode("div"), "Label | Link\n--- | ---\nA \\| B | [go](javascript:alert(1)) <img>");
  const table = body.querySelector("table");
  assert.equal(table.querySelector("tbody td").textContent, "A | B");
  assert.equal(table.querySelectorAll("a").length, 0);
  assert.equal(table.textContent.includes("<img>"), true);
});

test("chat tables preserve single columns and distinguish escaped trailing pipes", () => {
  const s = setup();
  const body = s.rooms.chatMarkdown(new FakeNode("div"), "| Value |\n| --- |\n| `a\\|b` |");
  assert.equal(body.querySelectorAll("table").length, 1);
  assert.equal(body.querySelector("th").textContent, "Value");
  assert.equal(body.querySelector("tbody code").textContent, "a|b");
  const prose = s.rooms.chatMarkdown(new FakeNode("div"), "A \\\\| B\n| --- |");
  assert.equal(prose.querySelectorAll("table").length, 0);
});

test("malformed tables and tables in fenced code remain prose or code", () => {
  const s = setup();
  const body = s.rooms.chatMarkdown(new FakeNode("div"), "| Header | Other\n| -- | ---\n\n```\n| A | B |\n| --- | --- |\n```");
  assert.equal(body.querySelectorAll("[data-markdown-table]").length, 0);
  assert.equal(body.querySelectorAll("pre").length, 1);
  assert.equal(body.querySelector("p").textContent, "| Header | Other| -- | ---");
  assert.equal(body.querySelector("pre").textContent.includes("| A | B |"), true);
});

test("rerendering edited chat content creates a fresh table node", () => {
  const s = setup();
  const body = new FakeNode("div");
  s.rooms.chatMarkdown(body, "| Before | State |\n| --- | --- |\n| old | draft |");
  assert.equal(body.querySelector("tbody td").textContent, "old");
  body.replaceChildren();
  s.rooms.chatMarkdown(body, "| After | State |\n| --- | --- |\n| new | sent |");
  assert.equal(body.querySelector("th").textContent, "After");
  assert.equal(body.querySelector("tbody td").textContent, "new");
});

test("tables stop before headings and preserve backtick and tilde fences", () => {
  const s = setup();
  for (const marker of ["```", "  ~~~~"]) {
    const body = s.rooms.chatMarkdown(new FakeNode("div"), "| Name | Value |\n| --- | --- |\n| one | two |\n" + marker + "text | example\n| A | B |\n|---|---|\n" + marker + "\n# Heading | text");
    assert.equal(body.querySelectorAll("table").length, 1);
    assert.equal(body.querySelector("th").getAttribute("data-align"), null);
    assert.equal(body.querySelectorAll("tbody tr").length, 1);
    assert.equal(body.querySelector("pre").textContent, "| A | B |\n|---|---|");
    assert.equal(body.children.find(node => node.localName === "h1").textContent, "Heading | text");
  }
});
