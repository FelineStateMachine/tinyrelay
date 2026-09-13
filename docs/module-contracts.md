# Module boundaries and protocol contracts

The project is a monorepo. `tinyrelay` is the relay host, `tinygit` is the Git engine and standalone Git service, and `tinyclient` is the browser interface and standalone frontend. Separate executables do not imply separate Go modules, databases or network services for every package.

The Git and frontend components, public `protocol/nostr` and `protocol/auth` packages, and `tinygit.ParseMetadata` establish the current boundaries. Blob, room and narrower community/records services below remain proposed follow-up work.

## Keep protocol rules separate from access policy

Apply checks in this order:

1. Parse the wire representation and verify its signature or request proof.
2. Validate the feature's protocol structure and semantics.
3. Apply host policy: membership, grants, visibility, quotas and enabled features.
4. Commit the event, feature projection and required work together.
5. Expose the result through the relevant transport and read model.

A valid Nostr event can still be refused by host policy. A policy refusal is not evidence of an invalid signature or a protocol violation. Conversely, an owner or maintainer role must never make a malformed signed event valid.

The client can validate a draft for feedback and verify a signer's result, but server-side validation remains authoritative. HTTP, WebSocket and MCP adapters must use the same feature operation rather than implement separate admission rules.

## Recommended seams

| Boundary | Owns | Must not own |
| --- | --- | --- |
| Public `protocol/nostr` | Event and filter types, canonical serialization, event IDs, signatures and common encoding rules. | Tenant configuration, membership, SQL or feature-specific tags. |
| Public `protocol/auth` | Verification for HTTP, WebSocket and service-specific proof profiles; authenticated actor and proof scope. | Deciding what that actor may do. |
| Host access policy | Membership, agent grants, visibility, quotas and admission decisions. | Reinterpreting signed fields or weakening feature protocol validation. |
| `tinygit` | Git repository metadata, signed refs, object storage, smart HTTP, repair and Git-specific protocol rules. | Rooms, browser sessions or general relay management. |
| `tinyclient` | Server-rendered pages, browser interactions, draft construction and signer-result verification. | Authoritative permissions, repository mutation or event persistence. |
| Blob service, a candidate for a later `tinyblob` extraction | Content-addressed storage, upload and download contracts, integrity and applicable Blossom behavior. | Site manifests, room membership or a frontend. |
| Site service | Signed site manifests, routing and site lifecycle over a blob-store contract. | Owning another copy of blob storage or upload authorization. |
| Room service | Room state and message projections, with the room protocols it implements. | General agent grants or all community administration. |
| Host assembly | Tenant lifecycle, transaction coordination, worker ownership and adapter wiring. | A second implementation of feature rules. |

Keep domain-specific protocol rules with their feature. Do not create a universal `nips` package containing every numbered NIP: that would recreate the coupling under a new name. A NIP that crosses authentication, transport and persistence boundaries should have one documented ownership map and shared conformance fixtures, not one oversized implementation package.

`records` and `community` are candidates for narrower internal services before another standalone executable. Their existing contracts span several responsibilities; splitting by durable state and operation ownership is more useful than renaming the packages.

## Define the contract before moving dependencies

Each public feature boundary should document:

- Inputs, outputs and stable error categories.
- The authenticated actor and authorization context it accepts.
- Which protocol checks it performs and which policy decisions the host supplies.
- Whether it owns a transaction or joins one supplied by the host.
- Idempotency, ordering and retry behavior.
- Cancellation, resource ownership and shutdown.
- Side effects and the point at which they become visible.
- Capability differences between standalone and integrated operation.

Use small, consumer-owned interfaces for access decisions, persistence and optional transports. Prefer feature-specific read models to a universal backend interface. Moving packages must not break the atomicity of event persistence, projections and durable work.

`tinygit.Event` and the frontend room event type now originate in public `protocol/nostr`. Public `protocol/auth` depends on those primitives, not host services. Internal aliases preserve one implementation, type identity, verifier replay state and error sentinels. Feature kind constants, room-reply helpers and private-kind search exclusions remain internal rather than becoming generic protocol rules.

`tinygit` owns its small policy and persistence contracts. Its default SQLite implementation lives in `internal/gitstore`, and tenant assembly adapts the existing host store and policy. The host retains transaction ownership; the engine does not close a supplied store. External-consumer tests exercise the public store interface, while dependency tests guard the protocol packages. `tinygit.ParseMetadata` validates metadata shape without asserting signature validity or authority; its admission entry points combine these checks before consulting host policy.

## Describe standards and project extensions separately

Protocol documentation should classify each behavior as one of:

- **Upstream protocol:** Behavior specified by a named NIP, GRASP specification, Blossom document or other upstream specification. Cite the document and the revision used by conformance tests.
- **Supported profile:** The subset or combination implemented by a particular executable, with omissions and required options stated explicitly. Do not imply full compliance when required behavior is absent.
- **Project extension:** Additional wire behavior defined by this project. Describe how an ordinary client behaves without it and how peers discover support.
- **Local implementation detail:** Storage paths, queues, indexes or internal APIs that are not an interoperability contract.

For each project extension, create a specification under `docs/extensions/` when its behavior has been audited. Each specification should include a stable identifier, status, version, owning module, upstream references, exact event kinds and tag shapes or endpoint schemas, validation rules, authorization requirements, errors, discovery, security considerations and compatibility rules. Include valid and invalid fixtures and link the implementation and tests.

The [extension catalog](extensions/README.md) indexes specifications for agent grants and grant requests, callbacks, custom-view transforms and the tinyclient HTTP read adapter. These describe implemented behavior and its audited limits; they are not full upstream conformance certificates. Continue auditing remaining custom bindings without repeating each specification in the catalog. Existing behavior is not automatically standardized, and an implementation detail should not be promoted into a wire extension merely because it is unusual.

Keep transport-specific method names in their adapters. A feature operation may have HTTP, MCP and browser bindings, but its domain contract and validation must have one owner. Generate discovery and schema checks from the same definitions where practical; do not create a parallel hand-maintained registry with different defaults.

## Compatibility and terminology

Use `tinyrelay` for the host, `tinygit` for Git functionality and `tinyclient` for frontend functionality in package documentation, command help and architecture descriptions. Reserve “module” for a functional boundary unless explicitly discussing a Go module.

Do not apply blanket replacements to protocol or persisted identifiers. Existing event kinds, tags, endpoints, database schemas, Git hooks, `.tinyrelay` journal paths and `tiny.*` browser APIs remain compatibility surfaces until an explicit migration is designed and tested. A historical storage name can remain while surrounding documentation identifies its current owner accurately.

## Verification

Keep the existing feature tests when moving implementation files. Add external-consumer build checks, standalone service tests and dependency checks that reject importing the host assembly from a standalone component. Use shared protocol fixtures for server validation and browser construction where their responsibilities overlap.

For each extraction, compare the integrated service with the baseline on the same fixtures. Cover valid and invalid events, authorization failures, persistence and restart, HTTP behavior and rendered content. Test the standalone profile separately and state unsupported features; a passing standalone subset is not full integrated parity.
