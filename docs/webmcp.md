# Browser tools

Every page registers tools for your browser agent; **Manage > Health** reports whether they are ready, and Chrome's Application panel lists them with their calls. The agent can browse repositories, files, issues and pull requests, read group chat rooms and threads, list and read wiki pages and merge requests, list the requests that wait for your decision, review access requests, follow long tasks, inspect service status, manage background jobs, create backups, update relay settings, open Nostr links, the Files page, rooms, wiki pages and the Approvals page, read this device's notification state and, as the owner, send a test notification. Social feeds and one-to-one conversations remain available through their pages and Nostr clients; the browser-agent bridge does not expose query tools for them yet. The MCP endpoint supports room chat tools, but it does not currently provide Social or direct-message read tools. Turning notifications on stays a manual step, since the browser asks the person for permission.

## Chat rooms

Rooms have their own tools. `tiny.list_rooms` lists the rooms you can see, `tiny.read_room` reads a room's members and newest messages, `tiny.read_thread` reads a thread's root and replies, and `tiny.open_room` opens a room in this tab. `tiny.post_message` posts a chat message to a room; it signs the message with your connected signer and lists mentioned keys as `p` tags so the relay notifies them.

## Wiki

Four tools cover the wiki. `tiny.list_wiki` lists pages with their shown version, version count and open merge requests; `q` searches titles and summaries and `author` prefers that key's versions. `tiny.read_wiki_page` returns one page with its content, links, every version, merge requests and redirects; `author` or `version` picks the version shown. `tiny.read_merge_request` returns one merge request with its answer, the proposed version and the destination author's current version. `tiny.open_wiki_page` opens the page list, one page, one version, the editor or the compare view for a merge request. The tools take a page name or title and normalize it the way the relay does. Publishing a version and answering a merge request stay signed actions on the page.

## Profile

`tiny.read_profile` returns a profile as this relay holds it, with the write relays from that key's relay list; without `pubkey` it reads the signed-in person's own. Publishing a profile stays a signed action on the Profile page.

## Approvals

Three tools cover requests for a decision. `tiny.list_approvals` lists the requests addressed to the signed-in person with each one's asker, subject, expiry, state and answer, plus counts of open, answered and expired requests; `state` narrows the list to `open`, `answered`, `expired` or `all`. `tiny.read_approval` returns one request by event id with every reaction and reply from the people asked. `tiny.open_approvals` opens the Approvals page, focused on one request when `id` is given. Answering stays a manual step: the person approves, denies or replies with their own signer. To make a request, publish an event as described in [Asking a person](agents.md#asking-a-person).

## Long tasks

Two tools follow [long tasks](agents.md#long-tasks). `tiny.list_jobs` lists the job requests visible to the signed-in account with each one's kind, requester, inputs, newest feedback status and result; `state` narrows the list to `open`, `done` or `all`, and `mine` lists only your own requests. `tiny.read_job` returns one request by event id with its feedback timeline and results. Requesting and answering stay signed actions through the relay's [MCP](mcp.md#long-tasks) tools or a relay connection.

Choose **Sign in** in the footer, or follow a sign-in link on a protected page, and connect your Nostr signer. Reading protected information uses your browser session. Management actions request a signature and follow your account permissions. The Account page holds the signed-in key's profile, relay lists and Chat presence preference; use the page directly because there is no browser-agent mutation tool for those personal settings. A queued backup or job has not finished until its status says so.

In Chrome, enable **WebMCP for testing** at `chrome://flags/#enable-webmcp-testing` and relaunch. Other browsers can use the same pages and forms. See [Chrome's WebMCP guide](https://developer.chrome.com/docs/ai/webmcp) for availability.

Read the current policy or connection list before changing settings. Job intervals are measured in hours; zero runs once.

## Agents

The owner and moderators can also manage [agent grants](agents.md) through the browser agent:

| Tool | What it does |
| --- | --- |
| `tiny.list_agents` | Lists every agent with its name, public key, owner, scope, state, expiry and last event. Read-only; uses the connected signer. |
| `tiny.read_agent` | Reads one agent's grant and its 10 newest events. Read-only; uses the browser session. |
| `tiny.pause_agent` | Pauses an agent. The grant stays and its writes are rejected until it is resumed. |
| `tiny.resume_agent` | Resumes a paused agent. |
| `tiny.revoke_agent` | Revokes an agent's grant. Only a new grant restores its access. |

Each control takes the agent's public key and requests a signature. Signing a new grant stays on the **Manage > Agents** page, since the secret key of a generated agent is shown to the person once.

## Access requests

The owner and moderators review [access requests](membership.md#access-requests), the join requests from people who asked without an invite:

| Tool | What it does |
| --- | --- |
| `tiny.list_join_requests` | Lists access requests, pending first, with each one's key, reason, time asked, state and decision. Read-only; uses the connected signer. |
| `tiny.approve_join` | Approves a request by public key. The key becomes a member and the relay's member list follows. |
| `tiny.deny_join` | Denies a request by public key. The key may ask again. |

Each decision requests a signature and is recorded in the audit log.

Four more tools manage [callbacks](agents.md#callbacks), the URLs an agent registers to be woken by new events:

| Tool | What it does |
| --- | --- |
| `tiny.list_callbacks` | Lists callbacks with their id, owner, host, filter, state, failures and last delivery. Members and agents see their own; the owner and moderators see all. Uses the connected signer. |
| `tiny.pause_callback` | Pauses a callback by id. Deliveries stop until it is resumed. |
| `tiny.resume_callback` | Resumes a paused callback and clears its failure count. |
| `tiny.remove_callback` | Deletes a callback by id. |

Registering a callback needs the secret shown once to the agent that will verify deliveries, so it is done by the agent itself over MCP or the management API rather than from the browser.

Agents that run outside the browser use the same tools over the relay's MCP endpoint. See [MCP](mcp.md).
