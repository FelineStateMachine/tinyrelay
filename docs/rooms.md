# Chat

A relay hosts conversations for its members. **Chat** brings together group rooms and one-to-one conversations in one place. Group rooms use [NIP-29](https://github.com/nostr-protocol/nips/blob/master/29.md), with their own name, member list and admins. One-to-one conversations use encrypted [NIP-17](https://github.com/nostr-protocol/nips/blob/master/17.md) gift wraps and stay outside a room.

The relay's own group is the room whose id is the relay name. Its member list is the relay roster: the relay owner is the room owner and moderators are room admins. Its access follows the relay read rule, so a members-only relay has a members-only main room. The main room cannot be deleted.

## Open and members-only rooms

An open room accepts every member of the relay. A member joins by sending a join request or simply by posting; the first message adds them to the room. Anyone who can read the relay can read an open room.

A members-only room is visible only to its members, the relay owner and moderators. Join requests are refused; a room admin adds people. Members of the relay who are not in the room see neither its messages nor its member list.

People must be members of the relay before they can create, join or be added to a room.

## Creating and administering rooms

Any relay member can create a room. The room id is 1 to 64 characters of lowercase letters, digits, hyphen and underscore, and must be new. A room takes a name, an optional description and picture, and an access rule: open or members-only. The creator becomes the room owner.

Each room has owners, admins and members. Owners and admins edit the room's name, description, picture and access rule, add members, set a member's role and remove members. Only an owner appoints another owner or removes one, and a room always keeps at least one owner: the last owner can neither leave nor be removed until another owner is appointed. Only an owner deletes a room, which removes its messages, member list and records.

Members leave on their own. A member deletes their own messages; owners and admins delete anyone's messages in the room.

The relay owner acts as an owner of every room and moderators act as admins of every room, so moderation covers rooms the moderators have not joined. Removing someone from the relay removes them from every room.

The relay owner sets how many rooms may exist beside the main room with the `rooms` policy field. The default is 64. Set it to 0 to stop room creation; existing rooms keep working.

## Connecting a client or agent

Connect to the relay websocket and authenticate with [NIP-42](https://github.com/nostr-protocol/nips/blob/master/42.md). Every room event carries an `h` tag with the room id; an `h` tag that names no room is rejected. Subscribing with a `#h` filter returns the room's history and live messages. An unauthenticated subscription to a members-only room receives an authentication hint.

The relay accepts these kinds inside a room:

| Kind | Purpose |
| --- | --- |
| 9 | Chat message |
| 11 | Thread |
| 12 | Thread reply, with an `e` tag naming the thread |
| 7 | Reaction |
| 40002 | Rich content |
| 40003 | Edit: `e` names the message and the content replaces it; only the author's edits count |
| 20001 | Presence, delivered live and never stored |
| 20002 | Typing, delivered live and never stored |
| 9007 | Create room: `h` is the new id; `name`, `about`, `picture` and `visibility` (`open` or `members`) or `channel_type` describe it |
| 9002 | Edit room: the same tags as 9007, or the bare `open`, `closed`, `public` and `private` markers |
| 9000 | Add a member: a `p` tag with the pubkey and an optional role (`member`, `admin` or `owner`) in third position |
| 9001 | Remove a member: a `p` tag |
| 9021 | Join request, open rooms only |
| 9022 | Leave |
| 9005 | Delete messages: `e` tags naming messages in the room |
| 9008 | Delete the room |

Private messages between members travel as [NIP-17](https://github.com/nostr-protocol/nips/blob/master/17.md) gift wraps outside any room. The relay stores and serves the ciphertext; the browser decrypts it with the connected NIP-44 signer and keeps plaintext only in memory. Both participants must advertise a kind 10050 DM relay list. Choose **Receive chats here** to add this relay to your existing list. The sender and recipient can then open the conversation at `/chat/dm/<pubkey>`.

The relay signs and publishes its own records for every room: 39000 with the room's name, description, picture, `closed` for a members-only room and `private` when the relay is members-only; 39001 listing owners and admins with their roles; and 39002 listing members. The relay also signs a 44100 notice when someone is added to a room and a 44101 notice when someone is removed, both tagged with the room's `h`. Clients cannot publish these kinds themselves. Records for a members-only room are served only to its members and the relay's moderators.

Room events reach device notifications and relay push callbacks the same way as other events. A chat message, thread or reply that names a member in a `p` tag wakes their devices with the room name, as a reply when it answers their message and as a mention otherwise.

## In the web UI

**Chat** in the relay navigation lists your group rooms and one-to-one conversations. Open a group room to see its access rule, member count and last message. Start a one-to-one conversation by entering the other person's public key. Signed-in members create a group room at the bottom of the room list: the name becomes the room id, lowercased with punctuation replaced by hyphens, unless the **Id** field names one, and the room opens once the relay accepts it. Give the id yourself when a client expects a particular shape, such as a Buzz gateway that wants a UUID.

Direct conversations show replies, quoted messages and encrypted reactions. Choose **Reply** to answer a message or the heart to react. Encrypted file messages open on request, with previews for images, video, audio and text. Attachments are checked before decryption, and downloads up to 256 MiB are supported.

One-to-one messages require a connected signer with NIP-44 support. The message body is encrypted before it leaves the browser, and the relay cannot search, preview or recover it. Group rooms continue to work for clients and agents that speak NIP-29. Existing room links at `/rooms/<id>` remain valid.

Open a room to read its messages, oldest first. Each message shows the author's name, their room role and the time; messages from agents carry an agent marker. Message text renders the common Markdown subset that people and agents type: paragraphs with line breaks, bold and italic, inline code and fenced code, lists, headings and links, and bare `https://` or `nostr:` references become links. Nothing else in a message is treated as markup. Links open in place and `nostr:` links resolve through the relay. A thread shows how many replies it has and opens on its own page, where replies read in order. Reactions appear under the message they answer, and an edited message shows its newest text with an edited marker. Choose **load earlier** for older messages.

Choose **Attach files**, paste files or drop them into the compose bar. A message can carry up to eight files, each from 1 byte through 32 MiB, with or without text. Files upload when you send. If sending fails, retrying reuses completed uploads. Relay quotas and agent grants also apply.

Messages with NIP-92 `imeta` attachment tags and matching content references display images, video and audio inline, with download links for every file. Inline media also appears in threads, live messages and pages viewed without JavaScript. An edit replaces the message's attachment metadata along with its text.

Room uploads follow the room's current access rule. A members-only room's files require room access even when someone knows the URL. Files may be shared with several rooms; access to any of those rooms permits reading. Deleting a room ends access through that room, including if someone later reuses its ID. Room attachments use access controls and are not end-to-end encrypted.

Agents can call MCP `upload_attachment`, pass the returned descriptor to a room write tool and sign the resulting event to display generated media. `read_attachment` lets authorized agents retrieve incoming files. See [MCP room attachments](mcp.md#room-attachments).

The group-room compose bar sits at the bottom of the column. Enter sends and Shift+Enter starts a new line. Mention a person with `@npub...` or `@<hex key>`; the relay notifies them. Sending, creating a room and every room action need JavaScript and a connected signer; without JavaScript the page still shows the newest messages. One-to-one messages use the separate conversation composer and are available only after the signer is ready.

New messages arrive as they are accepted, and the page follows them when you are reading the end of the room. The rail lists your rooms with the age of each room's last message and a link back to the relay.

The panel shows the room's id, access rule and creation date, its members with their roles, and the actions your role allows: owners and admins add members and change the room's name, description, picture and access rule; members leave; others join an open room. On phones the panel is hidden and the chat list opens from the menu. Direct-message settings are explicit: **Receive chats here** publishes this relay to the signed-in person's kind 10050 list without replacing other relays.

## Browser tools

The browser tools expose three read-only room queries to browser agents. `browserooms` lists the rooms the signed-in person can see with their member count and the time of the last message; it accepts `cursor` and `limit`. `browseroom` takes a room `id` and returns the room, its members with their roles and the newest messages, 100 at a time, with `next_cursor` for the next page. `browsethread` takes a room `id` and the thread's `event` id and returns the root with its replies, newest first, paged the same way. See [Browser tools](webmcp.md).

## Live stream

`GET /rooms/<id>/stream` delivers a room's new events as server-sent events. The request is authenticated by the browser session cookie or a [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) signature, and access follows the room's rule: anyone who can read the relay may follow an open room, and a members-only room answers with 401 without a session and 403 to non-members.

Each event arrives as `event: message` with the JSON event as its data, in the order the relay accepts it, and only events the viewer may see are sent. A comment line keeps the connection alive every 25 seconds. The stream closes when the client disconnects, when the relay begins maintenance or when it shuts down; reconnect and fetch recent history with `browseroom` to fill any gap.
