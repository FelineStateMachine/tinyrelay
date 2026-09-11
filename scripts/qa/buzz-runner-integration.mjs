#!/usr/bin/env node
// End-to-end optional-agent check. All state, keys and processes are local.
import {finalizeEvent, getPublicKey} from 'nostr-tools/pure';
import {createHash, randomUUID} from 'node:crypto';
import {mkdtemp, rm} from 'node:fs/promises';
import {createServer} from 'node:net';
import {tmpdir} from 'node:os';
import {dirname, resolve} from 'node:path';
import {spawn, spawnSync} from 'node:child_process';

const root = resolve(dirname(new URL(import.meta.url).pathname), '../..');
const binary = process.env.TINY_QA_BINARY || '/tmp/tiny-buzz-qa';
const reservation = createServer();
await new Promise((resolve,reject)=>{reservation.once('error',reject);reservation.listen(0,'127.0.0.1',resolve);});
const port = reservation.address().port;
await new Promise(resolve=>reservation.close(resolve));
const base = `http://127.0.0.1:${port}`;
const ws = `ws://127.0.0.1:${port}`;
const keys = Object.fromEntries(['owner', 'agent'].map((name, i) => [name, Uint8Array.from(Buffer.from((i + 1).toString(16).padStart(64, '0'), 'hex'))]));
const pub = Object.fromEntries(Object.entries(keys).map(([name, key]) => [name, getPublicKey(key)]));
const data = await mkdtemp(`${tmpdir()}/tiny-buzz-runner-`);
let relay, runner;
try {
  requireBinary();
  run([binary, 'tenant', 'create', '--data-dir', data, '--name', 'main', '--owner', pub.owner]);
  relay = spawn(binary, ['serve', '--data-dir', data, '--default-tenant', 'main', '--listen', `127.0.0.1:${port}`, '--allow-private-relays'], {stdio: ['ignore', 'ignore', 'pipe']});
  await waitFor(`${base}/readyz`);
  await publish('owner', 0, JSON.stringify({name: 'QA owner'}));
  await publish('owner', 30392, '', [['d', pub.agent], ['p', pub.agent], ['name', 'QA agent'], ['room', 'work'], ['room', 'cancel'], ['room', 'crash'], ['k', '0'], ['k', '9'], ['k', '5000'], ['k', '6000'], ['k', '7000'], ['k', '7'], ['k', '1111'], ['k', '20001'], ['jobs', 'both'], ['expiration', String(Math.floor(Date.now() / 1000) + 3600)]]);
  await publish('owner', 9007, '', [['h', 'work'], ['name', 'QA room'], ['visibility', 'open']]);
  await publish('owner', 9000, '', [['h', 'work'], ['p', pub.agent, 'member']]);
  for (const room of ['cancel', 'crash']) {
    await publish('owner', 9007, '', [['h', room], ['name', `QA ${room} room`], ['visibility', 'open']]);
    await publish('owner', 9000, '', [['h', room], ['p', pub.agent, 'member']]);
  }
  const acp = resolve(root, 'scripts/qa/buzz-runner-acp.mjs');
  runner = startRunner(acp, `${data}/queue.json`);
  const first = await publish('owner', 9, 'Please publish the preview.', [['h', 'work'], ['p', pub.agent]]);
  const request = await waitForEvent({kinds: [5000], '#h': ['work'], '#p': [pub.agent]});
  await waitForEvent({kinds:[7000], '#h':['work'], '#e':[request.id]}, event=>event.tags.some(tag=>tag[0]==='status'&&tag[1]==='processing'));
  const approval = await waitForEvent({kinds: [9], '#h': ['work'], '#p': [pub.owner]}, event => event.tags.some(tag => tag[0] === 'request' && tag[1] === 'approve'));
  assert(approval.tags.some(tag=>tag[0]==='expiration'&&Number(tag[1])>Math.floor(Date.now()/1000)), 'approval has no bounded expiry');
  await publish('owner', 7, '+', [['e', approval.id], ['p', pub.agent], ['k', '9'], ['h', 'work']]);
  const result = await waitForEvent({kinds: [6000], '#h': ['work'], '#e': [request.id]});
  const reply = await waitForEvent({kinds: [9], '#h': ['work'], '#e': [first.id], '#p': [pub.owner]}, event => event.pubkey === pub.agent && event.content === 'Preview published.');
  assert(result.content === 'Preview published.', `unexpected result: ${result.content}`);
  assert(reply.content === 'Preview published.', `unexpected reply: ${reply.content}`);
  await stopChild(runner);
  runner = startRunner(acp, `${data}/cancel-queue.json`, {FAKE_ACP_MODE: 'wait'}, 'cancel');
  const cancelMention = await publish('owner', 9, 'Start a task I will cancel.', [['h', 'cancel'], ['p', pub.agent]]);
  const cancelRequest = await waitForEvent({kinds: [5000], '#h': ['cancel'], '#p': [pub.agent]}, event => event.content.includes('Start a task'));
  const cancelApproval = await waitForEvent({kinds: [9], '#h': ['cancel'], '#p': [pub.owner]}, event => event.created_at >= cancelRequest.created_at && event.tags.some(tag => tag[0] === 'request' && tag[1] === 'approve'));
  await publish('owner', 9, '/cancel', [['h', 'cancel'], ['p', pub.agent]]);
  const canceled = await waitForEvent({kinds: [7000], '#h': ['cancel'], '#e': [cancelRequest.id]}, event => event.tags.some(tag => tag[0] === 'status' && tag[1] === 'error'));
  assert(canceled.content === 'Canceled', `unexpected cancellation: ${canceled.content}`);
  await stopChild(runner);
  const marker = `${data}/crash-once.marker`;
  runner = startRunner(acp, `${data}/crash-queue.json`, {FAKE_ACP_MODE: 'crash-once', FAKE_ACP_MARKER: marker}, 'crash');
  const crashedMention = await publish('owner', 9, 'Crash once.', [['h', 'crash'], ['p', pub.agent]]);
  const crashedRequest = await waitForEvent({kinds: [5000], '#h': ['crash'], '#p': [pub.agent]}, event => event.content.includes('Crash once'));
  const crashed = await waitForEvent({kinds: [7000], '#h': ['crash'], '#e': [crashedRequest.id]}, event => event.tags.some(tag => tag[0] === 'status' && tag[1] === 'error'));
  assert(crashed.content.includes('Agent stopped'), `missing crash report: ${crashed.content}`);
  const retryMention = await publish('owner', 9, 'Recover after crash.', [['h', 'crash'], ['p', pub.agent]]);
  const retryRequest = await waitForEvent({kinds: [5000], '#h': ['crash'], '#p': [pub.agent]}, event => event.content.includes('Recover after crash'));
  const retryApproval = await waitForEvent({kinds: [9], '#h': ['crash'], '#p': [pub.owner]}, event => event.created_at >= retryRequest.created_at && event.tags.some(tag => tag[0] === 'request' && tag[1] === 'approve'));
  await publish('owner', 7, '+', [['e', retryApproval.id], ['p', pub.agent], ['k', '9'], ['h', 'crash']]);
  const retryResult = await waitForEvent({kinds: [6000], '#h': ['crash'], '#e': [retryRequest.id]});
  assert(retryResult.content === 'Preview published.', `unexpected recovery result: ${retryResult.content}`);
  console.log(JSON.stringify({ok: true, request: request.id, approval: approval.id, result: result.id, reply: reply.id, canceled: cancelMention.id, crash: crashedMention.id, recovered: retryMention.id}));
} finally {
  if (runner) runner.kill('SIGTERM');
  if (relay) relay.kill('SIGTERM');
  await Promise.all([exitAfter(runner), exitAfter(relay)]);
  await rm(data, {recursive: true, force: true});
}

function startRunner(acp, state, extra = {}, room = 'work') {
  return spawn(binary, ['agent', '--relay', ws, '--room', room, '--command', process.execPath, '--arg', acp, '--state', state, '--cwd', root], {env: {...process.env, TINY_AGENT_KEY: Buffer.from(keys.agent).toString('hex'), ...extra}, stdio: ['ignore', 'pipe', 'pipe']});
}
async function stopChild(child) {
  child.kill('SIGTERM');
  await exitAfter(child);
}

function requireBinary() {
  const result = spawnSync(binary, ['version'], {encoding: 'utf8'});
  if (result.status !== 0) throw new Error(`build ${binary} first`);
}
function run(args) {
  const result = spawnSync(args[0], args.slice(1), {cwd: root, encoding: 'utf8'});
  if (result.status !== 0) throw new Error(result.stderr || result.stdout || `command failed: ${args.join(' ')}`);
}
async function waitFor(url) {
  for (let i = 0; i < 100; i++) {
    try { const response = await fetch(url); if (response.ok) return; } catch {}
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  throw new Error(`timed out waiting for ${url}`);
}
async function signed(path, method, value, who) {
  const body = JSON.stringify(value);
  const auth = finalizeEvent({kind: 27235, created_at: Math.floor(Date.now() / 1000), tags: [['u', base + path], ['method', method], ['payload', createHash('sha256').update(body).digest('hex')], ['nonce', randomUUID()]], content: ''}, keys[who]);
  const response = await fetch(base + path, {method, headers: {'content-type': 'application/json', authorization: `Nostr ${Buffer.from(JSON.stringify(auth)).toString('base64')}`}, body});
  const result = await response.json();
  if (!response.ok || result.error || result.accepted === false) throw new Error(JSON.stringify(result));
  return result;
}
async function publish(who, kind, content, tags = []) {
  const event = finalizeEvent({kind, content, tags, created_at: Math.floor(Date.now() / 1000)}, keys[who]);
  await signed('/events', 'POST', event, who);
  return event;
}
async function query(filter) { return await signed('/query', 'POST', [filter], 'owner'); }
async function waitForEvent(filter, predicate = () => true) {
  for (let i = 0; i < 160; i++) {
    const events = await query(filter);
    const match = events.filter(predicate);
    if (match.length) return match.sort((a, b) => b.created_at - a.created_at || b.id.localeCompare(a.id))[0];
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`timed out waiting for event ${JSON.stringify(filter)}`);
}
function assert(condition, message) { if (!condition) throw new Error(message); }
function exitAfter(child) {
 if(!child || child.exitCode!==null || child.signalCode!==null) return Promise.resolve();
 return new Promise(resolve=>{const timeout=setTimeout(()=>child.kill('SIGKILL'),3000); child.once('exit',()=>{clearTimeout(timeout);resolve();});});
}
