// Package daemon assembles the services of a multitenant Nostr relay.
// App owns the tenant catalog, telemetry and active Tenant instances. Each
// Tenant owns one store, its policy snapshot, protocol handlers and background
// services. HTTP, WebSocket and management requests reach domain services
// through these adapters.
//
// # Process and tenant lifetimes
//
// New opens the catalog and telemetry and attempts to resume incomplete tenant
// creation.
// Start acquires the serving process lock, opens ready tenants and starts
// catalog reconciliation. The caller serves App as an HTTP handler and calls
// Close after stopping its HTTP listeners. Close stops reconciliation, closes
// tenants, releases shared resources and releases the process lock. The
// optional peer monitor runs under the context passed to PeerMonitorRun.
//
// A tenant initializes community state and its admission gate before building
// the protocol and feature services. Custom-view storage is ready when Git
// initialization can promote repository events; the Git service is then bound
// to the view service. Handler registration completes before worker goroutines
// start. Tenant.Close stops new operations, cancels and joins background work,
// closes live connections and drains active operations before closing storage.
//
// # Admission and persistence
//
// Tenant implements the relay backend and the typed room reader used by the
// web UI. Execute dispatches management methods under the supplied actor's
// current authority. HTTP and WebSocket adapters establish that actor through
// request authentication. Policy returns a read-only snapshot; policy changes
// are serialized and replace the snapshot after persistence succeeds.
//
// Client publication and replication ingestion use eventCommitter to compose
// event storage, domain projections and durable intents. Client entry points
// also process membership, moderation and room actions. The relay router owns
// live event delivery. A repository state stays pending until Git promotion
// makes its referenced objects available.
//
// # Optional work and maintenance
//
// callbackService owns webhook registrations and delivery. customViewService
// owns transform definitions, requests and signed artifacts. eventFollowups
// captures matching registrations and persists planning work with accepted
// events. Planning checks current sources and registrations, uses stable work
// identities and retries within bounded attempts and age. Each feature's
// handler owns its remote delivery or transform policy.
//
// The maintenance gate admits ordinary operations while exclusive maintenance
// is idle. A maintenance operation blocks new admissions and drains active
// operations. Nested calls carry the admission in their context, and callers
// release it with the returned completion function. Workers use the same gate
// while operating on the tenant's services and store.
package daemon
