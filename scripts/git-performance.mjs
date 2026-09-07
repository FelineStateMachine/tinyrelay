#!/usr/bin/env node
// Benchmark committed repository copies. Source checkouts are never changed.
import { execFile } from "node:child_process";
import { appendFile, mkdtemp, mkdir, readdir, rm, writeFile, rename } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve, join, basename } from "node:path";
import { promisify } from "node:util";
import WebSocket from "ws";
import { finalizeEvent, generateSecretKey, getPublicKey } from "nostr-tools/pure";
import { npubEncode } from "nostr-tools/nip19";
import { startGitSigningProxy } from "./git-signing-proxy.mjs";

const exec = promisify(execFile);
const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const relay = option("--relay", "ws://127.0.0.1:17447");
const reposDir = option("--repos-dir");
const outDir = resolve(option("--out", "artifacts/git-performance/run"));
const baseline = option("--baseline", "").replace(/\/$/, "");
const only = option("--only", "").split(",").filter(Boolean);
const privateRepos = args.includes("--private");
const repeats = Math.max(1, Number(option("--repeats", "3")));
if (!reposDir) throw Error("--repos-dir is required");
const httpURL = relay.replace(/^ws/, "http").replace(/\/$/, "");
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const report = {schema: 3, started: new Date().toISOString(), relay, baseline, repeats, privateRepos, results: [], failures: []};
await mkdir(outDir, {recursive: true});
const progressPath = join(outDir, "progress.jsonl");
await writeFile(progressPath, "");
const progress = async row => {
  const value = {at: new Date().toISOString(), ...row};
  console.log(JSON.stringify(value));
  await appendFile(progressPath, JSON.stringify(value) + "\n");
};
const save = async () => {
  const path = join(outDir, "git-performance.json");
  await writeFile(path + ".tmp", JSON.stringify(report, null, 2) + "\n");
  await rename(path + ".tmp", path);
};
const git = async (cwd, ...args) => (await exec("git", ["-C", cwd, ...args], {
  encoding: "utf8", maxBuffer: 16 * 1024 * 1024,
  env: {...process.env, GIT_TERMINAL_PROMPT: "0", GIT_AUTHOR_NAME: "tiny benchmark", GIT_AUTHOR_EMAIL: "benchmark@example.invalid", GIT_COMMITTER_NAME: "tiny benchmark", GIT_COMMITTER_EMAIL: "benchmark@example.invalid"},
})).stdout.trim();

class Client {
  constructor(url) {
    this.url = url;
    this.challenge = new Promise(resolve => { this.setChallenge = resolve; });
    this.ws = new WebSocket(url);
    this.waiters = new Map();
    this.connected = new Promise((resolve, reject) => {
      this.ws.once("open", resolve);
      this.ws.once("error", reject);
    });
    this.ws.on("message", raw => {
      let message;
      try { message = JSON.parse(raw); } catch { return; }
      if (message[0] === "AUTH") { this.setChallenge(message[1]); return; }
      const waiter = this.waiters.get(message[1]);
      if (!waiter) return;
      if (message[0] === "EVENT") waiter.events.push(message[2]);
      else if (message[0] === "EOSE") waiter.finish(null, waiter.events);
      else if (message[0] === "OK") waiter.finish(message[2] ? null : Error(message[3]), message);
      else if (message[0] === "CLOSED") waiter.finish(Error(message[2]));
    });
    this.ws.on("error", error => { for (const waiter of [...this.waiters.values()]) waiter.finish(error); });
    this.ws.on("close", () => { for (const waiter of [...this.waiters.values()]) waiter.finish(Error("websocket closed")); });
  }
  async request(message, key) {
    await this.connected;
    if (this.ws.readyState !== WebSocket.OPEN) throw Error("websocket is not open");
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => finish(Error("Nostr request timed out after 30 seconds")), 30000);
      const finish = (error, value) => {
        clearTimeout(timer);
        this.waiters.delete(key);
        if (message[0] === "REQ" && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(["CLOSE", key]));
        if (error) reject(error); else resolve(value);
      };
      this.waiters.set(key, {events: [], finish});
      this.ws.send(JSON.stringify(message));
    });
  }
  publish(event) { return this.request(["EVENT", event], event.id); }
  async authenticate(secret) {
    const challenge = await this.challenge;
    const proof = finalizeEvent({kind: 22242, created_at: Math.floor(Date.now()/1000), content: "",
      tags: [["relay", this.url], ["challenge", challenge]]}, secret);
    return this.request(["AUTH", proof], proof.id);
  }
  query(filter) {
    const id = Math.random().toString(36).slice(2);
    return this.request(["REQ", id, filter], id);
  }
  close() { this.ws.close(); }
}

const control = new Client(relay);
const probes = new Client(relay);
await Promise.all([control.connected, probes.connected]);
const probeSecret = generateSecretKey();
let sequence = 0;
async function probeSample(label) {
  const row = {at: new Date().toISOString(), phase: label};
  const event = finalizeEvent({kind: 1, created_at: Math.floor(Date.now()/1000), tags: [], content: `tiny benchmark ${sequence++}`}, probeSecret);
  for (const [operation, run] of [["publish", () => probes.publish(event)], ["query", async () => {
    const found = await probes.query({ids: [event.id], limit: 1});
    if (found.length !== 1 || found[0].id !== event.id) throw Error("probe event was not returned by query");
  }]]) {
    const start = performance.now();
    try { await run(); row[operation + "Ms"] = performance.now() - start; }
    catch (error) { row[operation + "Error"] = error.message; row[operation + "Ms"] = performance.now() - start; }
  }
  return row;
}

const refsFor = async directory => (await git(directory, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags"))
  .split("\n").filter(Boolean).map(line => line.split(" "));
async function verifyRemote(url, refs, head) {
  const output = (await exec("git", ["ls-remote", url], {encoding: "utf8", maxBuffer: 16*1024*1024})).stdout;
  const actual = new Map(output.trim().split("\n").filter(Boolean).map(line => { const [oid, ref] = line.split(/\s+/); return [ref, oid]; }));
  for (const [ref, oid] of refs) if (actual.get(ref) !== oid) throw Error(`remote ref mismatch: ${ref}`);
  const expectedHead = refs.find(([ref]) => ref === head)?.[1];
  if (!expectedHead || actual.get("HEAD") !== expectedHead) throw Error(`remote HEAD does not resolve to ${head}`);
}

async function benchmarkFixture(fixture, index) {
  const name = basename(fixture);
  const result = {fixture: name, phases: [], probes: [], status: "running"};
  report.results.push(result);
  const temporaries = [];
  const temp = async () => { const path = await mkdtemp(join(tmpdir(), "tiny-git-benchmark-")); temporaries.push(path); return path; };
  let active = "idle", stopProbes = false, loop, signingProxy;
  const phase = async (name, fn) => {
    active = name;
    const started = new Date().toISOString(), start = performance.now();
    await progress({fixture: result.fixture, phase: name, event: "start"});
    let error;
    try { return await fn(); } catch (failure) { error = failure; throw failure; }
    finally {
      const row = {name, started, finished: new Date().toISOString(), ms: performance.now() - start, ok: !error};
      if (error) row.error = error.message.slice(0, 2000);
      result.phases.push(row);
      await progress({fixture: result.fixture, phase: name, event: "end", ms: row.ms, ok: row.ok});
      await save();
      active = "between_phases";
    }
  };
  try {
    await progress({fixture: name, event: "fixture_start"});
    for (let i=0; i<50; i++) { result.probes.push(await probeSample("idle")); await sleep(20); }
    loop = (async () => { while (!stopProbes) { result.probes.push(await probeSample(active)); await sleep(50); } })();
    const head = await git(fixture, "symbolic-ref", "HEAD");
    const refs = await refsFor(fixture);
    result.head = head;
    result.refs = refs.length;
    const sk = generateSecretKey(), pubkey = getPublicKey(sk), id = `${name.replace(/\.git$/, "")}-${index}`;
    const repositoryPath = `/${npubEncode(pubkey)}/${encodeURIComponent(id)}.git`;
    const canonicalURL = httpURL + repositoryPath;
    let url = canonicalURL;
    result.url = canonicalURL;
    if (privateRepos) {
      await control.authenticate(sk);
      signingProxy = await startGitSigningProxy(httpURL, sk);
      url = signingProxy.url + repositoryPath;
      result.transport = "local NIP-98 signing proxy; request hash spooled on client";
    }
    const announceAt = Math.floor(Date.now()/1000);
    const announcement = finalizeEvent({kind: 30617, created_at: announceAt, content: "", tags: [["d", id], ["clone", canonicalURL], ["relays", relay], ["maintainers", pubkey], ...(privateRepos ? [["private", "true"]] : [])]}, sk);
    const makeState = (refs, created_at) => finalizeEvent({kind: 30618, created_at, content: "", tags: [["d", id], ["HEAD", `ref: ${head}`], ...refs]}, sk);
    await phase("announcement_ack", () => control.publish(announcement));
    const state = makeState(refs, announceAt+1);
    await phase("state_ack", () => control.publish(state));
    // --shared only uses immutable fixture objects; updates live in this copy.
    const source = await temp();
    await git(source, "clone", "--bare", "--shared", fixture, ".");
    const refspecs = ["refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"];
    const waitVisible = async event => {
      const until = Date.now()+30000;
      while (Date.now()<until) {
        if ((await control.query({ids:[event.id],limit:1})).length) return;
        await sleep(25);
      }
      throw Error("signed Git state did not become visible after transfer");
    };
    await phase("initial_push", () => git(source, "push", url, ...refspecs));
    await phase("promotion_wait", () => waitVisible(state));
    const baseURL = baseline ? `${baseline}/${encodeURIComponent(name.endsWith(".git") ? name : name+".git")}` : "";
    if (baseURL) await phase("baseline_initial_push", () => git(source, "push", baseURL, ...refspecs));
    const clones = new Map();
    for (const [prefix, target] of [["", url], ["baseline_", baseURL]]) {
      if (!target) continue;
      for (let i=0; i<repeats; i++) {
        const destination = await temp();
        await phase(`${prefix}fresh_clone_${i+1}`, () => git(destination, "clone", target, "."));
        const actualHead = await git(destination, "rev-parse", "HEAD");
        if (actualHead !== refs.find(([ref])=>ref===head)[1]) throw Error(`${prefix}clone checked out the wrong HEAD`);
        if (i===0) clones.set(prefix, destination);
        else await rm(destination, {recursive: true, force: true});
      }
      await phase(prefix+"verify", async () => { await verifyRemote(target, refs, head); await git(clones.get(prefix), "fsck", "--full"); });
      for (let i=0; i<repeats; i++) await phase(`${prefix}noop_fetch_${i+1}`, () => git(clones.get(prefix), "fetch", "origin"));
    }
    const parent = await git(source, "rev-parse", head);
    const tree = await git(source, "rev-parse", `${head}^{tree}`);
    const next = await git(source, "commit-tree", tree, "-p", parent, "-m", "tiny performance incremental commit");
    await git(source, "update-ref", head, next);
    const nextRefs = refs.map(([ref, oid]) => [ref, ref===head ? next : oid]);
    const nextState = makeState(nextRefs, Math.max(announceAt+2, Math.floor(Date.now()/1000)));
    await phase("incremental_state_ack", () => control.publish(nextState));
    await phase("incremental_push", () => git(source, "push", url, `${head}:${head}`));
    await phase("incremental_promotion_wait", () => waitVisible(nextState));
    if (baseURL) await phase("baseline_incremental_push", () => git(source, "push", baseURL, `${head}:${head}`));
    for (const [prefix, target] of [["",url], ["baseline_",baseURL]]) {
      if (!target) continue;
      await phase(prefix+"incremental_fetch", () => git(clones.get(prefix), "fetch", "origin", head));
      if (await git(clones.get(prefix), "rev-parse", "FETCH_HEAD") !== next) throw Error("incremental fetch returned wrong commit");
      for (const count of [2,4]) {
        const destinations = await Promise.all(Array.from({length:count},()=>temp()));
        await phase(`${prefix}concurrent_clone_${count}`, async () => {
          const started = performance.now();
          const runs = await Promise.allSettled(destinations.map(async destination => {
            await git(destination,"clone",target,".");
            return performance.now()-started;
          }));
          const failure = runs.find(run=>run.status==="rejected");
          if (failure) throw failure.reason;
          result.phases.push({name:`${prefix}concurrent_clone_${count}_individual`, durationsMs:runs.map(run=>run.value), ok:true});
        });
        await Promise.all(destinations.map(path=>rm(path,{recursive:true,force:true})));
      }
    }
    result.status="passed";
  } catch (error) {
    result.status="failed";
    result.error=error.message.slice(0,4000);
    report.failures.push({fixture:name,error:result.error});
  } finally {
    stopProbes=true;
    if (loop) await loop;
    if (signingProxy) await signingProxy.close();
    await Promise.all(temporaries.map(path=>rm(path,{recursive:true,force:true})));
    await progress({fixture:name,event:"fixture_end",status:result.status});
    await save();
  }
}

try {
  const fixtures = (await readdir(reposDir,{withFileTypes:true})).filter(entry=>entry.isDirectory() && (!only.length || only.includes(entry.name.replace(/\.git$/,""))))
    .map(entry=>join(resolve(reposDir),entry.name));
  const order=["nzip","bindws","diagramzip","strudel","doorbearer","atlas"];
  fixtures.sort((a,b)=>order.indexOf(basename(a).replace(/\.git$/,""))-order.indexOf(basename(b).replace(/\.git$/,"")));
  for (let i=0;i<fixtures.length;i++) await benchmarkFixture(fixtures[i],i);
} finally {
  control.close(); probes.close();
  report.finished=new Date().toISOString();
  await save();
}
console.log(JSON.stringify({results:report.results.map(result=>({fixture:result.fixture,status:result.status})),failures:report.failures.length}));
process.exitCode=report.failures.length?1:0;
