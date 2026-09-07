import { defineConfig } from "vitest/config";
import { resolve } from "node:path";

const bindws = resolve(process.env.BINDWS_ROOT ?? "../bindws");
const compatibilityNip11 = resolve(import.meta.dirname, "nip11.compat.test.ts");

export default defineConfig({
  root: bindws,
  test: {
    include: ["test/conformance/**/*.test.ts", compatibilityNip11],
    exclude: [resolve(bindws, "test/conformance/nip11.test.ts")],
    environment: "node",
    testTimeout: 60_000,
    hookTimeout: 60_000,
    fileParallelism: false,
    globalSetup: [resolve(process.cwd(), "scripts/conformance/setup.ts")]
  }
});
