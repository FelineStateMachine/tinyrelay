import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync("internal/webui/components.js", "utf8");
const classSource = source.slice(source.indexOf("  class ViewArtifact"), source.indexOf("  // ViewForm adds"));

class HTMLElement {}
const ViewArtifact = vm.runInNewContext(`${classSource}\nViewArtifact`, {HTMLElement});

const setup = ({complete = false, naturalWidth = 100} = {}) => {
  const listeners = {};
  const image = {complete, naturalWidth, hidden: false, addEventListener: (name, fn) => { listeners[name] = fn; }};
  const details = {open: false};
  const artifact = new ViewArtifact();
  artifact.querySelector = selector => selector === "img" ? image : details;
  artifact.connectedCallback();
  return {image, details, fail: () => listeners.error()};
};

test("view artifact keeps a loaded image and source disclosure collapsed", () => {
  const {image, details} = setup({complete: true});
  assert.equal(image.hidden, false);
  assert.equal(details.open, false);
});

test("view artifact opens source and hides a failed image", () => {
  const {image, details, fail} = setup();
  fail();
  assert.equal(image.hidden, true);
  assert.equal(details.open, true);
});

test("view artifact handles an already failed cached image", () => {
  const {image, details} = setup({complete: true, naturalWidth: 0});
  assert.equal(image.hidden, true);
  assert.equal(details.open, true);
});
