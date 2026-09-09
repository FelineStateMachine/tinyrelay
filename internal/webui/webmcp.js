(function () {
  "use strict";

  const context = document.modelContext || navigator.modelContext;
  const state = window.tinyWebMCP = {
    supported: typeof context?.registerTool === "function",
    registered: [], errors: [], lastOperation: "", ready: Promise.resolve()
  };
  const announce = () => window.dispatchEvent(new CustomEvent("tiny:webmcp", {detail: state}));
  if (!state.supported) { announce(); return; }

  const root = (location.pathname.match(/^\/r\/[^/]+/) || [""])[0];
  const local = path => root + path;
  const output = value => ({
    content: [{type: "text", text: JSON.stringify(value)}],
    structuredContent: {result: value}
  });
  const object = (properties = {}, required = []) => ({type: "object", properties, required, additionalProperties: false});
  const text = {type: "string"};
  const pubkey = {type: "string", pattern: "^[0-9a-f]{64}$"};
  const hash = {type: "string", pattern: "^[0-9a-f]{64}$"};
  const id = {type: "string", minLength: 1};
  const limit = {type: "integer", minimum: 1, maximum: 100};
  const view = {type: "string", enum: ["tree", "file", "history", "commit", "activity"]};
  const repository = {owner: pubkey, repo: id, ref: text, path: text, view};
  const reads = {readOnlyHint: true, untrustedContentHint: true};
  const changes = {consequentialHint: true, untrustedContentHint: true};
  const pending = [];

  async function responseJSON(response) {
    const body = await response.text();
    let value;
    try { value = JSON.parse(body); }
    catch { throw new Error(response.ok ? "The relay returned an invalid response." : body || `Request failed (${response.status}).`); }
    if (!response.ok || value?.error) throw new Error(value?.error || `Request failed (${response.status}).`);
    return value;
  }

  async function query(method, input, signal) {
    const params = new URLSearchParams({method, params: JSON.stringify([input])});
    const response = await fetch(local("/webmcp/query") + "?" + params, {
      method: "GET", credentials: "same-origin", signal
    });
    return responseJSON(response);
  }

  async function manage(method, params, signal) {
    if (!window.tinySignedFetch) throw new Error("The signer is still loading. Try again.");
    const response = await window.tinySignedFetch(local("/manage/rpc"), "POST",
      JSON.stringify({method, params}), {contentType: "application/json", signal});
    const value = await responseJSON(response);
    return value.result;
  }

  function register(name, description, schema, annotations, execute) {
    const tool = {
      name, description, inputSchema: schema, annotations,
      execute: async (input, options = {}) => {
        state.lastOperation = `${name}: running`;
        announce();
        try {
          const value = await execute(input, options.signal);
          state.lastOperation = `${name}: completed`;
          announce();
          return output(value);
        } catch (error) {
          state.lastOperation = `${name}: ${error.message}`;
          announce();
          throw error;
        }
      }
    };
    pending.push(Promise.resolve().then(() => context.registerTool(tool)).then(() => {
      state.registered.push(name);
    }, error => {
      state.errors.push(`${name}: ${error.message || error}`);
    }));
  }

  register("tiny.list_repositories", "List repositories visible to your account. Supports search and pagination.",
    object({cursor: text, limit, q: text}), reads, (input, signal) => query("browserepos", input, signal));
  register("tiny.read_repository", "Read a repository tree, source file, history, commit diff or activity. Use ref to select a branch, tag or commit.",
    object({...repository, offset: {type: "integer", minimum: 0}, limit}, ["owner", "repo"]), reads,
    (input, signal) => query("browserepo", input, signal));
  const collaborationList = object({owner: pubkey, repo: id, cursor: text, limit, q: text, label: text,
    state: {type: "string", enum: ["open", "resolved", "merged", "closed", "draft"]}}, ["owner", "repo"]);
  const collaborationDetail = object({owner: pubkey, repo: id, event: hash, cursor: text, limit}, ["owner", "repo", "event"]);
  register("tiny.list_issues", "List repository issues with search, status filters and pagination.",
    collaborationList, reads, (input, signal) => query("browseissues", input, signal));
  register("tiny.read_issue", "Read an issue, its replies and authorized status changes.",
    collaborationDetail, reads, (input, signal) => query("browseissue", input, signal));
  register("tiny.list_pull_requests", "List repository pull requests with search, status filters and pagination.",
    collaborationList, reads, (input, signal) => query("browsepulls", input, signal));
  register("tiny.read_pull_request", "Read a pull request, its replies, authorized updates and available diff.",
    collaborationDetail, reads, (input, signal) => query("browsepull", input, signal));
  register("tiny.list_files", "List stored files visible to your account.",
    object({cursor: text, limit, q: text}), reads, (input, signal) => query("browsefiles", input, signal));
  register("tiny.read_file", "Read a stored file's metadata and available preview by SHA-256 hash.",
    object({hash}, ["hash"]), reads, (input, signal) => query("browsefile", input, signal));
  register("tiny.read_status", "Read service health, storage and job status. Requires a signed-in owner or moderator session.",
    object(), reads, (input, signal) => query("browsestatus", input, signal));
  register("tiny.list_approvals", "List requests for a decision addressed to the signed-in person, with each request's asker, subject, expiry, state and answer, plus counts of open, answered and expired requests. Answering is the person's own signed action.",
    object({cursor: text, limit, state: {type: "string", enum: ["open", "answered", "expired", "all"]}}), reads,
    (input, signal) => query("browseapprovals", input, signal));
  register("tiny.read_approval", "Read one request for a decision by event id with every reaction and reply from the people asked.",
    object({id: hash}, ["id"]), reads, (input, signal) => query("browseapproval", input, signal));

  const readMethods = ["stats", "getpolicy", "listaudit", "listjobs", "listbackups", "listdumps", "deliverystatus", "storagestats", "gitstorage", "listconnections", "listmembers"];
  register("tiny.read_management", "Read relay configuration, jobs, backups, delivery status or members using your connected signer. gitstorage requires owner and repo.",
    object({method: {type: "string", enum: readMethods}, owner: pubkey, repo: id}, ["method"]), reads,
    (input, signal) => {
      if (!readMethods.includes(input.method)) throw new Error("Unsupported read operation.");
      if (input.method === "gitstorage") {
        if (!input.owner || !input.repo) throw new Error("Choose a repository owner and name.");
        return manage(input.method, [input.owner, input.repo], signal);
      }
      return manage(input.method, [], signal);
    });

  register("tiny.list_agents", "List agent grants: name, public key, owner, scope, paused or revoked state, expiry and last event time. Uses your connected signer; owner or moderator.",
    object(), reads, (input, signal) => manage("listagents", [], signal));
  register("tiny.read_agent", "Read one agent's grant and its ten newest events. Requires a signed-in owner or moderator session.",
    object({agent: pubkey}, ["agent"]), reads, (input, signal) => query("browseagent", input, signal));

  function open(path) {
    const url = new URL(local(path), location.href);
    location.assign(url.href);
    return {opened: url.href};
  }
  function repoURL(input) {
    const params = new URLSearchParams();
    for (const key of ["owner", "repo", "ref", "path", "view", "id"]) {
      if (input[key] !== undefined) params.set(key, input[key]);
    }
    return "/repo?" + params;
  }
  register("tiny.open_repository", "Open a repository in this tab for browsing code, history or activity.",
    object({...repository, id: hash, view: {type: "string", enum: ["tree", "file", "history", "commit", "activity", "issues", "prs", "issue", "pr"]}}, ["owner", "repo"]), {}, input => open(repoURL(input)));
  register("tiny.open_file", "Open a stored file by its SHA-256 hash in this tab. For repository source, use tiny.open_repository with view=file.",
    object({hash}, ["hash"]), {}, input => open("/file?hash=" + encodeURIComponent(input.hash)));
  register("tiny.open_status", "Open the relay health page in this tab: build versions, service status and browser tool readiness.", object(), {}, () => open("/manage/health"));
  register("tiny.open_files", "Open the Files page in this tab, where files and folders upload and shared items wait.", object(), {}, () => open("/files"));
  register("tiny.open_approvals", "Open the Approvals page in this tab, where requests for a decision wait. Pass id to focus one request; the person answers with a signed tap.",
    object({id: hash}), {}, input => open("/approvals" + (input.id ? "?id=" + encodeURIComponent(input.id) : "")));
  register("tiny.open_link", "Open a nostr link in this tab: an npub, nprofile, note, nevent, naddr or 64-character event id, with or without a nostr: or web+nostr: prefix.",
    object({target: {type: "string", minLength: 1, maxLength: 512}}, ["target"]), {}, input => open("/open?target=" + encodeURIComponent(input.target)));
  register("tiny.read_notifications", "Read whether this device receives relay notifications, which categories it chose and the browser permission state.",
    object(), {readOnlyHint: true}, () => {
      const categories = (localStorage.getItem("tiny.push.categories") || "").split(",").filter(Boolean);
      return {
        supported: "PushManager" in window && "Notification" in window,
        permission: typeof Notification === "function" ? Notification.permission : "unsupported",
        enabled: Boolean(localStorage.getItem("tiny.push")),
        categories: categories.length ? categories : ["messages", "replies", "mentions", "approvals", "relay"],
        note: "Enabling notifications needs a person to press Enable on this device under Inbox."
      };
    });

  function control(name, description, schema, method, params) {
    register(name, description, schema, changes, (input, signal) => manage(method, params(input), signal));
  }
  control("tiny.send_test_notification", "Send the relay's test notice to the owner's inbox and enabled devices. Owner only.", object(), "notifytest", () => []);
  control("tiny.run_job", "Queue an existing job to run now. Inspect job status to check its completion.",
    object({id}, ["id"]), "runjob", input => [input.id]);
  control("tiny.add_job", "Add a background job. every is the interval in hours; 0 runs once. Pull and push jobs need ws:// or wss:// relay URLs; pull may instead discover relays for a public key. Dump and backup jobs may omit relays.",
    object({id, kind: {type: "string", enum: ["pull", "push", "import", "mirror", "dump", "backup"]},
      relays: {type: "array", items: text}, filter: {...text, description: "Nostr filter encoded as a JSON object string."},
      every: {type: "integer", minimum: 0, description: "Interval in hours. Zero runs once."}, discoverPubKey: pubkey}, ["id", "kind"]),
    "addjob", input => [input]);
  control("tiny.remove_job", "Remove a job and cancel its pending runs.", object({id}, ["id"]), "removejob", input => [input.id]);
  control("tiny.pause_agent", "Pause an agent. Its grant stays and the relay rejects its events until it is resumed.",
    object({agent: pubkey}, ["agent"]), "pauseagent", input => [input.agent]);
  control("tiny.resume_agent", "Resume a paused agent so it may publish again.",
    object({agent: pubkey}, ["agent"]), "resumeagent", input => [input.agent]);
  control("tiny.revoke_agent", "Revoke an agent's grant. The agent loses its role at once; only a new grant restores it.",
    object({agent: pubkey}, ["agent"]), "revokeagent", input => [input.agent]);
  control("tiny.backup_now", "Queue a backup of relay data. Inspect jobs and backups to check completion.", object(), "backupnow", () => []);
  control("tiny.dump_now", "Queue an event export. Inspect jobs and dumps to check completion.", object(), "dumpnow", () => []);
  control("tiny.set_connections", "Replace the relay's connection list. Read the current list before editing it.",
    object({connections: {type: "array", items: {type: "object"}}}, ["connections"]), "setconnections", input => [input.connections]);
  control("tiny.set_policy", "Apply a partial relay policy update. Read the current policy before changing access, delivery or features. Requires the owner signer.",
    object({patch: {type: "object", minProperties: 1}}, ["patch"]), "setpolicy", input => [input.patch]);

  state.ready = Promise.all(pending).then(() => { state.registered.sort(); announce(); });
})();
