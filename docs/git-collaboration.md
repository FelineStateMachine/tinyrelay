# Git collaboration

Open a repository and choose **Issues** or **Pull requests**. Search titles and descriptions, filter by status or label, and follow a conversation from its detail page. Reading works without JavaScript. Publishing requires JavaScript and a connected NIP-07 or NIP-46 signer.

## Issues and pull requests

Create an issue with a title, description and optional comma-separated labels. A pull request also needs the full Git commit ID and an HTTP or HTTPS clone URL. The optional merge base identifies the common ancestor with the target branch.

Replies can address the original post or an individual comment. The author, repository owner and current maintainers can change status. Issues support open, resolved, closed and draft states; pull requests use merged in place of resolved. Marking a pull request merged records its status; merge and push the Git changes with your Git client.

Pull request diffs use commits available in the hosted repository. Missing commits produce an availability message. Diff previews are limited to 512 KiB, with further display limits for long output. Clone the repository to review the complete change.

Events follow [NIP-34](https://github.com/nostr-protocol/nips/blob/master/34.md), with replies using [NIP-22](https://github.com/nostr-protocol/nips/blob/master/22.md). Markdown is supported in issues and pull request descriptions; comments are plain text. Status reflects the newest visible event signed by an authorized author or maintainer.

The browser tools page exposes the same lists and detail pages to browser agents. See [Browser tools](webmcp.md).

## Synchronization

Enable `features.grasp` and `features.grasp02` to synchronize repository announcements, state and conversations from the repository's announced relays. History synchronization prefers NIP-77 reconciliation and falls back to paginated relay queries. Live subscriptions follow new events as they arrive. Missing Git objects for signed branch state and pull request commits are fetched from the announced sources. Pull request commits stay available in the hosted repository under `refs/nostr/<event-id>`.

Enable `features.grasp03` as well to discover participant outboxes from their NIP-65 relay lists and kind 10317 GRASP lists. Profile and relay-list events are cached locally. New discussion participants are included in subsequent discovery passes.

Synchronization runs on a schedule. A repository that synchronizes successfully is checked again after 55 minutes. One that fails is retried after five minutes. A pass that runs out of time keeps its progress and continues from the next relay or participant on the following pass. Live subscriptions and outbox discovery are limited so a busy repository does not open unbounded connections.

History responses are limited to 10,000 events per filter. A history that reaches that limit is reported as incomplete instead of silently claiming success, and larger histories are not yet guaranteed to converge.

## Private repositories

Private synchronization requires a GRASP-08 tenant and operator-configured private peers. Relay discovery cannot authorize a new private destination. Both event queries and Git object repair stay within the configured peer list, and private relay connections authenticate with NIP-42. Members still need permission to read the local tenant.

See [Files and private repositories](files-and-private-repositories.md) for private hosting setup.
