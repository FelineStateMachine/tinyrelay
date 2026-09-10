# Architecture

Each tenant has a durable store and a set of services. The daemon constructs those services, admits operations and manages their lifetimes.

## Responsibilities

| Area | Responsibility |
| --- | --- |
| `event` and `policy` | Event formats and shared policy decisions. |
| `storage` | Event persistence, queries and transaction boundaries. |
| `community` | Membership, moderation, rooms and their projections. |
| `records` | Relay identity, signed records and notification records. Community data arrives through a required read interface. |
| `blob` | File storage, upload rules and file HTTP endpoints. |
| `gitrelay` | Repository admission, storage, synchronization, repair and Git HTTP endpoints. |
| `replication` | Replication policy, jobs and delivery handlers. |
| `work` | Durable queue claims, retries and worker lifetimes. The daemon combines handlers from the services. |
| `webui` | Pages, typed room reads and browser interactions. |

Keep schema changes and data access with the service that owns the data. Share a policy function or a small read contract when several services need the same rule or projection. Avoid introducing a general utility package for unrelated behavior.

## Event acceptance

Client publication and imported events share the persistence and projection path. Each entry point retains its own admission rules and protocol actions. Event projections and durable work records commit together.

Callbacks and custom views are optional. Local planning can recover after acceptance, with bounded retries that recheck the source and captured registrations. Completed work keeps its identity and is not reset by planning recovery. Remote delivery and transforms retain their own retry limits. See [callbacks](agents.md#retries-and-pauses) and [custom views](views.md#failures).

## Browser boundaries

Pages render without JavaScript. The page template owns the shell; page templates own their content sections. Generic components live in `components.js`, room interactions live in `rooms.js`, and signed publication uses the shared `tiny.signing` contract. The verifier checks signed events before publication.

Go module versions are declared in `go.mod`, with dependency checksums in `go.sum`. Browser dependencies are locked in `package-lock.json`. `make verify` checks the Go and JavaScript code and verifies generated browser bundles against their sources. See [Testing](testing.md) for local and Linux checks.
