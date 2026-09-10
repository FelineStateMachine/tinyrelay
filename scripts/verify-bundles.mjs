import { build } from "esbuild";
import { readFile, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { signerBundle, nostrNameBundle } from "./bundle-config.mjs";

const root = new URL("..", import.meta.url);
const temporaryDirectory = await mkdtemp(join(tmpdir(), "tinyrelay-bundles-"));
const bundles = [
  ["signer", signerBundle(join(temporaryDirectory, "signer.js")), "internal/webui/signer.js"],
  ["nostr-name", nostrNameBundle(join(temporaryDirectory, "nostr-name.js")), "internal/webui/nostr-name.js"],
];

try {
  for (const [name, config, checkedInPath] of bundles) {
    await build(config);
    const [built, checkedIn] = await Promise.all([
      readFile(config.outfile),
      readFile(new URL(checkedInPath, root)),
    ]);
    if (!built.equals(checkedIn)) {
      throw new Error(`${checkedInPath} is stale; run npm run build:${name}`);
    }
  }
  console.log(`verified ${bundles.length} generated browser bundles`);
} finally {
  await rm(temporaryDirectory, { recursive: true, force: true });
}
