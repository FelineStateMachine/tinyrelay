#!/usr/bin/env node
// Seed a disposable members-only room with image and generic-file messages.
// The owner key is the public QA key used by the other scripts in this folder.
import { mkdir, writeFile, readFile } from "node:fs/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { resolve, join } from "node:path";
import { createHash } from "node:crypto";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";
import { npubEncode } from "nostr-tools/nip19";

const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const base = option("--relay", "http://127.0.0.1:18459").replace(/\/$/, "");
const dataDir = resolve(option("--data", "/tmp/tinyrelay-room-attachments"));
const outDir = resolve(option("--out", "output/playwright/room-attachments"));
const secret = Uint8Array.from(Buffer.from("fc1d06a0fd5e622dcf448d0b3c2fccc891a5ecf74546a7675460d92441fecfdd", "hex"));
const agentSecret = Uint8Array.from(Buffer.from("4f6d7a2be7f2c6f677b1dc2d02f4388cbd1b9aeb8e5e47d41e4ae0f2eb3bde5f", "hex"));
const owner = getPublicKey(secret), agent = getPublicKey(agentSecret), room = "attachments-qa";
const json = value => JSON.stringify(value);
const now = () => Math.floor(Date.now() / 1000);
const exec = promisify(execFile);
async function signed(path, method, value, signer = secret, contentType = "application/json") {
  const body = Buffer.isBuffer(value) ? value : Buffer.from(json(value));
  const payload = createHash("sha256").update(body).digest("hex");
  const auth = finalizeEvent({kind: 27235, created_at: now(), tags: [["u", base + path], ["method", method], ["payload", payload]], content: ""}, signer);
  const token = "Nostr " + Buffer.from(json(auth)).toString("base64");
  const response = await fetch(base + path, {method, headers: {authorization: token, "content-type": contentType}, body});
  const result = await response.json().catch(() => ({}));
  if (!response.ok && !(method === "POST" && path === "/events" && String(result.message || "").startsWith("duplicate:"))) throw Error(`${method} ${path}: ${response.status} ${json(result)}`);
  return result;
}
async function publish(event) { return signed("/events", "POST", event); }
async function upload(bytes, type, filename) {
  const hash = createHash("sha256").update(bytes).digest("hex");
  const descriptor = await signed(`/rooms/${room}/attachments?filename=${encodeURIComponent(filename)}`, "PUT", bytes, secret, type);
  if (descriptor.sha256 !== hash || descriptor.size !== bytes.length) throw Error("fixture upload descriptor mismatch");
  return descriptor;
}
await mkdir(dataDir, {recursive: true});
await mkdir(outDir, {recursive: true});
// Tenant ownership grants management access but event writes still require a
// community membership record in a fresh disposable tenant.
await signed("/manage/rpc", "POST", {method: "setmember", params: [owner, {role: "member"}]});
const create = finalizeEvent({kind: 9007, created_at: now(), tags: [["h", room], ["name", "Attachment QA"], ["about", "Disposable room for browser attachment checks."], ["visibility", "members"]], content: ""}, secret);
await publish(create);
await signed("/manage/rpc", "POST", {method: "setmember", params: [agent, {role: "member"}]});
await publish(finalizeEvent({kind: 9000, created_at: now(), tags: [["h", room], ["p", agent, "member"]], content: ""}, secret));
const cardPath = join(outDir, "fixture-card.png");
await exec("/opt/homebrew/bin/convert", ["-size", "320x160", "gradient:#244f3b-#9bd4b1", "-fill", "#10261e", "-stroke", "#d7ffe6", "-strokewidth", "3", "-draw", "roundrectangle 15,15 305,145 12,12", cardPath]);
const png = await readFile(cardPath);
await writeFile(join(outDir, "browser-upload.png"), png);
const image = await upload(png, "image/png", "fixture.png");
const text = Buffer.from("Attachment QA generic file\n");
await writeFile(join(outDir, "only.txt"), Buffer.from("only attachment"));
const file = await upload(text, "text/plain", "fixture notes.txt");
const message = (descriptor, signer = secret, content = "") => finalizeEvent({kind: 9, created_at: now(), tags: [["h", room], ["imeta", `url ${descriptor.url}`, `m ${descriptor.type}`, `x ${descriptor.sha256}`, `size ${descriptor.size}`, `filename ${descriptor.filename}`]], content: `${content ? content + "\n\n" : ""}${descriptor.type.startsWith("image/") ? `![image](${descriptor.url})` : `[${descriptor.filename}](${descriptor.url})`}`}, signer);
const initial = message(image, secret, "Initial image attachment");
await publish(initial);
const agentMessage = message(file, agentSecret, "Agent published file");
await signed("/events", "POST", agentMessage, agentSecret);
const manifest = {schema: 1, relay: base, dataDir, room, owner, ownerNpub: npubEncode(owner), agent, image, file, initial: initial.id, agentMessage: agentMessage.id};
await writeFile(join(outDir, "fixture.json"), json(manifest) + "\n");
console.log(json(manifest));
