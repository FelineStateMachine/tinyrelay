import { build } from "esbuild";

await build({
  entryPoints: ["scripts/nostr-name-entry.mjs"],
  bundle: true,
  minify: true,
  format: "iife",
  platform: "browser",
  target: ["es2020"],
  outfile: "internal/webui/nostr-name.js",
  legalComments: "eof",
  banner: { js: '"use strict";' },
});
