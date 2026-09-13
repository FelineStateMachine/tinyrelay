# Event callbacks

| Field | Value |
| --- | --- |
| Identifier | `callbacks` |
| Status | Experimental |
| Contract version | 1; no negotiated wire version |
| Owner | `internal/daemon` callback service, registration, matching and delivery; host visibility is supplied by `internal/gates`. |
| Wire | Project management methods plus HTTPS POST of one signed Nostr event. No registration event kind. |

The [catalog's audit baseline and transport rules](README.md) apply. [NIP-01](https://github.com/nostr-protocol/nips/blob/master/01.md) supplies events and filter matching; NIP-86/NIP-98 supply the management/authentication foundations. Registration, HMAC headers, retries and quotas are project extensions, not NIP-01 subscriptions or an upstream webhook standard. No upstream revision or full conformance audit is asserted. See the [callback guide](../agents.md#callbacks) for receiver examples.

This contract is **not** the signed push-registration/replication callback implemented by [`internal/replication/push.go`](../../internal/replication/push.go). That service has a different registration, payload and authority model. Sharing the pinned HTTP client does not make the protocols interchangeable.

## Registration and management

Use the tenant-root management endpoint or the corresponding MCP tool at `/mcp`:

| Management method | MCP tool | Parameters/effect |
| --- | --- | --- |
| `listcallbacks` | `list_callbacks` | No parameters; list accessible registrations. |
| `addcallback` | `add_callback` | One object with `url`, `filter`, optional `secret`; creates a registration owned by the authenticated caller. |
| `removecallback` | `remove_callback` | First parameter is an ID string or `{"id":"..."}`; deletes the registration. MCP takes `id`. |
| `pausecallback` | `pause_callback` | Same ID forms; stops further delivery attempts. |
| `resumecallback` | `resume_callback` | Same ID forms; clears pause, failure count and last status. |

Members and agents manage their own registrations. Owner/moderator roles may list and control all registrations; registration always belongs to the caller, not an arbitrary key in the options. Guests are rejected. The per-key policy allowance is `callbacks` (default 4); all stored registrations count, including paused ones. Zero prevents nonowner registration. Only the owner is exempt from the cap. Management mutations are audited.

[`addCallback`](../../internal/daemon/callbacks.go) trims `url` and `secret`. The URL is at most 2,048 bytes after trimming, uses `https`, has a hostname, and has no credentials or fragment. Path, query and explicit port are allowed. Unknown top-level registration object fields are ignored by this decoder; filter fields are stricter.

A supplied nonempty secret must be 16 to 128 bytes of ASCII `!` through `~`: embedded spaces are rejected despite the error's “printable ASCII” wording. An omitted/blank secret generates 32 random bytes encoded as 64 lowercase hex characters. Registration IDs are 16 random bytes encoded as 32 lowercase hex characters. Treat an ID as an opaque registration identifier, not authentication.

Registration returns a summary plus `secret` once. Summary fields are `id`, `owner`, `url`, `host`, `filter`, `created`, `lastDelivery`, `lastStatus`, `failures` and `paused`. Times are Unix seconds. `host` includes an explicit port. List responses omit `url` for registrations owned by someone other than the caller, and never include secrets. Pause/resume return a summary; removal returns `{"id":"...","removed":true}`. **Audit gap:** pause/resume currently return `record.summary()` without the listing's cross-owner URL redaction. Do not interpret the redacted list as a guarantee that an operator cannot retrieve a path/query through a mutation response.

## Filter contract

The filter is a JSON object, or a JSON-encoded object string accepted for form compatibility. Matching uses [`event.Matches`](../../internal/event) through the callback's NIP-01 filter projection: OR within one field's values, AND across fields. Only these keys are accepted:

| Key | Validation |
| --- | --- |
| `kinds` | Required array of 1 to 32 integers, each 0 through 65535. Sorted on storage; duplicates are not removed. |
| `authors` | Optional array of 1 to 8 lowercase 64-character hex public keys. Prefixes are not accepted. |
| `#e`, `#p` | Optional arrays of 1 to 8 lowercase 64-character hex IDs/keys. |
| `#a` | Optional array of 1 to 8 `<kind>:<pubkey>:<identifier>` strings. Kind is a nonempty digit string; pubkey is lowercase 64-character hex. This validator does not bound the kind numerically or require a nonempty identifier. |
| `#h` | Optional array of 1 to 8 room IDs matching `^[a-z0-9_-]{1,64}$`. |
| `since` | Accepted and ignored, without value validation. There is no callback history/backfill cursor. |

Other keys, empty arrays and malformed values are rejected. At registration, an agent must have an active grant; every named `#h` must be a granted room, and recognized repository coordinates in `#a` must have a grant entry. Kinds are deliberately not constrained to the agent's publishing kinds. Omitting a room/repository filter does not grant read access; each event is separately checked by the host gate.

## Delivery and authentication

One delivery is `POST <registered url>` with the signed event object itself as JSON, not a batch, `EVENT` frame or retry envelope. The sender emits:

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `X-Tiny-Callback` | Registration ID. |
| `X-Tiny-Signature` | `sha256=` plus lowercase hex HMAC-SHA256 of the exact request-body bytes, keyed by the secret's bytes. |
| `X-Tiny-Relay` | Configured public relay URL. |
| `User-Agent` | `tinyrelay` |

A receiver should verify the MAC over the raw body with a constant-time comparison before acting, then validate the Nostr event if it needs proof of event authorship. The MAC proves possession of the registration secret, not the event author's identity. Header values other than the body MAC are not separately authenticated by that MAC. Neither `X-Tiny-Relay` nor the event's signature authorizes a local action at the receiver.

The receiver acknowledges with any 2xx status within 10 seconds. Response content is ignored (the sender drains at most 4,096 bytes). Non-2xx responses, timeout and connection failure count as failed attempts. There is no challenge handshake, delivery timestamp, nonce or delivery-attempt header. Receivers must make side effects idempotent, normally using registration ID plus event ID; retries and uncertain acknowledgments can cause duplicates. No exactly-once or strict ordering guarantee is made.

## Lifecycle and trust boundaries

[`callbacks.go`](../../internal/daemon/callbacks.go), [`callbacks_delivery.go`](../../internal/daemon/callbacks_delivery.go) and [`event_followups.go`](../../internal/daemon/event_followups.go) define the lifecycle:

- Stored event acceptance captures eligible registrations for later work. Optional local planning failure does not reject a valid source event. Recovery is bounded by a 15-minute age limit and three background worker planning attempts, separately from delivery retries. Successfully planned child work retains its identity. Failed discovery recovery excludes registrations created at or after the acceptance second, so uncertain same-second registrations can be skipped.
- Repository-state callbacks are planned after object promotion, rather than waking a receiver while objects are missing. Ephemeral events use a separate immediate planning path whose intent carries the event; they do not gain durable source history or the stored-event planning recovery guarantee.
- Before each POST the service rereads the registration, skips missing/paused registrations, checks current membership and applies `Gate.CanSee` to the queued event. A key with no remaining membership has its callback paused. Gate checks include bans, held events, host read policy, rooms and recipients.
- Each callback has at most one in-process POST at a time. A failed event gets two further scheduled attempts, after one minute and then five minutes. The third is final. Each failed HTTP attempt increments consecutive failures; success resets them. At 20 failures the callback pauses. Resumption does not promise replay of skipped or exhausted events.

Outbound URLs use the shared [`NewPinnedClient`](../../internal/replication/callback.go), which dials resolved public addresses directly and refuses redirects. The lexical registration check rejects private/loopback IPs and local-style hostnames. A relay configured with a private/loopback public URL deliberately relaxes private-target restrictions for local use; HTTPS and redirect refusal remain. This is a deployment trust switch, not an exemption clients can request. DNS filtering is the implementation's explicit address-deny list, not a claim of a complete SSRF audit.

Callbacks intentionally disclose the entire readable signed event to an external service. Secrets and URL tokens require protection in storage and at the receiver. Revocation cannot recall an already sent request.

**Read-state limits:** Delivery does not rerun `checkCallbackScope` or `AgentGrant.Active`; `Gate.CanSee` is a read gate, not a write-grant check. Paused/expired agents can retain membership/read access, so pausing a grant alone is not a guaranteed webhook kill switch. Pause the callback to stop it. Delivery checks the queued event payload, not a fresh source lookup; deletion/expiration alone is not a guaranteed cancellation of already planned deliveries. Do not broaden the guide's read-recheck wording into guarantees the implementation does not enforce.

## Errors, discovery and evidence

Registration/shape/quota errors use `invalid:`; role/scope errors use `restricted:`; missing IDs return `not found: callback`. Delivery records bounded status reasons such as `ok`, `HTTP 500`, `timeout`, `connection failed`, or `paused after 20 failures: ...`. Apply the [transport error rules](README.md#shared-transport-boundary).

Discover method names with `supportedmethods`, and MCP names/schemas with `tools/list`; `/llms.txt` also describes the tools. These surfaces do not negotiate callback version 1 or promise permission to register.

Evidence:

- [`callbacks_test.go`](../../internal/daemon/callbacks_test.go): `TestCallbackRegistrationValidatesURLFilterScopeAndCap`, `TestCallbackManagementPermissionMatrix`, `TestCallbackMatchesFilterAndKeepsPrivateEventsBehindTheGate`, `TestCallbackDeliverySignsBodyRetriesAndPauses`, `TestCallbackDeliveryRechecksTheGateAndMembership`, `TestMCPCallbackTools`.
- [`callback_followups_test.go`](../../internal/daemon/callback_followups_test.go): planning failure, preserved completed work, paused/removed/later registrations, exhaustion and independent view failure.
- [`replication/callback.go`](../../internal/replication/callback.go) supplies the shared pinned HTTP client; this audit found no direct pinned-HTTP-client test in the replication test files. The callback restart test in [`callback_restart_acceptance_test.go`](../../internal/daemon/callback_restart_acceptance_test.go) exercises **replication** callback work, not proof of this HMAC webhook's end-to-end restart delivery.

The URL-redaction, grant-state and queued-source limits above are unresolved implementation boundaries. This specification neither fixes them nor claims tests for stronger guarantees.
