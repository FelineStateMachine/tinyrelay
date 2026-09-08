#!/usr/bin/env node
// Seed a deterministic, public corpus for browser UX fuzzing.
// The script is intentionally self-contained so it can be run against a
// disposable daemon without changing a checkout or a user's home directory.
import { execFile } from "node:child_process";
import { mkdir, writeFile, readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import WebSocket from "ws";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";
import { getToken } from "nostr-tools/nip98";
import { npubEncode } from "nostr-tools/nip19";

const exec = promisify(execFile);
const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const http = (option("--relay", "http://127.0.0.1:18447")).replace(/^ws/, "http").replace(/\/$/, "");
const wsURL = http.replace(/^http/, "ws");
const dataDir = resolve(option("--data", "/tmp/tinyrelay-ux-20260908"));
const outDir = resolve(option("--out", "output/playwright/ux"));
const secret = Uint8Array.from(Buffer.from("fc1d06a0fd5e622dcf448d0b3c2fccc891a5ecf74546a7675460d92441fecfdd", "hex"));
const owner = getPublicKey(secret);
const now = () => Math.floor(Date.now() / 1000);
const json = value => JSON.stringify(value);

// This is printed before network or filesystem work so browser runners can
// discover the fixture identity even if a later seed operation fails.
console.log(json({ fixture: "tinyrelay-ux", owner, npub: npubEncode(owner), relay: http, dataDir, out: join(outDir, "fixture.json") }));

async function rpc(method, params = []) {
  const payload = { method, params };
  const token = await getToken(http, "POST", event => finalizeEvent(event, secret), true, payload);
  const response = await fetch(http, { method: "POST", headers: { "content-type": "application/nostr+json+rpc", authorization: token }, body: json(payload) });
  const result = await response.json();
  if (!response.ok || result.error) throw Error(`${method}: ${json(result)}`);
  return result.result;
}

class Client {
  constructor(url) {
    this.ws = new WebSocket(url); this.waiters = new Map(); this.events = new Map();
    this.open = new Promise((resolve, reject) => { this.ws.once("open", resolve); this.ws.once("error", reject); });
    this.ws.on("message", raw => { let m; try { m = JSON.parse(raw); } catch { return; }
      if (m[0] === "AUTH") return;
      const waiter = this.waiters.get(m[1]); if (!waiter) return;
      if (m[0] === "OK") {
        const reason = String(m[3] || "publish rejected");
        // Re-seeding an existing fixture is successful when the relay already
        // has this event, or has a newer replaceable event for the same d tag.
        waiter.done(m[2] || /^(duplicate|invalid: a newer version)/.test(reason) ? null : Error(reason), m);
      }
      else if (m[0] === "EOSE") waiter.done(null, waiter.items);
      else if (m[0] === "EVENT") waiter.items.push(m[2]);
      else if (m[0] === "CLOSED") waiter.done(Error(m[2]));
    });
  }
  async publish(event) { await this.open; return new Promise((resolve, reject) => {
    const finish = (e, value) => { clearTimeout(timer); this.waiters.delete(event.id); e ? reject(e) : resolve(value); };
    const timer = setTimeout(() => finish(Error("publish timeout")), 30000);
    this.waiters.set(event.id, { done: finish, items: [] }); this.ws.send(json(["EVENT", event]));
  }); }
  close() { this.ws.close(); }
}

function event(kind, content, tags = [], createdAt = now()) { return finalizeEvent({ kind, content, tags, created_at: createdAt }, secret); }
async function upload(body, type, label) {
  const bytes = Buffer.isBuffer(body) ? body : Buffer.from(body);
  const hash = createHash("sha256").update(bytes).digest("hex");
  const authEvent = finalizeEvent({ kind: 24242, created_at: now(), content: "upload", tags: [["t", "upload"], ["x", hash], ["expiration", String(now() + 300)]] }, secret);
  const token = "Nostr " + Buffer.from(json(authEvent)).toString("base64");
  const response = await fetch(http + "/upload", { method: "PUT", headers: { authorization: token, "content-type": type }, body: bytes });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw Error(`upload ${label}: ${response.status} ${json(result)}`);
  return { label, sha256: hash, type, size: bytes.length, url: result.url || `${http}/${hash}`, response: result };
}
async function git(cwd, ...command) { return (await exec("git", ["-C", cwd, ...command], { encoding: "utf8", maxBuffer: 4 * 1024 * 1024, env: { ...process.env, GIT_TERMINAL_PROMPT: "0", GIT_AUTHOR_NAME: "tinyrelay UX fixture", GIT_AUTHOR_EMAIL: "ux-fixture@example.invalid", GIT_COMMITTER_NAME: "tinyrelay UX fixture", GIT_COMMITTER_EMAIL: "ux-fixture@example.invalid" } })).stdout.trim(); }

await mkdir(dataDir, { recursive: true }); await mkdir(outDir, { recursive: true });
const policy = await rpc("getpolicy");
policy.features = { ...policy.features, files: true, sites: { ...(policy.features?.sites || {}), enabled: true, mirror: true }, grasp: true, grasp02: true, grasp03: true, grasp05: true, grasp06: true };
await rpc("setpolicy", [policy]);
const relay = new Client(wsURL); await relay.open;
// Keep event IDs stable across reruns so the relay's normal event dedupe makes
// this safe to invoke repeatedly against the same disposable daemon.
const created = Number(option("--created-at", "1788876000"));
if (!Number.isSafeInteger(created) || created < 1) throw Error("--created-at must be a positive integer");
const notes = [
  event(1, "UX fixture note: <script>alert('xss')</script> & \"quoted\" — emoji 🧪\n\nhttps://example.com/a?x=1&y=2\n\n" + "long text ".repeat(120), [["t", "ux-fixture"], ["d", "note-edge"]], created),
  event(1, "Short note with [markdown](https://example.com) and a bare https://example.org/path#fragment", [["t", "ux-fixture"], ["t", "links"]], created + 1),
];
const articles = [
  event(30023, "# Article heading\n\nBody containing <b>escaped markup</b>, `code`, and a very long paragraph.\n\n" + "article words ".repeat(180), [["d", "ux-long-article"], ["title", "A long article & edge cases"], ["summary", "Summary with <angle> & ampersand"], ["published_at", String(created)]], created + 2),
  event(30023, "Second revision with the same identifier", [["d", "ux-revision"], ["title", "Revision"]], created + 3),
];
for (const e of [...notes, ...articles]) await relay.publish(e);

const files = [];
files.push(await upload("plain text with <escaped> & unicode café\n".repeat(20), "text/plain; charset=utf-8", "text"));
files.push(await upload("# Markdown fixture\n\n[link](https://example.com)\n\n<script>ignored</script>", "text/markdown; charset=utf-8", "markdown"));
files.push(await upload("<!doctype html><meta charset=utf-8><title>UX HTML</title><p>fixture &amp; markup</p>", "text/html; charset=utf-8", "html"));
files.push(await upload(Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", "base64"), "image/png", "image"));

const repo = join(dataDir, "repo"); await mkdir(repo, { recursive: true });
await git(repo, "init", "-b", "main");
const entries = new Map([
  ["README UX fixture.md", "UX fixture repository\nPaths and refs contain awkward URL characters.\n"],
  ["nested dir/space # hash/percent%/question?.txt", "nested edge path\n"],
  ["nested dir/Unicode café/emoji 🧪.md", "unicode path content\n"],
  ["web/<escaped>.html", "<!doctype html><title>fixture</title><script>ignored</script>"],
]);
for (const [path, content] of entries) { const full = join(repo, path); await mkdir(join(full, ".."), { recursive: true }); await writeFile(full, content); }
await git(repo, "add", ".");
try { await git(repo, "commit", "-m", "UX fixture paths"); } catch (error) { if (!String(error.stdout).includes("nothing to commit")) throw error; }
await git(repo, "tag", "-f", "release/1.0"); await git(repo, "branch", "-f", "feature/café");
// GRASP identifiers are intentionally conservative; awkward characters live
// in the display name and tracked paths, where viewers must still URL-escape.
const repoId = "ux-edge-repo"; const cloneURL = `${http}/${npubEncode(owner)}/${encodeURIComponent(repoId)}.git`;
const refs = (await git(repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")).split("\n").filter(Boolean).map(line => line.split(" "));
const announcement = event(30617, "", [["d", repoId], ["name", "UX space/#/?/% café"], ["description", "Repository used by browser navigation fuzzing"], ["clone", cloneURL], ["relays", wsURL], ["maintainers", owner]]);
await relay.publish(announcement);
const state = event(30618, "", [["d", repoId], ["HEAD", "ref: refs/heads/main"], ...refs.map(([ref, oid]) => [ref, oid])]);
await relay.publish(state);
try { await git(repo, "remote", "set-url", "origin", cloneURL); } catch { await git(repo, "remote", "add", "origin", cloneURL); }
await git(repo, "push", "origin", "--all"); await git(repo, "push", "origin", "--tags");
const manifest = { schema: 1, fixture: "tinyrelay-ux", relay: http, owner, npub: npubEncode(owner), dataDir, notes: notes.map(e => ({ id: e.id, kind: e.kind, tags: e.tags })), articles: articles.map(e => ({ id: e.id, tags: e.tags })), files, repository: { id: repoId, clone: cloneURL, path: repo, refs } };
await writeFile(join(outDir, "fixture.json"), json(manifest) + "\n");
relay.close(); console.log(json({ ok: true, manifest: join(outDir, "fixture.json"), notes: notes.length, articles: articles.length, files: files.length, repository: repoId }));
