# Browser tools

Open **Manage > Tools** to see the tools available to your browser agent. The agent can browse repositories, files, issues and pull requests, inspect service status, manage background jobs, create backups and update relay settings.

Choose **Sign in** in the main navigation and connect your Nostr signer. Reading protected information uses your browser session. Management actions request a signature and follow your account permissions. A queued backup or job has not finished until its status says so.

In Chrome, enable **WebMCP for testing** at `chrome://flags/#enable-webmcp-testing` and relaunch. Other browsers can use the same pages and forms. See [Chrome's WebMCP guide](https://developer.chrome.com/docs/ai/webmcp) for availability.

The Tools page lists registered tools and the most recent operation. Read the current policy or connection list before changing settings. Job intervals are measured in hours; zero runs once.
