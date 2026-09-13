import { build } from "esbuild";
import { signerBundle } from "./bundle-config.mjs";

await build(signerBundle("tinyclient/signer.js"));
