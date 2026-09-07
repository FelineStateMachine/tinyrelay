import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const script = await readFile(new URL("../internal/webui/webmcp.js", import.meta.url), "utf8");

async function browser({path = "/tools", failRegistration = "", fetch, signedFetch} = {}) {
  const tools = new Map();
  const sandbox = {
    document: {modelContext: {registerTool(tool) {
      if (tool.name === failRegistration) return Promise.reject(new Error("Registration denied"));
      tools.set(tool.name, tool);
    }}},
    navigator: {}, URL, URLSearchParams, CustomEvent,
    location: {pathname: path, href: "https://tiny.example" + path,
      assign(url) { sandbox.opened = url; }},
    dispatchEvent() {}, fetch,
    tinySignedFetch: signedFetch
  };
  sandbox.window = sandbox;
  vm.runInNewContext(script, sandbox);
  await sandbox.tinyWebMCP.ready;
  return {tools, sandbox};
}

test("read tools keep tenant scope and return backend data", async () => {
  let request;
  const {tools} = await browser({path: "/r/work/tools", fetch: async (url, options) => {
    request = {url, options};
    return new Response(JSON.stringify({items: [{identifier: "notes"}]}));
  }});
  const result = await tools.get("tiny.list_repositories").execute({q: "notes"});
  const url = new URL(request.url, "https://tiny.example");
  assert.equal(url.pathname, "/r/work/webmcp/query");
  assert.equal(request.options.method, "GET");
  assert.equal(url.searchParams.get("method"), "browserepos");
  assert.deepEqual(JSON.parse(url.searchParams.get("params")), [{q: "notes"}]);
  assert.equal(result.structuredContent.result.items[0].identifier, "notes");
});

test("backup and policy controls await signed JSON RPC and preserve parameter types", async () => {
  const requests = [];
  let finish;
  const completed = new Promise(resolve => { finish = resolve; });
  const {tools, sandbox} = await browser({signedFetch: async (url, method, body, options) => {
    requests.push({url, method, body: JSON.parse(body), options});
    await completed;
    return new Response(JSON.stringify({result: {id: "backup-1"}}));
  }});
  const pending = tools.get("tiny.backup_now").execute({});
  assert.equal(sandbox.tinyWebMCP.lastOperation, "tiny.backup_now: running");
  finish();
  const result = await pending;
  assert.equal(result.structuredContent.result.id, "backup-1");
  assert.deepEqual(requests[0].body, {method: "backupnow", params: []});
  assert.equal(requests[0].options.contentType, "application/json");
  await tools.get("tiny.set_policy").execute({patch: {delivery: {enabled: true}}});
  assert.deepEqual(requests[1].body.params, [{delivery: {enabled: true}}]);
  await tools.get("tiny.set_connections").execute({connections: []});
  assert.deepEqual(requests[2].body.params, [[]]);
});

test("server refusal fails the tool and is visible in operation status", async () => {
  const {tools, sandbox} = await browser({signedFetch: async () =>
    new Response(JSON.stringify({error: "owner required"}), {status: 403})});
  await assert.rejects(tools.get("tiny.set_policy").execute({patch: {writes: "owner"}}), /owner required/);
  assert.match(sandbox.tinyWebMCP.lastOperation, /owner required/);
});

test("repository storage reads pass the selected owner and repository", async () => {
  let request;
  const {tools} = await browser({signedFetch: async (_, __, body) => {
    request = JSON.parse(body);
    return new Response(JSON.stringify({result: {bytes: 123}}));
  }});
  const read = tools.get("tiny.read_management");
  await assert.rejects(read.execute({method: "gitstorage"}), /repository owner and name/);
  await read.execute({method: "gitstorage", owner: "a".repeat(64), repo: "notes"});
  assert.deepEqual(request.params, ["a".repeat(64), "notes"]);
});

test("navigation actually opens the selected repository without changing tenant", async () => {
  const {tools, sandbox} = await browser({path: "/r/work/tools"});
  await tools.get("tiny.open_repository").execute({owner: "a".repeat(64), repo: "notes", path: "hello world.go", view: "file"});
  const url = new URL(sandbox.opened);
  assert.equal(url.pathname, "/r/work/repo");
  assert.equal(url.searchParams.get("path"), "hello world.go");
  assert.equal(url.searchParams.get("view"), "file");
});

test("registration failures stay visible while other tools remain available", async () => {
  const {sandbox} = await browser({failRegistration: "tiny.backup_now"});
  assert.equal(sandbox.tinyWebMCP.errors.length, 1);
  assert.match(sandbox.tinyWebMCP.errors[0], /tiny.backup_now: Registration denied/);
  assert.ok(sandbox.tinyWebMCP.registered.includes("tiny.read_status"));
  assert.ok(!sandbox.tinyWebMCP.registered.includes("tiny.backup_now"));
});

test("cancellation reaches the fetch request", async () => {
  const controller = new AbortController();
  let received;
  const {tools} = await browser({fetch: async (_, options) => {
    received = options.signal;
    return new Response("{}");
  }});
  await tools.get("tiny.list_files").execute({}, {signal: controller.signal});
  assert.equal(received, controller.signal);
});
