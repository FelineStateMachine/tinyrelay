# Wiki

The relay hosts a wiki in which anyone with write access can publish an article about a subject, and several people can hold their own version of the same page. Pages, versions, merge requests and redirects are Nostr events that follow [NIP-54](https://github.com/nostr-protocol/nips/blob/master/54.md), so any wiki client can read and write them. Reading follows the relay's read rule: on a members-only relay, guests see no pages.

## Pages and names

A page is identified by its name, the `d` tag of a kind 30818 article. Names are normalized before use: letters become lowercase, spaces become hyphens, punctuation and symbols are dropped, repeated hyphens collapse and hyphens at either end are removed. Letters in every script and all digits are kept. "What's Up?" becomes `whats-up`, "日本語 Article" becomes `日本語-article` and "Москва" becomes `москва`. Looking up a page by its title or by any spelling that normalizes to the same name finds the same page.

An article carries a `title` for display, an optional `summary` for lists and its content in [Djot](https://djot.net/) markup. Two link forms are specific to the wiki: `[[Page Name]]` or `[[Page Name|shown text]]` links to another page, and a reference-style link with no definition, such as `[proof of work][]`, does the same. Links to profiles and events use `nostr:` addresses, such as `[Bob](nostr:npub1...)`. The page list searches titles and summaries.

The relay renders headings, paragraphs, emphasis, strong text, code spans and blocks, bullet and numbered lists, links, wikilinks and `nostr:` links. Other markup and any HTML in an article is shown as plain text. Links are limited to `http`, `https`, `mailto`, `nostr` and relay paths.

## Versions and forks

Each author holds one version of a page; publishing the page again replaces that author's earlier version. A page lists every version newest first. The version shown by default is chosen in this order: the author you asked for, the relay owner's version, the version with the most `+` reactions from relay members, then the newest. You can also open any version by its event id.

To build on someone else's version, publish your own copy with `a` and `e` tags carrying the `fork` marker, which record the exact version you started from. If you later consider another version better than your own, add `a` and `e` tags with the `defer` marker pointing to it. A page shows the fork and defer references of every version.

## Merge requests

A merge request is a kind 818 event that asks the author of one version to take in changes from another. It names the target article with an `a` tag, the destination author with a `p` tag and the proposed version with an `e` tag carrying the `source` marker. An `e` tag without the marker records the version the change was based on, and the content explains the request. A page lists the merge requests aimed at any of its versions, and the relay wakes the destination author's devices in the replies category when a request arrives.

The destination author answers with a [NIP-25](https://github.com/nostr-protocol/nips/blob/master/25.md) reaction to the merge request: `+` accepts it and `-` rejects it. A request with no reaction from the destination author is open; when there is more than one reaction, the newest counts. Reactions from anyone else do not change the answer.

Accepting a request does not change the article by itself. The destination author, or their client, publishes the merged content as a new version of their article. Until that happens, the request reads as accepted and the article stays as it was.

## Redirects

A redirect is a kind 30819 event whose `d` tag is the alternative name and whose `a` tag points to the article it stands for, so `btc` can lead to `bitcoin` without copying the content. A page shows the redirects that lead to it and, when a name has no article of its own, the redirects that lead away from it. Several redirects for one name can serve as a disambiguation list.

## Browser tools

Browser agents read the same information through three queries: `browsewiki` lists pages with optional `q`, `author`, `limit` and `cursor` parameters; `browsewikipage` returns one page by `d` with optional `author` and `version`; and `browsewikimerge` returns one merge request by `id` with its proposed and target versions. See [Browser tools](webmcp.md).
