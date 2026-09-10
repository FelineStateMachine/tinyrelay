import fs from "node:fs";
import vm from "node:vm";

const tinySource = fs.readFileSync("internal/webui/tiny.js", "utf8");
const signingSource = tinySource.slice(tinySource.indexOf("  // Event publication is one browser boundary"), tinySource.indexOf("  // Feature bundles load"));

export function sharedSigning(window, tiny) {
  const globalThis = {tiny, nostr: window.nostr, NostrSigner: window.NostrSigner};
  vm.runInNewContext(`const tiny = globalThis.tiny;\n${signingSource}`, {globalThis});
  return globalThis.tiny.signing;
}
