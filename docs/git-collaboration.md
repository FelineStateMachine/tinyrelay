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

Synchronization runs on a schedule. A repository that synchronizes completely is checked again after 55 minutes. One with more history to fetch continues after five minutes, and one that fails is retried after five minutes. A pass that runs out of time keeps its progress and continues from the next relay or participant on the following pass. Live subscriptions and outbox discovery are limited so a busy repository does not open unbounded connections.

History responses are limited to 10,000 events per filter. When local history exceeds that limit, synchronization refreshes up to 256 recent events and resumes older history from a saved cursor. A cursor advances only after its events are stored or rejected by policy; a storage failure keeps the window for the next attempt. Interrupted queries retain received events for the next attempt. Large histories remain marked incomplete; slow peers and large groups of events with the same timestamp can prevent older history from converging.

Peers that do not support NIP-77 use paginated queries for four hours before another compatibility probe. Private peer authorization is checked on every connection.

Pull request repair limits ancestry depth to 128 commits per tip and allows 256 MiB of transfer traffic across its sources and updates. Fetched objects are checked in temporary storage before the pull request refs become visible. A tip must have complete ancestry, using objects already hosted when available. Repairs that exceed these limits leave the pull request unavailable until the required objects are hosted through another route, such as an authorized Git push.

## Private repositories

Private synchronization requires a GRASP-08 tenant and operator-configured private peers. Relay discovery cannot authorize a new private destination. Both event queries and Git object repair stay within the configured peer list, and private relay connections authenticate with NIP-42. Members still need permission to read the local tenant.

See [Files and private repositories](files-and-private-repositories.md) for private hosting setup.
