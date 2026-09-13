# Agent grants

| Field | Value |
| --- | --- |
| Identifier | `agent-grants` |
| Status | Experimental |
| Contract version | 1; no negotiated wire version |
| Owner | `internal/community` parses grants and owns their effective state; `internal/gates` enforces admission; `internal/daemon` binds management, storage and feature authority. |
| Wire | Kind `30392`; address `30392:<operator pubkey>:<agent pubkey>`. |

The [catalog's audit baseline and transport rules](README.md) apply. Foundations are [NIP-01](https://github.com/nostr-protocol/nips/blob/master/01.md) signed/addressable events, [NIP-09](https://github.com/nostr-protocol/nips/blob/master/09.md) deletion, [NIP-40](https://github.com/nostr-protocol/nips/blob/master/40.md) expiration, and NIP-86/NIP-98 management. Kind 30392, its grant tags, the agent role and the management methods below are project conventions, not an upstream delegation standard. No upstream revision or complete conformance audit is asserted. The user guide is [Agent identities](../agents.md).

## Event fields

The agent remains the author and signer of its own events. The grant's `pubkey` is the operator, not the agent. [`ParseAgentGrant`](../../internal/community/agents.go) validates the following fields; signer authority is a separate check.

| Field or tag | Version 1 validation and interpretation |
| --- | --- |
| `kind` | Exactly `30392`. |
| `d` | Required agent public key: 64 lowercase hexadecimal characters. Must not equal the event's signer. |
| `p` | Required; its first value must equal `d`. |
| `expiration` | Required positive, signed 64-bit Unix timestamp; no later than the admission time plus 365 days. A timestamp at or before admission time is allowed as revocation, unlike ordinary expired events. |
| `content` | Empty or any valid JSON value, not necessarily an object. No permission is derived from it. |
| `name` | Optional; trimmed and truncated to 64 **bytes**, not rejected for excess length. |
| `room` | Repeated, trimmed strings of 1 to 128 bytes; deduplicated after trimming. The grant parser does not require the narrower room ID syntax. |
| `repo` | Repeated `<owner>:<identifier>:<level>`. Owner is a lowercase 64-character hex key; identifier is 1 to 256 bytes; level is exactly `propose`, `read` or `maintain`. The final colon separates the level; the identifier may contain colons. Outer whitespace is trimmed. First entry for an owner/identifier wins. |
| `k` | Repeated decimal integers from 0 through 65535, parsed after trimming; deduplicated numerically. |
| `wiki` | Absent/empty, `propose` or `edit`. |
| `jobs` | Absent/empty, `request`, `serve` or `both`. |
| `sites` | Repeated `["sites","<label>",...]`; trimmed label is `*` or a site under the agent's key accepted by [`sites.ParseSite`](../../internal/sites). Snapshot labels are rejected. Subsequent trimmed values may only be `encrypted` or `ttl=<days>`, with days from 1 through 365. Repeated TTL flags use the last value; first entry for a label wins. |
| `rate` | Absent/empty defaults to 60 events per minute; otherwise a trimmed decimal integer from 1 through 600. |

Each deduplicated list of rooms, repositories, kinds or sites is limited to 256 entries. Scalar tags use the first tag with a value, as defined by [`event.Tag`](../../internal/event/event.go). Grant parsing does not reject unknown tags or duplicate scalar tags. Clients should emit one scalar tag per field and unambiguous list entries; the stricter [grant-request](agent-grant-requests.md) rules are not interchangeable with this parser. Original signed tags and content are preserved, even when the effective projection trims or deduplicates them.

## Signature, authority and state

[`agentGrantShape`](../../internal/gates/gates.go) requires the signer's current role to be `owner` or `moderator`. A valid signature or successful `ParseAgentGrant` call alone does not confer this authority. Host bans, allowed kinds, room membership and feature rules still apply. The same grant gate is used for client writes and imports.

[`ApplyAgentEventTx`](../../internal/community/agents.go), bound by [`event_commit.go`](../../internal/daemon/event_commit.go), mirrors a stored grant and the membership change inside the event transaction. A fresh active grant resets pause/revocation and adds role `agent` only if the key has no existing human role. A human member, moderator or owner retains that role and is not restricted by the grant. A revoked grant's record remains available for audit; its `agent` membership row is removed. Grant admission still checks retained grant state for a key with no role, preventing revoked keys from escaping the grant gate merely by losing membership.

A grant is active only when not paused, not revoked and `expires > now`. A new accepted grant replaces the effective grant for that agent. Nostr addressable-event replacement is scoped by kind, signer and `d`, but the effective grant table is keyed only by agent. Do not infer a cross-operator “newest timestamp wins” rule from NIP-01: different authorized operators can publish different addresses for the same agent, and the accepted grant application updates that one effective row.

Revocation can use:

- A kind 5 deletion from the current grant's signer naming its event ID in `e`, or its address in `a`.
- A newly accepted kind 30392 grant with an expired positive `expiration`.
- The privileged `revokeagent` management method.

Pause/resume are local management state, not signed grant replacements or portable Nostr events. A fresh active grant restores access; resuming a revoked grant is rejected. Request-linked replacements must also pass the provenance and stale-base rules in [Reviewed grant requests](agent-grant-requests.md).

## Scope enforcement

[`AgentGrant.Check` and `AllowsKind`](../../internal/community/agents.go), [`agentAdmission`](../../internal/gates/gates.go) and [`jobReply`](../../internal/gates/jobs.go) establish these rules:

- Profiles (`0`), relay lists (`10002`) and authentication (`22242`) pass the grant's kind test. An inactive grant still fails the state test. Other kinds require a `k` entry or the derived permission below.
- `wiki=propose` admits `30818` and `818`; `wiki=edit` adds `30819`. Proposal visibility and decisions belong to the [wiki approval integration](../../internal/daemon/wiki_approvals.go), not event signature validation.
- `jobs=request` admits requests `5000` through `5999`, excluding reserved kind `5128`; `serve` admits results `6000` through `6999` and feedback `7000`; `both` combines them. Agent answers require a locally held, readable request and valid request references. See [`jobs_test.go`](../../internal/gates/jobs_test.go).
- `sites` admits `15128` and `35128`, not snapshot kind `5128`. A manifest must match a covering label even if its kind was explicitly granted. When every covering entry has a TTL, its `expiration` must fit the longest covering TTL measured from admission time; an entry with no TTL removes that bound.
- A nonempty first `h` value must be in the grant's rooms. This does not bypass room existence, membership or administration rules.
- Repository collaboration kinds `1617`, `1618`, `1619`, `1621`, `1111` and `1630` through `1633` are checked against valid `30617:<owner>:<identifier>` coordinates in `a` and `A` tags. Every recognized coordinate needs a grant entry. All except a comment (`1111`) require such a coordinate. Status kinds require `maintain`; all still require kind permission.
- Repository announcements/state (`30617`, `30618`) have their own Git admission and maintainer rules. An active `maintain` grant contributes repository authority through [`repository_maintainers.go`](../../internal/daemon/repository_maintainers.go); it does not implicitly add kind 30618. Private Git membership checks remain separate.
- The rate limiter is a sliding 60-second admission counter, shared by writes/imports through the gate and reset by process restart. It is not a persistent count of successfully stored events.

Site upload TTL/encryption effects use [`agentUploadGrant` and `blobUploadTerms`](../../internal/daemon/services.go) and the blob service, not grant event signatures. `encrypted` on any site entry applies to all agent uploads; it is a MIME-based acceptance rule, not cryptographic proof of encryption. Grant scope is not a general read-access control list.

## Management and discovery

Owner and moderator methods are `listagents`, `pauseagent`, `resumeagent`, `revokeagent`, `pauseallagents` and `resumeallagents`. Single-agent methods take the public key as their first positional string. Corresponding MCP names are `list_agents`, `pause_agent`, `resume_agent`, `revoke_agent`, `pause_all_agents` and `resume_all_agents`. See [`agentExecute`](../../internal/daemon/management.go) and [`mcp_tools.go`](../../internal/daemon/mcp_tools.go).

`listagents` returns entries with `pubkey`, `owner`, `event_id`, `name`, `expires`, `paused`, `revoked`, `scope` and `lastEvent`. Scope uses `kinds`, `rooms`, `repos`, `rate` and optional `wiki`, `jobs`, `sites`; repository objects use `owner`, `identifier`, `level`, and site objects use `label`, optional `ttl` and `encrypted`. These are effective projections, not replacement event templates.

While an active grant exists, [`Capabilities`](../../internal/daemon/capabilities.go) adds `{"id":"agents","status":"enabled","reason":"agent identities under owner-signed grants"}` and [`information`](../../internal/daemon/http.go) includes the `agents` entry in relay information. This does not enumerate grants, encode version 1 or establish caller permission. Absence can mean no active grants, not that the implementation lacks this extension.

## Errors, compatibility and evidence

Malformed grants return `invalid: agent grant ...`; unauthorized signers return `restricted: only the owner or a moderator can grant an agent`. Inactive or out-of-scope agent writes return `restricted: agent grant ...`; site TTL violations return `invalid: agent grant allows site ...`. Preserve the message and apply the [transport-specific error rules](README.md#shared-transport-boundary).

Evidence:

- [`gates/agents_test.go`](../../internal/gates/agents_test.go): `TestAgentGrantShapeAndSigner`, `TestAgentGrantScopesKindsRoomsAndRepositories`, `TestAgentGrantStateRejectsWrites`, `TestAgentGrantRateLimitSlidesOverOneMinute`, `TestHumanMemberWithGrantKeepsMemberPermissions`, `TestImportEnforcesAgentGrant`, and site shape/expiry cases.
- [`daemon/agents_test.go`](../../internal/daemon/agents_test.go): `TestAgentGrantPublishCreatesRoleRowAndRevokesOnDeletion` and operator management/read cases.
- [`git_agent_maintainer_test.go`](../../internal/daemon/git_agent_maintainer_test.go) and [`blob_authorization_test.go`](../../internal/daemon/blob_authorization_test.go): host-specific Git and upload authority.

Audit limits: The name's byte truncation can split UTF-8 and differs from the guide's character wording. Direct grant room syntax is broader than request room syntax. Cross-operator replacement ordering is not a portable conflict-resolution protocol. These are documented behavior/ambiguities, not runtime changes or new conformance claims.
