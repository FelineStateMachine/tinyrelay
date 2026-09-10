import { build } from "esbuild";
import { nostrNameBundle } from "./bundle-config.mjs";

await build(nostrNameBundle("internal/webui/nostr-name.js"));
