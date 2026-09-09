# Browser tools

Every page registers tools for your browser agent; **Manage > Health** reports whether they are ready, and Chrome's Application panel lists them with their calls. The agent can browse repositories, files, issues and pull requests, inspect service status, manage background jobs, create backups, update relay settings, open Nostr links and the Files page, read this device's notification state and, as the owner, send a test notification. Turning notifications on stays a manual step, since the browser asks the person for permission.

Choose **Sign in** in the main navigation and connect your Nostr signer. Reading protected information uses your browser session. Management actions request a signature and follow your account permissions. A queued backup or job has not finished until its status says so.

In Chrome, enable **WebMCP for testing** at `chrome://flags/#enable-webmcp-testing` and relaunch. Other browsers can use the same pages and forms. See [Chrome's WebMCP guide](https://developer.chrome.com/docs/ai/webmcp) for availability.

Read the current policy or connection list before changing settings. Job intervals are measured in hours; zero runs once.

Agents that run outside the browser use the same tools over the relay's MCP endpoint. See [MCP](mcp.md).
