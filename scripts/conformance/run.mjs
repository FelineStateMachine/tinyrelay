#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";

const root = resolve(import.meta.dirname, "../..");
const stamp = new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d{3}Z$/, "Z");
const artifactDir = resolve(process.env.CONFORMANCE_ARTIFACTS ?? resolve(root, "artifacts/conformance"), stamp);
mkdirSync(artifactDir, { recursive: true });
const env = { ...process.env, BINDWS_ROOT: process.env.BINDWS_ROOT ?? resolve(root, "../bindws"), RELAY_URL: process.env.RELAY_URL ?? "ws://127.0.0.1:7447" };
writeFileSync(resolve(artifactDir, "environment.txt"), Object.entries({ RELAY_URL: env.RELAY_URL, BINDWS_ROOT: env.BINDWS_ROOT, NODE: process.version, PLATFORM: process.platform, ARCH: process.arch }).map(([key, value]) => `${key}=${value}`).join("\n") + "\n");
const result = spawnSync(process.execPath, [resolve(root, "node_modules/vitest/vitest.mjs"), "run", "--config", resolve(root, "scripts/conformance/vitest.config.mts"), ...process.argv.slice(2)], { cwd: root, env, encoding: "utf8", stdio: ["inherit", "pipe", "pipe"] });
process.stdout.write(result.stdout ?? "");
process.stderr.write(result.stderr ?? "");
writeFileSync(resolve(artifactDir, "stdout.log"), result.stdout ?? "");
writeFileSync(resolve(artifactDir, "stderr.log"), result.stderr ?? "");
writeFileSync(resolve(artifactDir, "status"), `${result.status ?? 1}\n`);
console.error(`conformance artifacts: ${artifactDir}`);
process.exit(result.status ?? 1);
