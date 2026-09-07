// Global setup: provision the tenant under test with the tiny CLI so the
// conformance suite writes as its owner. Set TINY_PREPROVISIONED=1 to skip.
import { execFileSync } from "node:child_process";
import { getPublicKey } from "nostr-tools/pure";
import { sha256 } from "@noble/hashes/sha2.js";
import { hexToBytes } from "@noble/hashes/utils.js";

const relayURL = process.env.RELAY_URL ?? "ws://127.0.0.1:7447";
const httpURL = relayURL.replace(/^ws/, "http").replace(/\/$/, "") + "/";
const owner = process.env.CLAIM_SK
  ? hexToBytes(process.env.CLAIM_SK)
  : sha256(new TextEncoder().encode("tiny conformance owner"));

function provision() {
  const bin = process.env.TINY_BIN ?? "tiny";
  const name = process.env.TINY_TENANT ?? new URL(httpURL).hostname.split(".")[0];
  const template = process.env.TINY_TEMPLATE ?? "default";
  const args = ["tenant", "create", "--name", name, "--owner", getPublicKey(owner), "--template", template];
  if (process.env.TINY_DATA_DIR) args.push("--data-dir", process.env.TINY_DATA_DIR);
  execFileSync(bin, args, { stdio: "inherit" });
}

export default async function setup() {
  if (process.env.TINY_PREPROVISIONED !== "1") provision();
  const response = await fetch(httpURL, { headers: { accept: "application/nostr+json" } });
  if (!response.ok) throw new Error(`relay did not become ready at ${httpURL}: ${response.status}`);
  console.log(`conformance tenant ready at ${relayURL} as ${getPublicKey(owner)}`);
}
