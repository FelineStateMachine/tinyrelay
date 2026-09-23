# NIP-XX

## Filter-scoped reading positions

`draft` `optional`

Status: Draft. This is an imagined NIP for design review. Tinyrelay does not publish or consume these events.

## Abstract

A reader can save a position in an ordered Nostr event stream and resume from the same event in another client. The stream is identified by a canonical feed filter. This version supports a subset of [NIP-01](https://github.com/nostr-protocol/nips/blob/master/01.md) filters. The position is an event ID and its `created_at` timestamp. An addressable event signed by the reader stores one position per context.

This draft defines a resume point. It does not define an unread count, a read receipt for other people or a claim that every earlier event was seen.

## Motivation

Clients already page through event streams with cursors, but a page cursor is tied to one request. A signed position lets a reader return to the event they last viewed after closing the page or changing devices. Kinds and filter constraints identify the stream without relying on a client route such as `/social` or `/rooms/general`.

## Context

In this version, a context is one NIP-01 filter object containing a nonempty `kinds` array of integers from 0 through 65535 and any of these optional selectors:

- `authors`, containing full, lowercase public keys.
- `ids`, containing full, lowercase event IDs.
- Tag constraints of the form `#<letter>`, where the letter is ASCII and the array contains string values.

These supported arrays are sets: their order and duplicate values do not change the context. Empty arrays are invalid in this version. Other filter fields are outside this draft. A NIP-01 `REQ` with multiple filter objects has no single position under this version; a client may track each filter as a separate context.

`since`, `until` and `limit` are request windows, not context fields. A client removes them before computing the context key. A feed with a different kind, author or tag constraint has a different position.

An optional `scope` identifies the object that owns the stream. It is a NIP-01 address in `kind:pubkey:d` form, with a decimal kind without leading zeros, a full lowercase public key and the exact `d` value. The scope changes the context key but does not add a filter constraint. For a single [NIP-29](https://github.com/nostr-protocol/nips/blob/master/29.md) group selected with one `#h` value, clients must use the group's kind 39000 metadata address as the scope and fetch messages from that group's relay. This distinguishes groups with the same `h` value on different relays. A multi-group context is outside this draft. A broad author feed needs no scope. Clients that assemble different event sets from the same filter and scope may resume near the saved position but cannot assume they have seen the same events.

For example, a group context has filter `{"kinds":[9,12],"#h":["general"]}` and scope `39000:<relay public key>:general`. Its key differs from a group named `general` whose metadata is signed by another relay.

### Canonical key

To compute the key, a client:

1. Removes `since`, `until` and `limit` from the request filter.
2. Rejects unsupported fields and invalid values.
3. Sorts and removes duplicates from each supported array. Kind numbers use ascending numeric order; strings use ascending UTF-8 byte order. It retains every supported property, including its original tag key.
4. Serializes the resulting object using [RFC 8785 JSON canonicalization](https://www.rfc-editor.org/rfc/rfc8785).
5. Computes lowercase hexadecimal SHA-256 of the UTF-8 bytes `nostr-read-position-v1\n` followed by that canonical JSON. If `scope` is present, it appends `\nscope:` and the exact scope string before hashing. `\n` means one LF byte, not a backslash and a letter.

The addressable event's `d` tag is `read-position:` followed by this hash. Clients use the same key when writing and looking up a position. The normalized filter and optional scope are included in the event content so a client can verify that they hash to `d`.

For example, these filters have the same key:

```json
{"kinds":[30023,1,1],"authors":["<author>"],"limit":30}
{"authors":["<author>"],"kinds":[1,30023],"until":1700000000}
```

The placeholder `<author>` stands for the same 64-character lowercase public key in both filters.

With an author of 64 lowercase `a` characters, the canonical filter is `{"authors":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"kinds":[1,30023]}` and the `d` value is `read-position:05fcf00de539b8b6d66cc467cc468ebdb1839edcfafe9eae63620967c8b093f9`.

## Position event

A finalized NIP needs a dedicated addressable kind in the 30000-39999 range. This draft does not assign one. [NIP-78](https://github.com/nostr-protocol/nips/blob/master/78.md) kind 30078 can carry a local experiment, but its format is intended for app-specific data rather than cross-client interchange.

The event has exactly one `d` tag with the canonical key. Its content is a JSON object with the following fields and, when needed, a `scope` string:

```json
{
  "version": 1,
  "filter": {"kinds": [1], "authors": ["<author>"]},
  "cursor": {"created_at": 1700000000, "id": "<event-id>"}
}
```

The `filter` field contains the normalized context filter; clients reject unsupported or extra fields. `scope`, when present, is part of the same context key. `cursor.id` is a full, lowercase event ID. `cursor.created_at` is the nonnegative integer timestamp of that event, not the time the position was saved. The position event's own `created_at` is the save time. A client should set the cursor only to an event it displayed from the context. If the anchor event is available, the client must confirm its ID, timestamp and filter membership before using the position.

The position is signed by the reader. The event ID of the anchor does not need an `e` tag because the cursor is not a reply or a public reference. A client queries the reader's position by the dedicated kind, author and exact `#d` value. The newest addressable event for that `(kind, pubkey, d)` is the saved position under NIP-01 replacement rules.

## Resume behavior

The stream order for this draft is `created_at` descending, then event ID ascending. Clients must sort returned events by this order and remove duplicate IDs before applying the cursor; NIP-01 does not guarantee relay response order. Clients that display a different order must not use this position as an exact resume point.

When opening a context, a client reads the saved event, queries the context filter and displays the anchor event once. A page that continues toward older items excludes the anchor and takes events with a lower `created_at` or the same timestamp and a lexically greater ID. A page toward newer items uses the opposite comparison. The client may use `cursor.created_at` to seek near the anchor before resolving its ID. If the anchor was deleted, replaced or is no longer visible, the next visible older event is the fallback; if none exists, the nearest newer event is the fallback. The client should not present a fallback as the original anchor.

Saving another position publishes a replacement for the same context, even if the anchor is older in the stream. NIP-01 chooses the version with the later event timestamp; when timestamps match, it retains the version with the lower event ID. Clients should advance the save timestamp beyond their last known version and avoid multiple saves within one second. Concurrent saves may still race, so this draft does not promise that the last user action wins. A client may clear the position with a [NIP-09](https://github.com/nostr-protocol/nips/blob/master/09.md) deletion request naming its address. A missing position means the client chooses its ordinary starting point.

## Scope and compatibility

The feed filter is the full rule for deciding which events belong in the stream. A relay query may implement only part of that rule; a client can apply the remaining predicates after receiving events. In this version, the canonical filter contains only the NIP-01 fields listed above. Root-only posts, photos selected from media metadata and text search are also filter predicates, but this version does not define their canonical fields or matching rules. Clients cannot share an exact position for those views under this version. For example, `{"kinds":[1]}` includes replies, so it does not identify tinyrelay's root-only Notes feed.

Clients that do not recognize the position kind ignore it. Relays need only ordinary event storage, addressable replacement and querying by author and `#d`; they do not interpret the cursor. A relay may reject the kind or not retain it, so a client must still work without a saved position.

## Privacy and security

A plaintext position reveals the reader's filter, the last viewed event and when the position was saved to anyone who can fetch the event. A hash in `d` does not hide a guessable filter. This draft defines no encryption. Clients should not publish private room, direct-message or sensitive search positions to a relay that may serve them to other readers. A private transport requires a separate design.

A position is personal state, not authority over the referenced event or proof that it was read. Clients must verify the signature and key, bound the JSON size, reject malformed filters and cursors, and apply their ordinary access checks when fetching the anchor.

## Open design limit

The next revision should define canonical filter fields and matching rules for root posts, media categories and text search. Those fields belong in the same filter and its context key, so clients saving and retrieving a position identify the same feed. A client-specific route or category name does not define event membership.
