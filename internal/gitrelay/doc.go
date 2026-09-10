// Package gitrelay coordinates signed Nostr repository events with native
// Git object storage and smart HTTP. Signed events establish repository refs,
// metadata and maintainer authority.
//
// GitRelay stores bare repositories under Config.Root. Journal files under
// .tinyrelay/journal make event-to-ref transitions recoverable, while pending
// state files keep a signed ref announcement out of the visible repository
// until every advertised tip object is present. New creates the root,
// recovers journals and reloads repository metadata from the event store.
// Synchronization runs through Service.Tick or transport callbacks invoked by
// the caller.
//
// Validate checks event and maintainer admission. After the caller durably
// stores the event, CommitAfterStore stages the corresponding Git transition.
// CommitAfterStoreNoNotify serves callers already holding the host publication
// fence. PromotePending completes a receive-pack repair and invokes OnPromote
// after state becomes visible.
//
// ServeHTTP exposes Git smart HTTP for public and private repositories.
// Private requests pass through AuthorizeHTTP. GRASP-08 private peer traffic
// is limited to Config.PrivatePeers and is signed by HTTPAuth. GitSync and
// EventSync supply synchronization transports; the daemon supplies the relay
// client and WebSocket lifecycle.
package gitrelay
