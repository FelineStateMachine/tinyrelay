# Git collaboration

Open a repository and choose **Issues** or **Pull requests**. Search titles and descriptions, filter by status or label, and follow a conversation from its detail page. Reading works without JavaScript. Publishing requires JavaScript and a connected NIP-07 or NIP-46 signer.

## Issues and pull requests

Create an issue with a title, description and optional comma-separated labels. A pull request also needs the full Git commit ID and an HTTP or HTTPS clone URL. The optional merge base identifies the common ancestor with the target branch.

Replies can address the original post or an individual comment. The author, repository owner and current maintainers can change status. Maintainers are the keys in the announcement's `maintainers` tag and any [agent](agents.md) whose active grant holds `maintain` on the repository; the repository panel lists them and marks agents with the grant's name. Issues support open, resolved, closed and draft states; pull requests use merged in place of resolved. Marking a pull request merged records its status; merge and push the Git changes with your Git client.

Pull request diffs use commits available in the hosted repository. Missing commits produce an availability message. Diff previews are limited to 512 KiB, with further display limits for long output. Clone the repository to review the complete change.

Events follow [NIP-34](https://github.com/nostr-protocol/nips/blob/master/34.md), with replies using [NIP-22](https://github.com/nostr-protocol/nips/blob/master/22.md). Markdown is supported in issues and pull request descriptions; comments are plain text. Status reflects the newest visible event signed by an authorized author or maintainer.

## Proposals from agents

An issue, pull request, patch or comment published by an [agent](agents.md) whose grant holds `propose` on the repository is a proposal. It is pending until the repository owner, one of its maintainers, the relay owner or a moderator reacts to the event's id with `+`, which approves it, or `-`, which rejects it. Maintainers are the keys in the announcement's `maintainers` tag and any agent whose active grant holds `maintain` on the repository. When there is more than one reaction from them, the newest counts. Reactions from members and from the agent itself do not change the state. A resubmitted event is a new id, so it waits for its own approval. Status changes still need `maintain`, and a status event from a proposing agent never counts toward an issue's or pull request's status.

A pending or rejected proposal is visible only to those deciders and the agent that published it. For everyone else it does not exist: the issue and pull request lists leave it out, its page is not found, a proposed comment is missing from the conversation, and a proposed patch is missing from the repository's activity. An approved proposal is an ordinary issue, pull request, patch or comment. The same rule applies to the browser queries and the MCP tools, which read through the same code.

The lists mark each proposal with its state for the people who may see it. On an issue or pull request page, the deciders see "Proposed by" the agent above the body, and above each proposed comment, with Accept and Reject buttons while the proposal is pending, and the decision, its time and the deciding key afterward; each press signs one reaction. The agent sees its proposal with its state and no buttons. Every listed or shown item carries `proposal`, `approval` (`pending`, `approved` or `rejected`) and, once decided, `approval_event`, `approval_at` and `approval_by`; the list and detail results carry `can_approve` for callers who may decide.

When a proposal arrives, the repository owner's and maintainers' devices are woken in the requests for a decision category with the agent's name, what it proposes and the subject or an excerpt. A decision wakes nobody. Events from agents whose grant holds `read` or `maintain`, and from people, are never proposals.

## Review comments

A comment under a pull request or patch can point at one line of the diff. Each line of the diff on the pull request page has a **comment** link; it opens the reply form with that line set, and the page says which file and line the comment will name. Comments that name a line are shown under that line of the diff, oldest first, and the rest of the conversation follows below. A comment whose line is not part of the diff shown stays in the conversation with its file and line noted.

A review comment is a kind 1111 event with the NIP-22 tags a comment normally carries plus two tags:

| Tag | Value |
| --- | --- |
| `file` | The path of the file in the diff, as Git prints it after `b/`. |
| `line` | The line number, then the side: `old` for the base version or `new` for the proposed version. |

Example tags for a comment on line 12 of the new version of `src/main.go`:

```json
[
  ["a", "30617:<owner pubkey>:tinyrelay"],
  ["E", "<pull request id>", "", "<author pubkey>"],
  ["K", "1618"],
  ["P", "<author pubkey>"],
  ["e", "<pull request id>", "", "<author pubkey>"],
  ["k", "1618"],
  ["p", "<author pubkey>"],
  ["file", "src/main.go"],
  ["line", "12", "new"]
]
```

The two tags go together, once each. The path must not be empty, the line number must be a positive integer and the side must be `old` or `new`. The root named by the `K` tag must be a pull request (kind 1618) or patch (kind 1617). The relay refuses a comment whose tags break these rules with a `blocked:` reason, whether it is published directly or arrives through synchronization. The `browsepull` query and the `read_pull_request` tool return each reply with `file`, `line` and `side` fields; the fields are absent on comments that name no line. Agents publish review comments through the `comment` tool with its `file`, `line` and `side` arguments. See [MCP](mcp.md).

The browser tools page exposes the same lists and detail pages to browser agents. See [Browser tools](webmcp.md).

## Synchronization

Enable `features.grasp` and `features.grasp02` to synchronize repository announcements, state and conversations from the repository's announced relays. History synchronization prefers NIP-77 reconciliation and falls back to paginated relay queries. Live subscriptions follow new events as they arrive. Missing Git objects for signed branch state and pull request commits are fetched from the announced sources. Pull request commits stay available in the hosted repository under `refs/nostr/<event-id>`.

Enable `features.grasp03` as well to discover participant outboxes from their NIP-65 relay lists and kind 10317 GRASP lists. Profile and relay-list events are cached locally. New discussion participants are included in subsequent discovery passes.

Synchronization runs on a schedule. A repository that synchronizes completely is checked again after 55 minutes. One with more history to fetch continues after five minutes, and one that fails is retried after five minutes. A pass that runs out of time keeps its progress and continues from the next relay or participant on the following pass. Live subscriptions and outbox discovery are limited so a busy repository does not open unbounded connections.

History responses are limited to 10,000 events per filter. When local history exceeds that limit, synchronization refreshes up to 256 recent events and resumes older history from a saved cursor. A cursor advances only after its events are stored or rejected by policy; a storage failure keeps the window for the next attempt. Interrupted queries retain received events for the next attempt. Large histories remain marked incomplete; slow peers and large groups of events with the same timestamp can prevent older history from converging.

Peers that do not support NIP-77 use paginated queries for four hours before another compatibility probe. Private peer authorization is checked on every connection.

Pull request repair limits ancestry depth to 128 commits per tip and allows 256 MiB of transfer traffic across its sources and updates. Fetched objects are checked in temporary storage before the pull request refs become visible. A tip must have complete ancestry, using objects already hosted when available. Repairs that exceed these limits leave the pull request unavailable until the required objects are hosted through another route, such as an authorized Git push.

## Private repositories

Private synchronization requires a GRASP-08 tenant and operator-configured private peers. Relay discovery cannot authorize a new private destination. Both event queries and Git object repair stay within the configured peer list, and private relay connections authenticate with NIP-42. Members still need permission to read the local tenant.

See [Files and private repositories](files-and-private-repositories.md) for private hosting setup.
