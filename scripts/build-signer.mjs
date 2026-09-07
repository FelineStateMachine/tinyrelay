import { build } from "esbuild";

await build({
  entryPoints: ["scripts/signer-entry.mjs"],
  bundle: true,
  minify: true,
  format: "iife",
  platform: "browser",
  target: ["es2020"],
  outfile: "internal/webui/signer.js",
  legalComments: "eof",
  banner: { js: '"use strict";' },
});
