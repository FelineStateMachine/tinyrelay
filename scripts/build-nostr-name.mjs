import { build } from "esbuild";
import { nostrNameBundle } from "./bundle-config.mjs";

await build(nostrNameBundle("tinyclient/nostr-name.js"));
