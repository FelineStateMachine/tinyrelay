const browserBundle = (entryPoint, outfile) => ({
  entryPoints: [entryPoint],
  bundle: true,
  minify: true,
  format: "iife",
  platform: "browser",
  target: ["es2020"],
  outfile,
  legalComments: "eof",
  banner: { js: '"use strict";' },
});

export const signerBundle = outfile => browserBundle("scripts/signer-entry.mjs", outfile);
export const nostrNameBundle = outfile => browserBundle("scripts/nostr-name-entry.mjs", outfile);
