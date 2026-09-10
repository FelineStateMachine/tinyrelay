# Architecture

Each tenant has a SQLite store, a policy snapshot and a set of services. The daemon constructs those services, connects their contracts and manages their lifetimes. HTTP, WebSocket and management adapters establish caller identity and enter the tenant's operation gate before calling services.

## Find your starting point

[`cmd/tiny`](../cmd/tiny/doc.go) owns command dispatch, listeners and process shutdown. [`daemon`](../internal/daemon/doc.go) describes tenant assembly and event flow. Every internal package has a `doc.go` overview, and interface, constructor and transaction comments describe the contracts at their declarations. These comments are also available through `go doc` and editor symbol help.

## Responsibilities

| Packages | Responsibility |
| --- | --- |
| [`event`](../internal/event/doc.go), [`policy`](../internal/policy/doc.go) | Event and filter values, signing, validation and shared policy decisions. |
| [`auth`](../internal/auth/doc.go), [`gates`](../internal/gates/doc.go) | Signed request proofs, event admission and visibility checks. |
| [`storage`](../internal/storage/doc.go), [`work`](../internal/work/doc.go) | Event persistence, queries, transaction hooks and durable queue claims. |
| [`community`](../internal/community/doc.go), [`communityread`](../internal/communityread/doc.go) | Membership, moderation, rooms, agents and the read models consumed by records. |
| [`records`](../internal/records/doc.go) | Relay identity, signed protocol records, projections and notifications. |
| [`blob`](../internal/blob/doc.go), [`sites`](../internal/sites/doc.go) | Content-addressed files, upload rules and sites backed by signed manifests. |
| [`gitrelay`](../internal/gitrelay/doc.go) | Repository admission, object storage, synchronization, repair and Git HTTP endpoints. |
| [`replication`](../internal/replication/doc.go), [`syncprotocol`](../internal/syncprotocol/doc.go) | Relay synchronization plans and transports, count sketches and reconciliation sessions. |
| [`relay`](../internal/relay/doc.go), [`mcp`](../internal/mcp/doc.go), [`webui`](../internal/webui/doc.go) | WebSocket sessions, MCP requests, HTML pages and browser interactions. |
| [`views`](../internal/views/doc.go), [`wiki`](../internal/wiki/doc.go), [`webpush`](../internal/webpush/doc.go) | Fenced-block parsing, article rendering and encrypted browser push delivery. |
| [`catalog`](../internal/catalog/doc.go), [`domains`](../internal/domains/doc.go) | Tenant lifecycle records, filesystem paths and host mappings. |
| [`templates`](../internal/templates/doc.go), [`configport`](../internal/configport/doc.go) | Built-in policy templates and configuration import, export and application. |
| [`telemetry`](../internal/telemetry/doc.go), [`daemon`](../internal/daemon/doc.go) | Observability, service assembly, request routing and worker ownership. |

## Ownership and contracts

`App` owns the catalog, telemetry and active tenants. Each `Tenant` owns its store and service lifetimes. Services borrow the store and policy reader supplied at construction. Policy reads return a shared snapshot; management writes persist and replace that snapshot. Shutdown stops admissions, joins background work and drains active operations before closing storage.

Services own the schema and data access for their features. `storage.Save` and `storage.SaveTx` accept transaction hooks so event data, projections and work intents can commit together. APIs that receive `*sql.Tx` use the caller's transaction. Community event handlers that accept a persistence callback open the transaction and pass it into the callback.

Consumers declare interfaces around the operations they need. `records.CommunityReader` reads membership and moderation projections through `communityread` values. `relay.Backend` supplies event operations to the WebSocket router. `webui.Backend` supplies management results, and `webui.RoomsReader` supplies typed room pages. The daemon implements these adapters and assembles the feature handlers used by `work.Worker`.

## Event acceptance

Client publication and imported events use the shared event persistence and projection path. Each entry point applies its admission rules and protocol actions. Signed repository state stays pending until its referenced Git objects are available; promotion makes it visible and plans its optional follow-up work.

Callbacks and custom views are optional. A planning intent commits with an accepted event and captures its eligible registrations or failed local lookups. Recovery rechecks the source and current registrations, with at most three worker attempts and a 15-minute age limit. Stable child intent IDs preserve completed work. Remote delivery and transforms have their own retry policies. See [callbacks](agents.md#retries-and-pauses) and [custom views](views.md#failures).

## Browser boundaries

Pages render without JavaScript. `page.html` owns the shell and panel dispatch; each page template owns one content section. Markup uses elements and IDs for styling in `style.css`. Shared form and data elements live in `components.js`, room interactions live in `rooms.js`, and signed publication uses `tiny.signing`. The signing contract verifies returned events before publication. Browser modules describe their entry points and shared services in their source headers.

## Dependencies and checks

Go module versions are declared in `go.mod`, with dependency checksums in `go.sum`. Browser dependencies are locked in `package-lock.json`. `make verify` checks the Go and JavaScript code and verifies generated browser bundles against their sources. See [Testing](testing.md) for local and Linux checks.
