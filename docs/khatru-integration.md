# Khatru integration spike

Date: 2026-09-07
Module inspected: `fiatjaf.com/nostr@v0.0.0-20260902034142-316ef6591fa2/khatru`
Reference transport: `../bindws/src/relay.ts`

## Decision

Khatru is useful protocol prior art and can carry a small first milestone, but it
is not a drop-in transport for tinyrelay's full contract. Use its event types,
NIP-42/NIP-45/NIP-77 codecs and policy vocabulary where useful. Keep the
production websocket transport under tinyrelay's ownership once the requirements
below matter. The decisive reasons are the historical/live subscription race,
the lack of a connection shutdown API, and the absence of an error-bearing query
iterator. These are correctness and lifecycle boundaries, not benchmark concerns.

## What the pinned module provides

`Relay` exposes hooks for event admission, storage, replacement, deletion,
request and count authorization, historical query, authentication, listener
add/remove, live broadcast filtering, and NIP-11/NIP-86. `RequestAuth(ctx)` can
send a challenge from `OnConnect`, `OnEvent`, `OnRequest`, or another hook. NIP-45
COUNT and NIP-77 negentropy are wired through `Count`/`CountHLL` and
`QueryStored`.

The wire path verifies event IDs and signatures before hooks. Normal events are
stored, then `OnEventSaved` runs, then listeners are notified. Deletion events
are stored before Khatru's NIP-09 target deletion pass. `QueryStored` is
`iter.Seq[nostr.Event]`; iteration has no error return. The default websocket
handler starts a goroutine for every incoming message and two connection loops.
Writes are serialized per socket, but there is no relay-level worker registry or
wait group.

## Compatibility matrix

| Requirement | Native hook fit | Adapter / blocker |
|---|---|---|
| Proactive AUTH challenge | Yes | Call `RequestAuth(ctx)` from `OnConnect`; configure the relay URL carefully. This is opt-in and not sent by `NewRelay`. |
| kind 24133 signer exception | Partial | `OnRequest`, `QueryStored`, and `PreventBroadcast` can implement the encrypted NIP-46 visibility rule. No built-in exception exists; make it an explicit policy branch and test historical, live, COUNT, and negentropy paths separately. |
| Per-event privacy for REQ/live | Partial | Historical privacy belongs inside `QueryStored`; live privacy belongs in `PreventBroadcast`. `OnRequest` alone is too coarse. |
| Per-event privacy for COUNT | Yes, with own store | Implement `Count`/`CountHLL` against the same authorization predicate. The hook only gates the request; it does not filter rows for you. |
| Per-event privacy for negentropy | Yes, with own store | Negentropy calls `QueryStored` with a negentropy context, so the same filtered iterator can work. Verify that the vector contains exactly the events visible to that authenticated client. |
| No historical/live gap | No | Khatru queries history first and adds listeners afterward. An event committed between those operations can be missed. There is no public snapshot/fence hook. Own the transport or add a store-level sequence barrier and modify the dispatch path. |
| NIP-09 deletion-before-arrival tombstones | No | Khatru only deletes rows it finds. Persist tombstones in an admission transaction and have future `StoreEvent` reject/ignore matching events. This requires an own storage/admission layer. |
| Gift-recipient deletion | Partial | `AllowDeleting(target, deletion)` can authorize the p-tagged recipient for kind 1059. The default path has no recipient tombstone model, so recipient deletes still need the own store. |
| Private policy change closes sessions | No | The public API returns snapshots and listener callbacks, but does not expose a socket close/remove method. `WebSocket.Context` is observable, not cancellable by callers. Own transport or maintain a fork. |
| Event + delivery intent atomic acknowledgement | Partial | Put both writes in a custom `StoreEvent`/`ReplaceEvent` transaction. Do not use `UseEventstore` unchanged: its `SaveEvent` cannot atomically add tinyrelay delivery rows. Intent calculation must be deterministic and side-effect free inside the transaction. |
| Query/storage errors | No | `iter.Seq` cannot report a database error. A failing iterator can only stop early, which Khatru will treat as a complete EOSE. Use a query API with an error result in the owned transport, or expose a conservative health/error side channel and fail closed. |
| Clean shutdown of all goroutines | No | The comments mention `Server.Shutdown`, but this pinned package has no exported relay shutdown/wait method. `http.Server.Shutdown` does not provide a join for Khatru's websocket goroutines. Track and close sockets in an owned transport. |
| Arbitrary operator host limits | Partial | `MaxMessageSize`, `MaxAuthenticatedClients`, websocket deadlines, and query limits are configurable. Defaults include 512000-byte messages, eight authenticated keys, and a caller-supplied query limit; set every limit from explicit host configuration so no default becomes a product cap. |

## Required adapter rules if Khatru is used temporarily

* Set `ChallengePrefix`, `ServiceURL`, `MaxMessageSize`,
  `MaxAuthenticatedClients`, write/read/ping deadlines, and query limits from the
  operator configuration. Do not inherit `NewRelay` defaults silently.
* Implement one authorization predicate and reuse it from `QueryStored`,
  `Count`/`CountHLL`, `PreventBroadcast`, and negentropy. It must understand
  authenticated pubkeys, event author, p-tags, private kinds, and the 24133
  signer exception.
* Wrap `StoreEvent`, `ReplaceEvent`, and deletion admission in the tenant
  transaction. Store a deletion tombstone before acknowledging a deletion and
  check it before accepting a late event. Add recipient tombstones for gift
  wraps according to the local privacy policy.
* Persist delivery intent before returning a successful local `OK`. Remote
  sends remain at-least-once and must be idempotent by tenant, event, and target.
* Treat a query iterator ending without an explicit store-success marker as
  incomplete. Until the owned transport exists, surface a degraded health state
  rather than claiming a complete history.
* On policy revision, reject new requests immediately and close every affected
  socket through the owned connection registry. Khatru hooks alone cannot do the
  close.

## Recommendation

Prototype event policy and storage transactions behind Khatru only if it shortens
the first vertical slice. Before enabling private relay templates or promising
durable subscriptions, replace `HandleWebsocket` with a tinyrelay transport that
owns a per-connection reader, single writer, cancellation, admission fence, and
wait-group shutdown. Keep Khatru's codecs and NIP implementations, but do not
make Khatru's default `ServeHTTP` the long-term lifecycle boundary.
