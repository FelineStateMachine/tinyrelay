# Browser tools

Every page registers tools for your browser agent; **Manage > Health** reports whether they are ready, and Chrome's Application panel lists them with their calls. The agent can browse repositories, files, issues and pull requests, list the requests that wait for your decision, inspect service status, manage background jobs, create backups, update relay settings, open Nostr links, the Files page and the Approvals page, read this device's notification state and, as the owner, send a test notification. Turning notifications on stays a manual step, since the browser asks the person for permission.

## Approvals

Three tools cover requests for a decision. `tiny.list_approvals` lists the requests addressed to the signed-in person with each one's asker, subject, expiry, state and answer, plus counts of open, answered and expired requests; `state` narrows the list to `open`, `answered`, `expired` or `all`. `tiny.read_approval` returns one request by event id with every reaction and reply from the people asked. `tiny.open_approvals` opens the Approvals page, focused on one request when `id` is given. Answering stays a manual step: the person approves, denies or replies with their own signer. To make a request, publish an event as described in [Asking a person](agents.md#asking-a-person).

Choose **Sign in** in the main navigation and connect your Nostr signer. Reading protected information uses your browser session. Management actions request a signature and follow your account permissions. A queued backup or job has not finished until its status says so.

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

Agents that run outside the browser use the same tools over the relay's MCP endpoint. See [MCP](mcp.md).
