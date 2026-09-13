// Package tinygit coordinates signed Nostr repository events with native
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
// ParseMetadata checks repository announcement and state shape without a store,
// signature check or host policy. Its Metadata result contains claims, not
// resolved repository authority. Use protocol/nostr.Validate for the separate
// wire, event ID and signature check. Neither check establishes maintainer
// authority, hosting permission or Git object availability, and the metadata
// parser does not claim full NIP or GRASP compliance.
//
// Validate and ValidateImported check signatures and metadata shape before
// resolving repository authority and applying host admission. A valid shape may
// still be denied by Config.Authorize or lack a current announcement. After the
// caller durably stores an admitted event, CommitAfterStore stages its Git transition.
// CommitAfterStoreNoNotify serves callers already holding the host publication
// fence. PromotePending completes a receive-pack repair and invokes OnPromote
// after state becomes visible.
//
// ServeHTTP exposes Git smart HTTP for public and private repositories.
// Private requests pass through AuthorizeHTTP. GRASP-08 private peer traffic
// is limited to Config.PrivatePeers and is signed by HTTPAuth. GitSync and
// EventSync supply synchronization transports; the daemon supplies the relay
// client and WebSocket lifecycle.
//
// For a standalone public host, OpenServer owns event persistence and exposes
// POST /events plus Git smart HTTP. It requires an owner public key, admits
// only that owner's public repositories and their announced maintainers, and
// rejects private announcements. Publish signed kind 30617 metadata first,
// then signed kind 30618 refs before pushing their objects. Receive-pack
// promotes pending refs; unsigned ref updates are rejected by the same native
// hook used by the embedded engine. GET /healthz reports process readiness.
//
// New is the lower-level integration boundary. Supply an absolute Config.Root
// and an OpenStore result, or share the host's existing Store. Event, Policy
// and Store aliases make this API callable outside the tinyrelay module.
// The package still depends on tinyrelay's internal event, policy, auth and
// SQLite storage implementations and schema. It is a public package in the
// root Go module, not a separate dependency-free module. Neither constructor
// starts the daemon, relay protocol, UI or replication workers. Native Git
// and a POSIX shell are required for smart HTTP and receive hooks.
package tinygit
