import { execFileSync } from "node:child_process";
import { getPublicKey, finalizeEvent } from "nostr-tools/pure";
import { getToken } from "nostr-tools/nip98";
import { sha256 } from "@noble/hashes/sha2.js";
import { hexToBytes } from "../../../bindws/src/negentropy.ts";

const relayURL = process.env.RELAY_URL ?? "ws://127.0.0.1:7447";
const httpURL = relayURL.replace(/^ws/, "http").replace(/\/$/, "") + "/";
const owner = process.env.CLAIM_SK
  ? hexToBytes(process.env.CLAIM_SK)
  : sha256(new TextEncoder().encode("tiny conformance owner"));

function provisionWithTiny() {
  const bin = process.env.TINY_BIN ?? "tiny";
  const name = process.env.TINY_TENANT ?? new URL(httpURL).hostname.split(".")[0];
  const template = process.env.TINY_TEMPLATE ?? "default";
  execFileSync(bin, ["tenant", "create", "--name", name, "--owner", getPublicKey(owner), "--template", template], { stdio: "inherit" });
}

async function provisionLegacy() {
  const payload = { method: "claim", params: [] };
  const body = JSON.stringify(payload);
  const token = await getToken(httpURL, "POST", (event) => finalizeEvent(event, owner), true, payload);
  const response = await fetch(httpURL, { method: "POST", headers: { "content-type": "application/nostr+json+rpc", authorization: token }, body });
  const json: any = await response.json();
  if (!json.result?.claimed) throw new Error(`legacy claim failed: ${response.status} ${JSON.stringify(json)}`);
  console.warn("conformance: LEGACY_CLAIM=1 uses bindws claim compatibility");
}

export default async function setup() {
  if (process.env.LEGACY_CLAIM === "1") await provisionLegacy();
  else if (process.env.TINY_PREPROVISIONED !== "1") provisionWithTiny();
  const response = await fetch(httpURL, { headers: { accept: "application/nostr+json" } });
  if (!response.ok) throw new Error(`relay did not become ready at ${httpURL}: ${response.status}`);
  console.log(`conformance tenant ready at ${relayURL} as ${getPublicKey(owner)}`);
}
