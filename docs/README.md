# Guides

Tiny combines a Nostr relay with pages for publishing, conversation, files and collaboration. These guides describe the built-in client and the signed protocols other clients and agents use. Each tenant's policy controls which features and actions are available.

## Use the relay

| Task | Guide |
| --- | --- |
| Set up a personal relay and connect clients | [Personal relay](personal-relay.md) |
| Join, invite people and manage membership | [Membership](membership.md) |
| Manage your profile, relay lists, presence and device notifications | [Account and installed app](app.md) |
| Use group Chat and encrypted one-to-one messages | [Chat](rooms.md) |
| Publish notes and articles, comment and react | [Social](social.md) |
| Upload, organize and preview files; choose public, member or secret-link access | [Files and private repositories](files-and-private-repositories.md) |
| Publish wiki pages and review proposals or merges | [Wiki](wiki.md) |
| Host repositories and collaborate on issues and pull requests | [Git collaboration](git-collaboration.md) |
| Render supported content with templates and custom views | [Views](views.md) and [view definitions](view-definitions.md) |

## Connect an agent

Start with the relay's `/llms.txt` endpoint for its URLs, authentication rules, tool inventory and feature guides. It includes the tenant prefix when opened at `/r/<tenant>/llms.txt`. Use MCP `tools/list` for current argument schemas.

| Task | Guide |
| --- | --- |
| Authenticate and call tools, upload attachments or publish sites | [MCP](mcp.md) |
| Grant an agent access, request missing permissions, ask for decisions or run tasks | [Agents](agents.md) |
| Use tools through a connected browser signer | [Browser tools](webmcp.md) |

Agents sign with their own keys. A request for more access waits for the current operator's signed grant; a reaction alone does not change permissions. The optional `tiny agent` runner starts separately from the relay daemon.

## Develop and operate

- [Architecture](architecture.md) explains service ownership and event flow.
- [Fixi and Paxi](fixi.md) explains page navigation, partial refreshes and signed forms.
- [Testing](testing.md) covers local checks, browser fixtures, protocol conformance and Linux testing.
- [Observability](observability.md) covers health, metrics, logs and profiling.
