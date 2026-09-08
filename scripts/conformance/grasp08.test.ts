// GRASP-08 interoperability uses stock Git Smart HTTP with one reusable,
// repository-root NIP-98 proof shared through git's extraHeader option.
import { execFile } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { promisify } from "node:util";
import { describe, expect, it } from "vitest";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";
import { npubEncode } from "nostr-tools/nip19";
import { getToken } from "nostr-tools/nip98";
import { sha256 } from "@noble/hashes/sha2.js";
import { hexToBytes } from "@noble/hashes/utils.js";
import { Client, HTTP_URL, RELAY_URL, now } from "./helpers.ts";

const exec = promisify(execFile);
const KIND_REPO = 30617;
const KIND_REPO_STATE = 30618;
const OWNER_SK = process.env.CLAIM_SK ? hexToBytes(process.env.CLAIM_SK) : sha256(new TextEncoder().encode("tiny conformance owner"));

async function git(cwd: string, ...args: string[]): Promise<string> {
  const result = await exec("git", args, {
    cwd,
    env: { ...process.env, GIT_TERMINAL_PROMPT: "0" },
  });
  return result.stdout.trim();
}

function auth(sk: Uint8Array, root: string): string {
  const event = finalizeEvent({ kind: 27235, created_at: now(), content: "", tags: [["u", root], ["method", "GET"]] }, sk);
  return `Nostr ${Buffer.from(JSON.stringify(event)).toString("base64")}`;
}

async function rpc(method: string, params: unknown[] = []): Promise<any> {
  const payload = { method, params };
  const token = await getToken(HTTP_URL, "POST", event => finalizeEvent(event, OWNER_SK), true, payload);
  const response = await fetch(HTTP_URL, {
    method: "POST",
    headers: { "Content-Type": "application/nostr+json+rpc", Authorization: token },
    body: JSON.stringify(payload),
  });
  const result: any = await response.json();
  if (!response.ok || result.error) throw new Error(JSON.stringify(result));
  return result.result;
}

async function waitForRefs(url: string, authorization: string): Promise<void> {
  for (let attempt = 0; attempt < 60; attempt++) {
    const response = await fetch(`${url}/info/refs?service=git-upload-pack`, { headers: { authorization } });
    const text = await response.text();
    if (response.status === 200 && text.includes("# service=git-upload-pack")) return;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error("GRASP-08 repository did not become available");
}

describe("GRASP-08 stock Git", () => {
  it("reuses one repository-root proof for clone, fetch and push", async ({ skip }) => {
    if (process.env.TINY_GRASP08 !== "1") skip();
    const policy = await rpc("getpolicy");
    policy.features = { ...policy.features, grasp: true, grasp08: true };
    policy.reads = "members";
    await rpc("setpolicy", [policy]);
    const infoResponse = await fetch(HTTP_URL, { headers: { accept: "application/nostr+json" } });
    const info: any = await infoResponse.json();
    expect(info.supported_grasps).toContain("GRASP-08");

    const root = await mkdtemp(`${tmpdir()}/tiny-grasp08-`);
    const clone = await mkdtemp(`${tmpdir()}/tiny-grasp08-clone-`);
    const sk = OWNER_SK;
    const pubkey = getPublicKey(sk);
    const npub = npubEncode(pubkey);
    const identifier = `stock-${Math.random().toString(36).slice(2, 10)}`;
    const repoURL = `${HTTP_URL}/${npub}/${encodeURIComponent(identifier)}.git`;
    const proof = auth(sk, repoURL);
    const c = await Client.connect(RELAY_URL);
    try {
      await c.auth(OWNER_SK, RELAY_URL);
      await git(root, "init", "--initial-branch=main");
      await git(root, "config", "user.email", "grasp08-conformance@example.com");
      await git(root, "config", "user.name", "GRASP-08 conformance");
      await writeFile(`${root}/README.md`, "private GRASP-08 repository\n");
      await git(root, "add", "README.md");
      await git(root, "commit", "-m", "initial");
      const first = await git(root, "rev-parse", "HEAD");
      const createdAt = now();
      await c.publish(finalizeEvent({ kind: KIND_REPO, content: "", created_at: createdAt, tags: [["d", identifier], ["clone", repoURL], ["relays", RELAY_URL], ["maintainers", pubkey], ["private", "true"]] }, sk));
      await c.publish(finalizeEvent({ kind: KIND_REPO_STATE, content: "", created_at: createdAt + 1, tags: [["d", identifier], ["HEAD", "ref: refs/heads/main"], ["refs/heads/main", first]] }, sk));
      const header = `Authorization: ${proof}`;
      await waitForRefs(repoURL, proof);
      await git(root, "-c", `http.extraHeader=${header}`, "push", repoURL, "HEAD:refs/heads/main");
      await git(clone, "-c", `http.extraHeader=${header}`, "clone", repoURL, ".");
      expect(await git(clone, "rev-parse", "HEAD")).toBe(first);

      await writeFile(`${root}/README.md`, "private GRASP-08 incremental repository\n");
      await git(root, "add", "README.md");
      await git(root, "commit", "-m", "incremental");
      const second = await git(root, "rev-parse", "HEAD");
      await c.publish(finalizeEvent({ kind: KIND_REPO_STATE, content: "", created_at: createdAt + 2, tags: [["d", identifier], ["HEAD", "ref: refs/heads/main"], ["refs/heads/main", second]] }, sk));
      await waitForRefs(repoURL, proof);
      await git(root, "-c", `http.extraHeader=${header}`, "push", repoURL, "HEAD:refs/heads/main");
      await git(clone, "-c", `http.extraHeader=${header}`, "fetch", repoURL, "refs/heads/main:refs/remotes/origin/main");
      expect(await git(clone, "rev-parse", "refs/remotes/origin/main")).toBe(second);
    } finally {
      c.close();
      await Promise.all([rm(root, { recursive: true, force: true }), rm(clone, { recursive: true, force: true })]);
    }
  }, 120_000);
});
