# Project extensions

This catalog indexes project-defined interoperability contracts. An extension is not a claim of upstream NIP, GRASP or Blossom standardization. See [Module boundaries and protocol contracts](../module-contracts.md) for the distinction between upstream rules, supported profiles, project extensions and implementation details.

| Identifier | Contract owner | Status | Specification |
| --- | --- | --- | --- |
| `tinyclient-read-adapter` | `tinyclient` presentation contract; tinyrelay data and authorization | Experimental, revision 1 | [Read-only frontend/backend HTTP adapter](tinyclient-read-adapter.md) |
| `agent-grants` | `internal/community` grant semantics; `internal/gates` admission; `internal/daemon` integration | Experimental, version 1 | [Agent grants](agent-grants.md) |
| `agent-grant-requests` | `internal/community` review and replacement; `internal/gates` admission; `internal/daemon` MCP and approvals | Experimental, version 1 | [Reviewed grant requests](agent-grant-requests.md) |
| `callbacks` | `internal/daemon` callback service and management | Experimental, version 1 | [Event callbacks](callbacks.md) |
| `tinyagent` | Hermes connector; `internal/daemon` request validation; `tinyclient` presentation | Experimental, version 1 | [Native agent interactions](tinyagent.md) |
| `custom-view-transforms` | `internal/daemon` transform service; `internal/views` block identity; `internal/records` relay signing | Experimental, version 1 | [Custom-view block transforms](custom-view-transforms.md) |

## Status and compatibility

The four version 1 specifications document existing implementation behavior audited at repository baseline [`9d68e8778c3448fdc08c95e7b44bb30732692f83`](https://012.run/repo?owner=c01503e8035f5a98eb2004721dc4eeddda498c7871927ae7edb4525ab6ccf2d7&repo=tinyrelay&ref=9d68e8778c3448fdc08c95e7b44bb30732692f83&view=home). Relative source and test links identify the owners in this stack. Their identifiers and versions are documentation identifiers, not new wire fields. There is no negotiated extension version, extension handshake or independent conformance certification. Use compatible client and relay revisions; changing existing field meanings, authority or persisted identifiers requires a compatibility review, not a naming-only refactor.

“Must” and “rejects” in these specifications describe the audited contract, not new runtime requirements. An explicit audit limit takes precedence over broader wording in a feature guide. The catalog remains incomplete: unlisted project bindings and proposed reusable view definitions are not implicitly covered. The upstream standards audit is also incomplete. References below identify foundations, but this audit did not verify an upstream commit or establish full conformance to those documents. A NIP number advertised by the relay is discovery, not proof that every provision is implemented.

## Shared transport boundary

Signed grant and request events use the normal Nostr event envelope and publication path: WebSocket `EVENT`, `POST /events` with one event JSON object, or an MCP publishing tool. Event ID/signature validation proves authorship; the feature's current authority checks decide whether the event may take effect. The relay does not sign as an agent or convert a request into an operator grant.

Management methods in these specifications are project methods carried through the [NIP-86](https://github.com/nostr-protocol/nips/blob/master/86.md) management envelope. Send `POST /` at the tenant root with `Content-Type: application/nostr+json+rpc` and `{"method":"<method>","params":[...]}`. Except for `supportedmethods`, the HTTP handler resolves the actor from authentication. [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) proofs are bound to the actual URL, method and body; a supplied public key is not authority. HTTP routing recognizes the management media type after dedicated routes, so clients should use the root, not invent a feature-specific management endpoint.

Management HTTP success is `{"result":...}` with status 200; errors are `{"error":"<reason>"}`. Authentication failures use 401; `restricted:` and `blocked:` method errors use 403; other method errors use 400. Event publication instead returns `{"event_id":"...","accepted":true|false,"message":"..."}` with status 200 or 400 after parsing/authentication. Clients must inspect `accepted`, not assume all error prefixes map to the management HTTP status rules. MCP tools use `POST /mcp`; inspect `isError` and the publishing result, not HTTP success alone. The MCP transport revision is separate from these extension versions; see [MCP](../mcp.md).

Sources: [`http.go`](../../internal/daemon/http.go) (`ServeHTTP`, `bridge`, `httpPublish`, `manageHTTP`, `Execute`), [`mcp_tools.go`](../../internal/daemon/mcp_tools.go), [`mcp_http.go`](../../internal/daemon/mcp_http.go), [`relay.go`](../../internal/relay/relay.go) and [`event.go`](../../internal/event/event.go).

## Ownership rules

- Feature owners define field parsing and semantic validation. Shared event and storage code does not confer operator, repository or tenant authority.
- The daemon binds authentication, host policy and feature services. An internal parser or unsigned-event builder alone is not an admission boundary.
- The caller that stores an accepted event owns its transaction. Grant effects participate in that transaction; optional callback and transform planning must not turn a valid source event into a rejected publish. Remote delivery is subsequent work, not part of the source event's atomic commit.
- Test links are evidence for the stated behavior, not a declaration of an exhaustive test suite. Gaps and differences between guides, schemas and implementation are recorded in each specification.
