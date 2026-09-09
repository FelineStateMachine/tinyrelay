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

test("collaboration tools preserve repository, event and filter scope", async () => {
  const requests = [];
  const {tools} = await browser({path: "/r/work/tools", fetch: async url => {
    requests.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({items: [], item: {title: "Issue"}}));
  }});
  const repo = {owner: "a".repeat(64), repo: "notes"};
  for (const [name, method, extra] of [
    ["tiny.list_issues", "browseissues", {q: "bug", state: "resolved", limit: 5}],
    ["tiny.read_issue", "browseissue", {event: "b".repeat(64)}],
    ["tiny.list_pull_requests", "browsepulls", {state: "merged"}],
    ["tiny.read_pull_request", "browsepull", {event: "c".repeat(64)}]
  ]) {
    const tool = tools.get(name);
    assert.equal(tool.annotations.readOnlyHint, true);
    await tool.execute({...repo, ...extra});
    const url = requests.at(-1);
    assert.equal(url.pathname, "/r/work/webmcp/query");
    assert.equal(url.searchParams.get("method"), method);
    assert.deepEqual(JSON.parse(url.searchParams.get("params")), [{...repo, ...extra}]);
  }
});

test("approval tools list, read and open requests for a decision", async () => {
  const requests = [];
  const {tools, sandbox} = await browser({path: "/r/work/approvals", fetch: async url => {
    requests.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({items: [{id: "a".repeat(64), state: "open"}], counts: {open: 1}}));
  }});
  const list = tools.get("tiny.list_approvals");
  assert.equal(list.annotations.readOnlyHint, true);
  const listed = await list.execute({state: "open", limit: 5});
  assert.equal(requests.at(-1).pathname, "/r/work/webmcp/query");
  assert.equal(requests.at(-1).searchParams.get("method"), "browseapprovals");
  assert.deepEqual(JSON.parse(requests.at(-1).searchParams.get("params")), [{state: "open", limit: 5}]);
  assert.equal(listed.structuredContent.result.counts.open, 1);
  assert.equal(list.inputSchema.properties.state.enum.join(","), "open,answered,expired,all");
  const read = tools.get("tiny.read_approval");
  assert.equal(read.annotations.readOnlyHint, true);
  assert.equal(read.inputSchema.required.join(","), "id");
  await read.execute({id: "b".repeat(64)});
  assert.equal(requests.at(-1).searchParams.get("method"), "browseapproval");
  assert.deepEqual(JSON.parse(requests.at(-1).searchParams.get("params")), [{id: "b".repeat(64)}]);
  await tools.get("tiny.open_approvals").execute({});
  assert.equal(new URL(sandbox.opened).pathname, "/r/work/approvals");
  await tools.get("tiny.open_approvals").execute({id: "c".repeat(64)});
  const opened = new URL(sandbox.opened);
  assert.equal(opened.pathname, "/r/work/approvals");
  assert.equal(opened.searchParams.get("id"), "c".repeat(64));
  assert.equal(opened.searchParams.get("answer"), null);
});

test("job tools list and read long tasks through the session", async () => {
  const requests = [];
  const {tools} = await browser({path: "/r/work/tools", fetch: async url => {
    requests.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({items: [{id: "a".repeat(64), kind: 5001, state: "open", status: "processing"}], next_cursor: "",
      item: {id: "b".repeat(64), kind: 5002, state: "done"}, feedback: [{status: "processing"}], results: [{kind: 6002}]}));
  }});
  const list = tools.get("tiny.list_jobs");
  assert.equal(list.annotations.readOnlyHint, true);
  assert.equal(list.inputSchema.properties.state.enum.join(","), "open,done,all");
  assert.equal(list.inputSchema.properties.mine.type, "boolean");
  const listed = await list.execute({state: "open", mine: true, limit: 5});
  assert.equal(requests.at(-1).pathname, "/r/work/webmcp/query");
  assert.equal(requests.at(-1).searchParams.get("method"), "browsejobs");
  assert.deepEqual(JSON.parse(requests.at(-1).searchParams.get("params")), [{state: "open", mine: true, limit: 5}]);
  assert.equal(listed.structuredContent.result.items[0].status, "processing");
  const read = tools.get("tiny.read_job");
  assert.equal(read.annotations.readOnlyHint, true);
  assert.deepEqual([...read.inputSchema.required], ["id"]);
  const detail = await read.execute({id: "b".repeat(64)});
  assert.equal(requests.at(-1).searchParams.get("method"), "browsejob");
  assert.deepEqual(JSON.parse(requests.at(-1).searchParams.get("params")), [{id: "b".repeat(64)}]);
  assert.equal(detail.structuredContent.result.item.state, "done");
  assert.equal(detail.structuredContent.result.results[0].kind, 6002);
});

test("link, files and notification tools reach the new surfaces", async () => {
  const calls = [];
  const {tools, sandbox} = await browser({path: "/r/work/tools", signedFetch: async (url, method, body) => {
    calls.push({url, method, body: JSON.parse(body)});
    return new Response(JSON.stringify({result: true}), {status: 200});
  }});
  sandbox.localStorage = {getItem: key => ({"tiny.push": "https://push.example/x", "tiny.push.categories": "replies,relay"})[key] ?? null};
  await tools.get("tiny.open_link").execute({target: "web+nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2"});
  const opened = new URL(sandbox.opened);
  assert.equal(opened.pathname, "/r/work/open");
  assert.equal(opened.searchParams.get("target"), "web+nostr:npub1ttrypewl3au52wqux86r22yt506c077k3maj02a0jste97wrvd5sjfutc2");
  await tools.get("tiny.open_files").execute({});
  assert.equal(new URL(sandbox.opened).pathname, "/r/work/files");
  const status = await tools.get("tiny.read_notifications").execute({});
  assert.equal(status.structuredContent.result.enabled, true);
  assert.equal(status.structuredContent.result.categories.join(","), "replies,relay");
  assert.equal(status.structuredContent.result.permission, "unsupported");
  await tools.get("tiny.send_test_notification").execute({});
  assert.equal(calls[0].url, "/r/work/manage/rpc");
  assert.equal(calls[0].body.method, "notifytest");
});

test("agent tools list through the signer, read through the session and control through management", async () => {
  const calls = [], queries = [];
  const {tools} = await browser({path: "/r/work/manage/agents", fetch: async url => {
    queries.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({agent: {name: "hermes"}, events: []}));
  }, signedFetch: async (url, method, body) => {
    calls.push({url, method, body: JSON.parse(body)});
    return new Response(JSON.stringify({result: [{name: "hermes", paused: false}]}));
  }});
  const agent = "a".repeat(64);
  const list = tools.get("tiny.list_agents");
  assert.equal(list.annotations.readOnlyHint, true);
  const listed = await list.execute({});
  assert.equal(listed.structuredContent.result[0].name, "hermes");
  assert.deepEqual(calls[0].body, {method: "listagents", params: []});
  assert.equal(calls[0].url, "/r/work/manage/rpc");
  const read = await tools.get("tiny.read_agent").execute({agent});
  assert.equal(queries[0].pathname, "/r/work/webmcp/query");
  assert.equal(queries[0].searchParams.get("method"), "browseagent");
  assert.deepEqual(JSON.parse(queries[0].searchParams.get("params")), [{agent}]);
  assert.equal(read.structuredContent.result.agent.name, "hermes");
  for (const [name, method] of [["tiny.pause_agent", "pauseagent"], ["tiny.resume_agent", "resumeagent"], ["tiny.revoke_agent", "revokeagent"]]) {
    const tool = tools.get(name);
    assert.equal(tool.annotations.consequentialHint, true);
    assert.deepEqual([...tool.inputSchema.required], ["agent"]);
    await tool.execute({agent});
    assert.deepEqual(calls.at(-1).body, {method, params: [agent]});
  }
});

test("callback tools list through the signer and control by id", async () => {
  const calls = [];
  const {tools} = await browser({path: "/r/work/manage/agents", signedFetch: async (url, method, body) => {
    calls.push({url, method, body: JSON.parse(body)});
    return new Response(JSON.stringify({result: [{id: "cb-1", host: "hooks.example", paused: false}]}));
  }});
  const list = tools.get("tiny.list_callbacks");
  assert.equal(list.annotations.readOnlyHint, true);
  const listed = await list.execute({});
  assert.equal(listed.structuredContent.result[0].host, "hooks.example");
  assert.equal(calls[0].url, "/r/work/manage/rpc");
  assert.deepEqual(calls[0].body, {method: "listcallbacks", params: []});
  assert.equal(tools.get("tiny.add_callback"), undefined);
  for (const [name, method] of [["tiny.pause_callback", "pausecallback"], ["tiny.resume_callback", "resumecallback"], ["tiny.remove_callback", "removecallback"]]) {
    const tool = tools.get(name);
    assert.equal(tool.annotations.consequentialHint, true);
    assert.deepEqual([...tool.inputSchema.required], ["id"]);
    await tool.execute({id: "cb-1"});
    assert.deepEqual(calls.at(-1).body, {method, params: ["cb-1"]});
  }
});

test("wiki tools read pages and merge requests and open pages in the tenant", async () => {
  const requests = [];
  const {tools, sandbox} = await browser({path: "/r/work/wiki", fetch: async url => {
    requests.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({items: [{d: "release-notes-1-4"}], d: "release-notes-1-4", merge: {status: "open"}}));
  }});
  for (const [name, method, input] of [
    ["tiny.list_wiki", "browsewiki", {q: "release", limit: 10}],
    ["tiny.read_wiki_page", "browsewikipage", {d: "Release Notes 1.4", version: "b".repeat(64)}],
    ["tiny.read_merge_request", "browsewikimerge", {id: "c".repeat(64)}]
  ]) {
    const tool = tools.get(name);
    assert.equal(tool.annotations.readOnlyHint, true);
    const result = await tool.execute(input);
    const url = requests.at(-1);
    assert.equal(url.pathname, "/r/work/webmcp/query");
    assert.equal(url.searchParams.get("method"), method);
    assert.deepEqual(JSON.parse(url.searchParams.get("params")), [input]);
    assert.equal(result.structuredContent.result.d, "release-notes-1-4");
  }
  const open = tools.get("tiny.open_wiki_page");
  assert.equal(open.annotations.readOnlyHint, undefined);
  await open.execute({});
  assert.equal(new URL(sandbox.opened).pathname, "/r/work/wiki");
  await open.execute({d: "日本語 Article", merge: "c".repeat(64)});
  let opened = new URL(sandbox.opened);
  assert.equal(opened.pathname, "/r/work/wiki/" + encodeURIComponent("日本語 Article"));
  assert.equal(opened.searchParams.get("merge"), "c".repeat(64));
  await open.execute({d: "release-notes-1-4", edit: true, version: "b".repeat(64)});
  opened = new URL(sandbox.opened);
  assert.equal(opened.pathname, "/r/work/wiki/release-notes-1-4");
  assert.equal(opened.searchParams.get("edit"), "1");
  assert.equal(opened.searchParams.get("version"), "b".repeat(64));
});

test("room tools read rooms and threads, open rooms and post signed messages", async () => {
  const {finalizeEvent, generateSecretKey, verifyEvent} = await import("nostr-tools/pure");
  const requests = [], calls = [];
  const {tools, sandbox} = await browser({path: "/r/work/tools", fetch: async url => {
    requests.push(new URL(url, "https://tiny.example"));
    return new Response(JSON.stringify({items: [{id: "build"}]}));
  }, signedFetch: async (url, method, body, options) => {
    calls.push({url, method, body: JSON.parse(body), options});
    return new Response(JSON.stringify(calls.length > 1 ? {accepted: false, message: "restricted: members only"} : {accepted: true}));
  }});
  const secret = generateSecretKey();
  sandbox.nostr = {signEvent: async event => finalizeEvent(JSON.parse(JSON.stringify(event)), secret)};
  sandbox.NostrSigner = {verifyEvent};
  for (const [name, method, input] of [
    ["tiny.list_rooms", "browserooms", {limit: 5}],
    ["tiny.read_room", "browseroom", {id: "build", cursor: "c1"}],
    ["tiny.read_thread", "browsethread", {id: "build", event: "a".repeat(64)}]
  ]) {
    const tool = tools.get(name);
    assert.equal(tool.annotations.readOnlyHint, true);
    await tool.execute(input);
    const url = requests.at(-1);
    assert.equal(url.pathname, "/r/work/webmcp/query");
    assert.equal(url.searchParams.get("method"), method);
    assert.deepEqual(JSON.parse(url.searchParams.get("params")), [input]);
  }
  await tools.get("tiny.open_room").execute({id: "build"});
  assert.equal(new URL(sandbox.opened).pathname, "/r/work/rooms/build");
  await tools.get("tiny.open_room").execute({});
  assert.equal(new URL(sandbox.opened).pathname, "/r/work/rooms");
  const post = tools.get("tiny.post_message");
  assert.equal(post.annotations.consequentialHint, true);
  const result = await post.execute({room: "build", text: "hello", mentions: ["b".repeat(64), "b".repeat(64)]});
  assert.equal(calls[0].url, "/r/work/events");
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].options.contentType, "application/json");
  assert.equal(calls[0].body.kind, 9);
  assert.deepEqual(calls[0].body.tags, [["h", "build"], ["p", "b".repeat(64)]]);
  assert.equal(verifyEvent(calls[0].body), true);
  assert.equal(result.structuredContent.result.id, calls[0].body.id);
  await assert.rejects(post.execute({room: "build", text: "again"}), /members only/);
  await assert.rejects(post.execute({room: "build", text: "  "}), /Enter a message/);
});

test("the profile tool reads through the session and defaults to the signed-in key", async () => {
  const requests = [];
  const {tools} = await browser({path: "/r/work/profile", fetch: async url => { requests.push(new URL(url, "https://tiny.example")); return new Response(JSON.stringify({pubkey: "a".repeat(64), profile: {name: "dami"}, relays: []})); }});
  const tool = tools.get("tiny.read_profile");
  assert.equal(tool.annotations.readOnlyHint, true);
  const own = await tool.execute({});
  assert.equal(requests[0].searchParams.get("method"), "browseprofile");
  assert.deepEqual(JSON.parse(requests[0].searchParams.get("params")), [{}]);
  assert.equal(own.structuredContent.result.profile.name, "dami");
  await tool.execute({pubkey: "b".repeat(64)});
  assert.deepEqual(JSON.parse(requests[1].searchParams.get("params")), [{pubkey: "b".repeat(64)}]);
});

