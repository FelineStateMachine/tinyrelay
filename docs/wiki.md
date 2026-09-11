# Wiki

The relay hosts a wiki in which anyone with write access can publish an article about a subject, and several people can hold their own version of the same page. Pages, versions, merge requests and redirects are Nostr events that follow [NIP-54](https://github.com/nostr-protocol/nips/blob/master/54.md), so any wiki client can read and write them. Reading follows the relay's read rule: on a members-only relay, guests see no pages.

## Pages and names

A page is identified by its name, the `d` tag of a kind 30818 article. Names are normalized before use: letters become lowercase, spaces become hyphens, punctuation and symbols are dropped, repeated hyphens collapse and hyphens at either end are removed. Letters in every script and all digits are kept. "What's Up?" becomes `whats-up`, "日本語 Article" becomes `日本語-article` and "Москва" becomes `москва`. Looking up a page by its title or by any spelling that normalizes to the same name finds the same page.

An article carries a `title` for display, an optional `summary` for lists and its content in [Djot](https://djot.net/) markup. Two link forms are specific to the wiki: `[[Page Name]]` or `[[Page Name|shown text]]` links to another page, and a reference-style link with no definition, such as `[proof of work][]`, does the same. Links to profiles and events use `nostr:` addresses, such as `[Bob](nostr:npub1...)`. The page list searches titles and summaries.

The relay renders headings, paragraphs, emphasis, strong text, code spans and blocks, bullet and numbered lists, pipe tables, links, wikilinks and `nostr:` links. Other markup and any HTML in an article is shown as plain text. Links are limited to `http`, `https`, `mailto`, `nostr` and relay paths.

## Versions and forks

Each author holds one version of a page; publishing the page again replaces that author's earlier version and keeps it as a [revision](#revisions). A page lists every version newest first. The version shown by default is chosen in this order: the author you asked for, the relay owner's version, the version with the most `+` reactions from relay members, then the newest. You can also open any version by its event id.

To build on someone else's version, publish your own copy with `a` and `e` tags carrying the `fork` marker, which record the exact version you started from. If you later consider another version better than your own, add `a` and `e` tags with the `defer` marker pointing to it. A page shows the fork and defer references of every version.

## Revisions

A version has a history. When an author publishes a page again, the relay keeps the replaced event as a revision instead of discarding it. The author's first event is revision 1, and the number rises with every replacement, so the page header reads "revision 3 by" the author before the version position. Every version carries `revision` and `revisions`, its number and the author's count.

A page's history lists every revision across authors, newest first: each author's current version and the revisions it replaced, with the author, title, summary, the time it was published and, for a replaced revision, `superseded_by`, the id of the event that took its place. A proposal keeps its state in the history. Any revision opens by its event id; an archived revision renders with a note that a newer revision exists. Reactions to a revision's event id keep counting after it is replaced.

Revisions stay until the page's author deletes them with a kind 5 event naming the revision's event id. A kind 5 naming the page's address also removes the author's revisions published up to that time. The relay never prunes revisions on its own.

## Merge requests

A merge request is a kind 818 event that asks the author of one version to take in changes from another. It names the target article with an `a` tag, the destination author with a `p` tag and the proposed version with an `e` tag carrying the `source` marker. An `e` tag without the marker records the version the change was based on, and the content explains the request. A page lists the merge requests aimed at any of its versions, and the relay wakes the destination author's devices in the replies category when a request arrives.

The destination author answers with a [NIP-25](https://github.com/nostr-protocol/nips/blob/master/25.md) reaction to the merge request: `+` accepts it and `-` rejects it. A request with no reaction from the destination author is open; when there is more than one reaction, the newest counts. Reactions from anyone else do not change the answer.

Merge requests also appear in **Approvals** for their destination author, with a link to compare the versions. A decision made there has the same effect as a decision on the wiki page.

Accepting a request does not change the article by itself. The destination author, or their client, publishes the merged content as a new version of their article. Until that happens, the request reads as accepted and the article stays as it was.

## Proposals from agents

A version published by an [agent](agents.md) whose grant says `wiki: propose` is a proposal. It is pending until the relay owner or a moderator reacts to the version's event id with `+`, which approves it, or `-`, which rejects it. When there is more than one reaction from them, the newest counts. Reactions from members and from the agent do not change the state. Each version is a new event id, so an agent's edit of an approved page is a new proposal that waits for its own approval, and the approved revision stays in view until the edit is approved.

A pending or rejected proposal is visible only to the owner, moderators and the agent that published it. Everyone else sees the agent's newest approved revision of the page in its place, so an approved page keeps showing its last approved text while an edit waits for a decision or after one is rejected. The page list, the versions and the history follow the same rule: a revision that is not approved is left out for readers, and a page with no approved revision by any author is not found. An approved proposal is an ordinary version. The same rule applies to the browser queries and the MCP tools, which read through the same code.

The page list, the page and its history mark each proposal with its state for the people who may see it. Above the article, the owner and moderators see "Proposed by" the agent with Accept and Reject buttons while a proposal is pending, and the decision, its time and the deciding key afterward; each press signs one reaction. While the revision in view is not approved, the bar also names the approved revision readers see. The agent sees its proposal with its state and no buttons. Every browse result that lists versions carries `proposal`, `approval` (`pending`, `approved` or `rejected`) and, once decided, `approval_event`, `approval_at` and `approval_by`; the page and list results carry `can_approve` for callers who may decide.

Pending proposals also appear in **Approvals** for the owner and moderators. The page includes the wiki title and a link to the exact revision being reviewed. It shares the wiki page's approval state, so a decision from either place updates both.

When a proposal arrives, the owner's and moderators' devices are woken in the requests for a decision category with the agent's name and the page title. A decision wakes nobody. Versions from agents whose grant says `wiki: edit`, and from people, are never proposals.

## Redirects

A redirect is a kind 30819 event whose `d` tag is the alternative name and whose `a` tag points to the article it stands for, so `btc` can lead to `bitcoin` without copying the content. A page shows the redirects that lead to it and, when a name has no article of its own, the redirects that lead away from it. Several redirects for one name can serve as a disambiguation list.

## In the web UI

The Wiki tab lists pages with the version shown by default, the number of versions, the open merge requests and the latest change. The search box matches titles and summaries. A page shows its article under a line with the name, the revision number, the author, the version count and the number of forks. The panel lists the other versions, the number of revisions, the names that lead to the page, and links to edit or fork the page and to view its history, which lists every revision with its state. A name that has no page yet offers the editor to signed-in members.

An open merge request aimed at the shown version appears above the article. Compare shows the proposed version above the current one. The destination author accepts or rejects the request with one press, which signs a reaction; anyone can reply on the request's event page.

The editor takes a title, a name, a summary and Djot content, and publishes with the connected signer. The name follows the title until it is edited and is normalized the way the relay normalizes it. Someone other than the page's author publishes a fork of the version in view and can propose it to that author, which also publishes a merge request. Publishing needs JavaScript and a signer; reading does not.

## Browser tools

Browser agents read the same information through three queries: `browsewiki` lists pages with optional `q`, `author`, `limit` and `cursor` parameters; `browsewikipage` returns one page by `d` with its versions and `history`, with optional `author` and `version`, which also opens an archived revision; and `browsewikimerge` returns one merge request by `id` with its proposed and target versions. See [Browser tools](webmcp.md).
