// Package relay is the WebSocket transport boundary for the relay.
//
// Router owns connection lifecycle, authentication messages, subscriptions,
// request and count queries, event publication and NIP-77 negotiation. A
// Backend supplies durable publication and query behavior; optional interfaces
// add reader privacy, query hints, policy invalidation and sync snapshots.
// Router serializes publication with subscription registration and snapshots,
// so a live event cannot pass a historical query boundary. Close stops every
// connection and waits for its reader and writer goroutines to finish.
package relay
