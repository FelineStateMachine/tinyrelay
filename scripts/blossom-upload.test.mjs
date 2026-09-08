import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {createServer} from "node:http";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("../internal/webui/blossom-upload.js", import.meta.url), "utf8");
const sandbox = {crypto: webcrypto, Blob, ArrayBuffer, Uint8Array, AbortController, Map, setTimeout, clearTimeout, URL};
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox);
const upload = sandbox.TinyBlossomUpload.upload;
const digest = async bytes => Buffer.from(await webcrypto.subtle.digest("SHA-256", bytes)).toString("hex");
const response = (status, headers = {}, body = null) => ({
  status, ok: status >= 200 && status < 300,
  headers: {get(name) { return headers[name.toLowerCase()] ?? null; }},
  async json() { if (body === null) throw Error("no body"); return body; }
});

test("uses BUD-14 PATCH chunks and signs each chunk payload", async () => {
  const bytes = new TextEncoder().encode("abcdefghij");
  const hash = await digest(bytes);
  const calls = [];
  const result = await upload(bytes, {
    url: "https://blossom.example/upload",
    hash,
    type: "text/plain",
    chunkSize: 4,
    fetch: async (url, init) => {
      calls.push({url, init});
      return calls.length === 1 ? response(204, {allow: "GET, PUT, PATCH"}) :
        calls.length === 4 ? response(201, {}, {sha256: hash, size: bytes.length}) : response(204);
    },
    authorize: async (url, method, body) => "Nostr " + await digest(body)
  });
  assert.equal(result.hash, hash);
  assert.equal(calls.length, 4);
  assert.equal(calls[1].init.method, "PATCH");
  assert.equal(calls[1].init.headers["upload-offset"], "0");
  assert.equal(calls[2].init.headers["upload-offset"], "4");
  assert.equal(calls[3].init.headers["content-length"], undefined);
  assert.equal(calls[1].init.headers["content-type"], "application/octet-stream");
  assert.match(calls[1].init.headers.authorization, new RegExp((await digest(bytes.slice(0, 4)))));
});

test("falls back to BUD-13 PUT when PATCH is not advertised", async () => {
  const bytes = new TextEncoder().encode("fallback");
  const hash = await digest(bytes);
  const calls = [];
  const result = await upload(bytes, {
    url: "https://blossom.example/upload",
    hash,
    type: "text/plain",
    fetch: async (url, init) => {
      calls.push({url, init});
      return calls.length === 1 ? response(204, {allow: "GET, PUT"}) : response(201, {}, {sha256: hash, size: bytes.length});
    }
  });
  assert.equal(result.descriptor.sha256, hash);
  assert.equal(calls[1].init.method, "PUT");
  assert.deepEqual([...calls[1].init.body], [...bytes]);
  assert.equal(calls[1].init.headers["content-type"], "text/plain");
});

test("retries transient chunks, reports local state, and can be canceled", async () => {
  const bytes = new Uint8Array([1, 2, 3, 4]);
  const hash = await digest(bytes);
  let attempts = 0;
  const states = [];
  const task = upload(bytes, {
    url: "https://blossom.example/upload", hash, chunkSize: 2, retries: 1,
    fetch: async (url, init) => {
      if (init.method === "OPTIONS") return response(204, {allow: "PATCH"});
      attempts++;
      if (attempts === 1) return response(503);
      return response(204);
    },
    onState: state => states.push(state)
  });
  await assert.rejects(task, /without completion/);
  assert.equal(attempts, 5);
  assert.equal(states.at(-1).status, "uploaded");

  const canceled = upload(bytes, {url: "https://blossom.example/upload", hash, fetch: async (url, init) => {
    if (init.signal.aborted) throw Object.assign(new Error("aborted"), {name: "AbortError"});
    return new Promise((resolve, reject) => init.signal.addEventListener("abort", () => reject(Object.assign(new Error("aborted"), {name: "AbortError"}))));
  }});
  canceled.cancel();
  await assert.rejects(canceled, error => error.name === "AbortError");
});

test("resumes recent local chunks, rejects stale state, and checks the descriptor", async () => {
  const bytes = new Uint8Array([7, 8, 9, 10]);
  const hash = await digest(bytes);
  const state = new Map([[0, {offset: 0, length: 2, status: 204}]]);
  state.meta = {hash, size: bytes.length, chunkSize: 2, url: "https://blossom.example/tenant/" + hash, type: "application/octet-stream", lastAcceptedAt: Date.now()};
  const offsets = [];
  await upload(bytes, {url: "https://blossom.example/tenant/" + hash, hash, chunkSize: 2, state,
    fetch: async (url, init) => {
      if (init.method === "OPTIONS") return response(204, {allow: "PATCH"});
      offsets.push(init.headers["upload-offset"]);
      return offsets.length === 1 ? response(201, {}, {sha256: hash, size: bytes.length}) : response(204);
    }
  });
  assert.deepEqual(offsets, ["2"]);

  await assert.rejects(upload(bytes, {url: "https://blossom.example", hash, probe: false,
    fetch: async () => response(201, {}, {sha256: "0".repeat(64), size: bytes.length})
  }), /invalid Blossom upload descriptor/);
});

test("keeps the response body readable after headers arrive", async () => {
  const bytes = new Uint8Array([11, 12]);
  const hash = await digest(bytes);
  const server = createServer((request, reply) => {
    if (request.method === "OPTIONS") { reply.writeHead(204, {Allow: "PATCH"}); return reply.end(); }
    reply.writeHead(201, {"content-type": "application/json"});
    setTimeout(() => reply.end(JSON.stringify({sha256: hash, size: bytes.length})), 20);
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address();
    const result = await upload(bytes, {url: `http://127.0.0.1:${address.port}`, hash, chunkSize: bytes.length, timeoutMs: 1000, fetch});
    assert.equal(result.descriptor.sha256, hash);
  } finally { await new Promise(resolve => server.close(resolve)); }
});
