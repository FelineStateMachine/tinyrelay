# Reviewed grant requests

| Field | Value |
| --- | --- |
| Identifier | `agent-grant-requests` |
| Status | Experimental |
| Contract version | 1; no negotiated wire version |
| Owner | `internal/community` request parsing, patching and replacement validation; `internal/gates` admission/privacy; `internal/daemon` MCP preparation, approvals and history. |
| Wire | Agent-signed kind `1111` with `request=grant`; operator-signed kind `30392` replacement. |

The [catalog's audit baseline and transport rules](README.md) apply. [NIP-22](https://github.com/nostr-protocol/nips/blob/master/22.md) supplies the kind 1111 comment/reference foundation; [NIP-25](https://github.com/nostr-protocol/nips/blob/master/25.md) supplies kind 7 reactions; NIP-01 and NIP-40 supply signed event and expiration foundations. The `grant` request type, JSON patch, provenance and authority rules are project additions to those foundations. No upstream revision or full NIP-22 conformance audit is asserted. This is not the general-purpose approval convention or upstream delegation. See [Agent identities](../agent-access.md#requesting-additional-access) for the workflow.

## Request envelope

The request is signed by the agent named by the active current [grant](agent-grants.md). Required tags are:

```json
[
  ["E", "<current grant event ID>", "", "<operator pubkey>"],
  ["K", "30392"],
  ["P", "<operator pubkey>"],
  ["e", "<current grant event ID>", "", "<operator pubkey>"],
  ["k", "30392"],
  ["request", "grant"],
  ["grant", "{\"base\":\"<current grant event ID>\",\"changes\":{\"kinds\":[9]}}"],
  ["p", "<operator pubkey>"]
]
```

This is the builder's tag shape, with placeholders, not a signed event. The content is the reason: valid UTF-8, nonblank after trimming, at most 500 Unicode code points. Direct event validation counts the original content; the MCP builder trims it before constructing the event.

[`decodeGrantRequest` and `ReviewAgentGrantRequest`](../../internal/community/agent_grant_requests.go) enforce:

- Kind `1111` and first `request` value exactly `grant`.
- Only `request`, `grant`, `E`, `K`, `P`, `e`, `k`, `p`, `subject` and `expiration` tag names. Any repeated tag name or unsupported/empty tag is rejected. `subject` and `expiration` are optional; no extension-specific subject length check is made.
- `grant` contains one JSON value with fields `base` and `changes`. Unknown struct fields or trailing JSON values are rejected. Duplicate JSON object keys are not separately rejected by the Go decoder; clients should emit unique keys.
- `base` equals both `E` and `e`. The reference is 64 characters and must resolve to the current grant; `K` and `k` are `30392`; `P` and the one `p` name the current grant signer. Reference relay hints and additional positions are not fully NIP-22-validated here; clients should use the builder shape above.
- The stored current grant belongs to the requesting agent and is active. A human role with a grant cannot use the narrow agent-request admission path. Requests without grants are rejected even when general comment publishing is allowed.
- At least one nonnull change field is present, and the resulting parsed scope differs from the original under ordered `reflect.DeepEqual` comparison. Empty arrays alone or adding only existing kinds/rooms cannot satisfy the no-op check. Repository/site replacements remove matching tags and append requested entries, so reordering alone can pass even when permissions are unchanged.

The optional positive `expiration` ends review eligibility. Malformed/nonpositive expiration values map to no expiry through [`event.Expiration`](../../internal/event/event.go); this parser does not add a stricter expiration syntax check. Ordinary event admission still rejects already expired requests.

## Changes and replacement construction

| JSON field | Accepted value and operation |
| --- | --- |
| `kinds` | Integer array; add entries, retaining existing kinds. Resulting grant kinds must be from 0 through 65535. |
| `rooms` | String array; add entries. Each requested room must match `^[a-z0-9_-]{1,64}$`, stricter than a direct grant's room parser. |
| `repos` | Objects with `owner`, `identifier`, `level`; replace entries matched by raw owner/identifier, retaining other repositories (see compatibility limits below). Repeated identities in a request are rejected. Resulting entries must pass the grant parser. |
| `sites` | Objects with `label`, optional integer `ttl` and boolean `encrypted`; replace entries matched by raw label, retaining others (see compatibility limits below). Duplicate labels and negative TTLs are rejected. Omitted/zero TTL means no TTL in the signed-event path; positive TTL is 1 through 365. The label must belong to the agent or be `*`. |
| `wiki` | String replacing the field: empty, `propose` or `edit`. |
| `jobs` | String replacing the field: empty, `request`, `serve` or `both`. |
| `rate` | Integer replacing the field, 1 through 600. |

All resulting lists and fields pass [`ParseAgentGrant`](../../internal/community/agents.go), including its 256-entry caps. Null pointer fields mean no replacement. There is no request patch for the agent, operator, name, expiration or grant content. Kinds/rooms cannot remove entries; repository/site entries can change an existing permission, and empty wiki/jobs can clear those fields. “Additional access” is a use case, not a rule that every patch must increase permissions.

[`grantRequestTags`](../../internal/community/agent_grant_requests.go) copies retained tags in order, removes previous request provenance, adds requested kinds then rooms then repositories then sites, and appends supplied wiki/jobs/rate values. It then appends:

```json
[
  ["grant-request", "<request event ID>"],
  ["grant-base", "<current grant event ID>"],
  ["e", "<request event ID>", "", "grant-request"]
]
```

The unsigned replacement retains the original operator, kind, name, expiration and content. Its ID/signature are cleared and its timestamp is advanced past the base grant if necessary. Clients should sign the reviewed template without reordering or normalizing tags: validation compares the complete ordered tag array as well as parsed scope, identity, name, expiration and original content.

## Authority and lifecycle

1. `request_grant` over `POST /mcp` with `reason` and `changes` derives the current base/operator and returns `unsigned` plus `next`. This prepares but does not publish. Its schema is in [`mcp_grant_request.go`](../../internal/daemon/mcp_grant_request.go).
2. The agent signs the event. Calling `request_grant` with `event` requires its `pubkey` to match the authenticated actor, reviews it again and uses normal publication. WebSocket `EVENT` and `POST /events` enforce the same feature admission through the gate. This narrow path does not require `k=1111`; the agent rate limit and host restrictions still apply.
3. The request can be read only by its author or the `p` operator, subject to other read checks. Relay-owner/moderator standing alone does not bypass this participant check in [`Gate.CanSee`](../../internal/gates/gates.go). Notifications direct the operator to review, not one-tap grant approval.
4. Approval is a separately signed replacement grant from that operator, who must still have grant authority. A `+` reaction never changes access and does not settle the request as approved. Agents cannot approve themselves.
5. Any stored, unexpired kind 7 reaction from the operator with an `e` reference to the request and trimmed content `-` blocks replacement. This is not a newest-reaction-wins rule. A later `+` does not undo a still-stored denial.
6. Replacement requires the request to be stored, not expired or denied; its base to remain current; the agent grant to remain unpaused, unrevoked and unexpired; and the exact reviewed replacement fields/tags. Validation runs at admission and again in [`ApplyAgentEventTx`](../../internal/community/agents.go), inside the event/grant transaction. A prepared template is not a reservation on current authority.
7. Approvals report `open`, `answered` with decision `approved`/`denied`, or `expired`. A superseded base produces a conflict and is rendered expired; inactive requests can carry `grant_error` with no actionable review. [`grant_approvals.go`](../../internal/daemon/grant_approvals.go) consults archived grant revisions so a later grant edit does not erase the historical approval receipt. A historical approved receipt does not prove the permission is still effective.

Private visibility is relay access control, not encryption. The operator and relay can read the reason and requested scope; signed copies may exist outside this relay. Do not put secrets in a rationale.

## Errors and discovery

Error families include `invalid:` for malformed/no-op patches or replacement differences; `restricted:` for a missing/inactive grant, wrong requesting role or replacement operator; and `conflict:` for stale, expired or denied requests. Examples are `invalid: grant request changes nothing`, `conflict: agent grant changed since the request` and `invalid: grant replacement tags differ from the reviewed request`. MCP preparation/submission failures set `isError` and include an `expected` shape; an unsigned preview is not a successful write. Apply the [transport error rules](README.md#shared-transport-boundary).

Discover `request_grant` with MCP `tools/list`. `browseapprovals` and `browseapproval` are daemon read queries used by the approvals UI; the latter supplies the `grant` review (`request_id`, `base`, `agent`, `operator`, `before`, `after`, `unsigned`) or `grant_error`. There is no dedicated grant-request version capability; the relay's `agents` information entry describes grants generally, not caller eligibility.

## Compatibility limits and evidence

- MCP's advertised plain-field schema requires nonempty wiki/jobs strings and a site TTL of at least 1, while signed-event review permits empty wiki/jobs and omitted/zero TTL. Do not assume schema acceptance equals event admission or that both paths expose every clearing operation.
- Direct grants trim repository strings and site labels, and repository identifiers may contain colons. The request patch remover matches raw tags without trimming and splits repository strings differently (`SplitN(..., 3)`). A normalized repository/site identity may therefore fail to match the old tag; the retained entry takes precedence over the appended entry when parsed. This can make the request a rejected no-op or leave that replacement unapplied within an otherwise accepted patch. For example, an existing `["sites", " * ", "ttl=7"]` plus a request adding a new kind `9` and setting site `*` to TTL 3 is accepted but retains effective TTL 7. Inspect the reviewed effective scope, not just the requested changes. This audit documents the mismatches; it does not redefine identities or fix runtime behavior.
- Request JSON and NIP-22 references have the decoder/positional validation limits noted above. No independently versioned JSON schema or exhaustive upstream conformance suite is claimed.

Evidence: [`community/agent_grant_requests_test.go`](../../internal/community/agent_grant_requests_test.go) tests retained entries and patch ordering; [`grant_admission_test.go`](../../internal/daemon/grant_admission_test.go) tests malformed requests, role/state restrictions, exact replacement and stale review; [`grant_approvals_test.go`](../../internal/daemon/grant_approvals_test.go) tests no-kind-1111 admission, participant reads, ineffective `+`, successful replacement/history, denial and supersession; [`mcp_grant_request_test.go`](../../internal/daemon/mcp_grant_request_test.go) tests operator-bound preparation, invalid changes and schema behavior.
