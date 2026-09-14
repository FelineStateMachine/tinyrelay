# Social

**Social** brings notes, longer posts and media into one chronological feed, with replies and reaction counts folded into each post. Use the Social navigation to browse Feed, Notes, Posts, Photos, Videos, Podcasts or Profile.

Feed shows everything together. Notes collects short updates, while Posts collects long-form articles. Photos and Videos show posts with matching media. Podcasts collects native Nostr shows and episodes. An audio attachment on a note remains a note.

## Profiles

Choose **Profile** to see your own profile and posts. Select an author's name on any post to see theirs, or enter a public key or npub on the profile page. The profile's filters narrow that person's posts by type.

You can read profiles without signing in when the relay allows public reads. Sign in to open your own profile or use **Edit profile** to change your name, picture and other details.

## Podcasts

Podcasts follow [NIP-F4](https://github.com/nostr-protocol/nips/blob/master/F4.md). Each show has its own Nostr key. Sign in with that key, open **Podcasts**, then choose **Your podcast**. Use **Podcast details** to set the show's title, description, artwork and website. Use **Publish an episode** to upload audio or enter its URL, add a title and write show notes in Markdown.

The show information is a replaceable kind 10154 event. Episodes are kind 54 events signed by the show key, with `title`, `description`, optional `image` and one or more `audio` tags. Each audio tag contains its URL and optional media type. Uploaded audio also includes NIP-92 metadata with its type, size and hash.

A show's author credits appear when both sides agree: the show names the person in a `p` tag, and that person's [NIP-51](https://github.com/nostr-protocol/nips/blob/master/51.md) authored-podcasts list, kind 10064, names the show. Favorite-podcast lists use kind 10054. These lists can be published by other Nostr clients.

### Listen in a podcast app

Open a show's profile and choose **Subscribe with RSS**. Copy that address into your podcast app's option to add a show by URL. The feed exposes the same native episodes, with show details, artwork, show notes and audio enclosures. Episode IDs remain stable between refreshes.

The feed for one show is `/social/podcasts.rss?author=<show-pubkey>`. The address also accepts an npub. `/social/podcasts.rss` combines the newest 100 episodes on the relay; a show feed contains that show's newest 100 episodes. Public feeds need no sign-in. Private relay feeds retain their normal access requirements, so a conventional player must support the relay's authentication to read them.

The built-in `podcasts` [relay view](views.md) refreshes after show and episode publication. RSS is an XML presentation of the current native events: show edits appear on refresh, and deleted or expired episodes disappear. Audio streams from its published URL. For broad player support, use MP3 or M4A audio hosted with support for HEAD and byte-range requests. The relay's own file hosting supports both.

## Notes and articles

A note is a regular kind 1 text note. It is suited to short updates and microblogging. An article is a replaceable kind 30023 event with a `d` tag, a `title` tag and Markdown content. Articles can also carry `summary` and `image` tags for previews. The Posts view shows these articles newest first.

The web page supports writing either form, previewing article Markdown, and adding media. The relay keeps the signed event unchanged, so a Nostr client can publish to or read from the same feed. See [NIP-01](https://github.com/nostr-protocol/nips/blob/master/01.md), [NIP-23](https://github.com/nostr-protocol/nips/blob/master/23.md) and [NIP-19](https://github.com/nostr-protocol/nips/blob/master/19.md).

## Conversations

Replies to notes use kind 1 with the usual [NIP-10](https://github.com/nostr-protocol/nips/blob/master/10.md) thread references. Articles and podcast episodes use kind 1111 comments with [NIP-22](https://github.com/nostr-protocol/nips/blob/master/22.md). Article roots use `A`, `K` and `P`; podcast episode roots use `E`, `K` and `P`. Lowercase tags identify the immediate parent. Older kind 1 article replies remain readable when they carry an article `a` tag.

Open a post to see its conversation. Replies are folded into the post's count in the feed, while the thread view lists comments in order and shows reaction totals with the same author and profile information.

## Reactions

Reactions are kind 7 events following [NIP-25](https://github.com/nostr-protocol/nips/blob/master/25.md). The feed aggregates each reaction by content and counts one reaction per author and content for a post. Custom emoji from [NIP-30](https://github.com/nostr-protocol/nips/blob/master/30.md) use their image metadata and remain distinct when the same shortcode names different images. The signed-in person's reaction is marked separately, so the page can show which reactions they have already made.

## Media and Markdown

Attach images, video or audio from the composer, or paste a media URL. Uploaded media is stored through the relay's public Blossom file service and referenced in the event with [NIP-92](https://github.com/nostr-protocol/nips/blob/master/92.md) `imeta` tags. Alt text is included with the attachment metadata. Images, video and audio are shown inline when the browser can play them; unsupported media is offered as a direct link.

Articles and podcast show notes use Markdown with the relay's safe text renderer. Notes and comments remain plaintext, with line breaks and safe links preserved. Supported media references become native browser elements; unsafe HTML and unsupported link targets remain text.

## Routes and feeds

Use `/social` for the mixed feed and `/social/<event-id>` for a post and its conversation. `/social/profile` opens your profile; add `?author=<pubkey>` to view another person. An addressable article can also be opened with `/social?address=30023:<pubkey>:<d-tag>`. `/articles` redirects to Social, while `/articles.json` and `/feed.xml` remain available for existing article readers. The mixed feed is available as `/social.json` and `/social.xml`.

The browser keeps unfinished composer text as a local draft for the signed-in account and relay. Drafts stay in that browser until published or cleared. Preview requests may send the current text to the relay for rendering, but a draft is not published until you submit it.

## Relay template

The **Social** tenant template enables member publishing for notes, comments, reactions, articles and native podcasts while keeping reads public. It also allows profiles, podcast lists and deletion events. Existing relays with a custom kind allowlist must include 54, 10154, 10064 and 10054 to accept all podcast events.

Add the relay directly to any Nostr client that supports NIP-01 subscriptions and the event kinds above. The same events can therefore be viewed in the relay, Jumble, Primal and other Nostr clients.
