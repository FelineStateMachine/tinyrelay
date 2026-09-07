#!/usr/bin/env node
// Publish the current Git repository to a tiny relay over GRASP-01.
//
//   node scripts/publish-repository.mjs --relay wss://relay.example --sec bunker://... [options]
//
// The script announces the repository (kind 30617), publishes the signed ref
// state (kind 30618) from the local branches and tags, then pushes through a
// local NIP-98 signing proxy. Run it again after new commits: the relay only
// accepts pushes that match the latest signed state.
//
// Options:
//   --relay URL         relay websocket URL (default: derived from --remote)
//   --remote NAME       git remote whose URL names the relay and identifier
//   --identifier NAME   repository identifier (default: remote path or directory name)
//   --sec VALUE         nsec, hex secret, or bunker:// URL; also NOSTR_SECRET_KEY
//   --name TEXT         repository display name for the announcement
//   --description TEXT  repository description for the announcement
//   --web URL           web page to advertise
//   --private           announce a private repository
//   --state-only        skip the announcement and only publish state and push
//   --force             force-update relay branches that diverged from local ones
import { execFile } from "node:child_process";
import { basename } from "node:path";
import { promisify } from "node:util";
import WebSocket from "ws";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";
import { npubEncode, decode } from "nostr-tools/nip19";
import { BunkerSigner, parseBunkerInput } from "nostr-tools/nip46";
import { generateSecretKey } from "nostr-tools/pure";
import { hexToBytes } from "@noble/hashes/utils.js";
import { startGitSigningProxy } from "./git-signing-proxy.mjs";

globalThis.WebSocket = WebSocket;
const exec = promisify(execFile);
const args = process.argv.slice(2);
const option = (name, fallback) => { const i = args.indexOf(name); return i >= 0 ? args[i + 1] : fallback; };
const flag = name => args.includes(name);
const git = async (...argv) => (await exec("git", argv, { env: { ...process.env, GIT_TERMINAL_PROMPT: "0" } })).stdout.trim();

async function signerFrom(value) {
  if (!value) throw Error("Pass --sec with an nsec, hex secret or bunker:// URL, or set NOSTR_SECRET_KEY.");
  if (value.startsWith("bunker://")) {
    const pointer = await parseBunkerInput(value);
    if (!pointer) throw Error("The bunker URL could not be parsed.");
    const bunker = BunkerSigner.fromBunker(generateSecretKey(), pointer);
    await bunker.connect();
    const pubkey = await bunker.getPublicKey();
    return { pubkey, sign: template => bunker.signEvent(template), close: () => bunker.close() };
  }
  const secret = value.startsWith("nsec1") ? decode(value).data : hexToBytes(value);
  return { pubkey: getPublicKey(secret), sign: async template => finalizeEvent(template, secret), close: async () => {} };
}

// publish sends one event and waits for OK, answering a NIP-42 AUTH
// challenge with the same signer when the relay asks for it.
function publish(relayURL, signer, event) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(relayURL);
    const timer = setTimeout(() => { socket.close(); reject(Error("relay did not acknowledge " + event.kind)); }, 30_000);
    const finish = (err) => { clearTimeout(timer); socket.close(); err ? reject(err) : resolve(); };
    let retried = false;
    socket.on("open", () => socket.send(JSON.stringify(["EVENT", event])));
    socket.on("error", finish);
    socket.on("message", async raw => {
      const message = JSON.parse(raw);
      if (message[0] === "AUTH" && !retried) {
        retried = true;
        const auth = await signer.sign({ kind: 22242, created_at: Math.floor(Date.now() / 1000), content: "", tags: [["relay", relayURL], ["challenge", message[1]]] });
        socket.send(JSON.stringify(["AUTH", auth]));
        socket.send(JSON.stringify(["EVENT", event]));
        return;
      }
      if (message[0] !== "OK" || message[1] !== event.id) return;
      if (message[2]) finish();
      else if (String(message[3]).startsWith("auth-required:") && !retried) return;
      else finish(Error(`relay rejected kind ${event.kind}: ${message[3]}`));
    });
  });
}

async function refsMatch(cloneURL, expected) {
  const response = await fetch(cloneURL + "/info/refs?service=git-upload-pack");
  if (!response.ok) return false;
  const text = await response.text();
  return expected.every(([ref, oid]) => text.includes(`${oid} ${ref}`));
}

async function main() {
  const remoteName = option("--remote");
  const remoteURL = remoteName ? await git("remote", "get-url", remoteName) : "";
  const relayURL = option("--relay") || (remoteURL && new URL(remoteURL).origin.replace(/^http/, "ws"));
  if (!relayURL) throw Error("Pass --relay or --remote.");
  const httpBase = relayURL.replace(/^ws/, "http").replace(/\/$/, "");
  const identifier = option("--identifier") || (remoteURL ? decodeURIComponent(basename(new URL(remoteURL).pathname).replace(/\.git$/, "")) : basename(await git("rev-parse", "--show-toplevel")));
  const signer = await signerFrom(option("--sec", process.env.NOSTR_SECRET_KEY));
  try {
    const npub = npubEncode(signer.pubkey);
    const path = `/${npub}/${encodeURIComponent(identifier)}.git`;
    const cloneURL = httpBase + path;
    const head = await git("symbolic-ref", "HEAD");
    const refs = (await git("for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")).split("\n").filter(Boolean).map(line => line.split(" "));
    if (!refs.length) throw Error("The repository has no branches or tags to publish.");
    const now = Math.floor(Date.now() / 1000);

    if (!flag("--state-only")) {
      const tags = [["d", identifier], ["clone", cloneURL], ["relays", relayURL], ["maintainers", signer.pubkey]];
      for (const name of ["name", "description", "web"]) if (option("--" + name)) tags.push([name, option("--" + name)]);
      if (flag("--private")) tags.push(["private", "true"]);
      await publish(relayURL, signer, await signer.sign({ kind: 30617, created_at: now, content: "", tags }));
      console.log("announced " + identifier);
    }
    await publish(relayURL, signer, await signer.sign({ kind: 30618, created_at: now + 1, content: "", tags: [["d", identifier], ["HEAD", "ref: " + head], ...refs] }));
    console.log(`published state for ${refs.length} refs, HEAD ${head}`);

    const proxy = await startGitSigningProxy(httpBase, signer.sign);
    try {
      await exec("git", ["push", ...(flag("--force") ? ["--force"] : []), proxy.url + path, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"], { stdio: "inherit", maxBuffer: 1 << 26 });
    } finally {
      await proxy.close();
    }
    for (let attempt = 0; attempt < 40 && !(await refsMatch(cloneURL, refs)); attempt++) await new Promise(r => setTimeout(r, 500));
    if (!(await refsMatch(cloneURL, refs))) throw Error("the relay has not advertised the pushed refs yet");
    console.log("clone URL: " + cloneURL);
  } finally {
    await signer.close();
  }
}

main().then(() => process.exit(0), err => { console.error(err.message); process.exit(1); });
