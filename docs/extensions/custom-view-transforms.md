# Custom-view block transforms

| Field | Value |
| --- | --- |
| Identifier | `custom-view-transforms` |
| Status | Experimental |
| Contract version | 1; no negotiated wire version |
| Owner | `internal/daemon` definitions, scheduling, transform HTTP, artifacts and access; `internal/views` fenced blocks/hash/path contract; `internal/records` relay signing identity. |
| Wire | Project management methods, HTTPS JSON transform exchange, relay-signed kind `30078` artifacts and `/views/` media routes. |

The [catalog's audit baseline and transport rules](README.md) apply. [NIP-78](https://github.com/nostr-protocol/nips/blob/master/78.md) supplies kind 30078 application data, with NIP-01 addressable events, NIP-40 expiration and NIP-86/NIP-98 management foundations. The `bind.ws/view/` namespace, source/block tags, transform exchange and audience rules are project-defined. Neither the namespace nor the `X-Transform-*` name claims upstream standardization. No upstream revision or complete conformance audit is asserted.

This contract renders fenced code blocks. It is not the [reusable view proposal](../view-definitions.md), a generic whole-event transform or the built-in `listviews` summaries. The [Custom views guide](../views.md) covers operation and presentation.

## Definition and authority

All methods, including listing, require the current `owner` role; moderators, members and agents cannot configure a transform. A permitted author may still trigger an owner-configured write view by publishing matching source content. Defining a view authorizes disclosure of those blocks to that external transform.

| Management method | MCP tool | Effect |
| --- | --- | --- |
| `listcustomviews` | `list_custom_views` | Lists definitions without secrets. |
| `addcustomview` | `add_custom_view` | Takes one definition object; creates a new named view and returns its secret once. Existing names are rejected, not updated. |
| `removecustomview` | `remove_custom_view` | Deletes the view and its artifacts. |
| `pausecustomview` | `pause_custom_view` | Disables transform requests; retains artifacts. |
| `resumecustomview` | `resume_custom_view` | Enables the view and clears failure count/status. |
| `runcustomview` | `run_custom_view` | Queues a bounded backfill; `refresh=true` rebuilds existing artifacts. A paused view must be resumed first. |

Use the [shared management transport](README.md#shared-transport-boundary). Named methods accept a first positional name string or `{"name":"..."}`; run also accepts `{"name":"...","refresh":true}`. MCP uses named `name` and optional `refresh` fields. Source: [`custom_views.go`](../../internal/daemon/custom_views.go) and [`mcp_tools.go`](../../internal/daemon/mcp_tools.go).

| Definition field | Version 1 validation |
| --- | --- |
| `name` | Trimmed, matches `^[a-z0-9-]{1,32}$`. |
| `kinds` | 1 to 16 integers, each 0 through 65535; sorted/deduplicated after the count check. |
| `transform` | Trimmed HTTPS URL using the [callback URL restrictions](callbacks.md#registration-and-management), including 2,048-byte limit and no credentials/fragment. |
| `languages` | 1 to 32 entries, then trimmed/lowercased/deduplicated; each is 1 to 32 bytes using `a-z`, `0-9`, `+`, `-`, `_`, `.` or `#`. |
| `trigger` | Trimmed `write` or `hourly`; absent/empty defaults to `write`. |
| `audience` | Trimmed `public` or `members`; absent/empty defaults to `public`. |
| `max_bytes` | Artifact-byte cap, 1 through 4 MiB; omitted, null or empty string defaults to 1 MiB. Integer or numeric string is accepted by management. |
| `secret` | Same trimmed 16-to-128-byte nonspace ASCII rule as callbacks; generated as 64 lowercase hex characters if omitted/blank. |

Management additionally accepts kinds as numeric strings or a delimited string and languages as a delimited string (comma, space or newline separators). The MCP schema is narrower; clients should use arrays and integers. Unknown definition object fields are ignored by the management decoder.

Summaries contain `name`, `kinds`, `transform`, `host`, `trigger`, `audience`, `languages`, `maxBytes`, `enabled`, `state` (`active`/`paused`), `created`, `lastRun`, `lastStatus` and `failures`. `max_bytes` is input spelling; `maxBytes` is output spelling. Run adds `queued`; removal returns `name` and `removed:true`. Neither summaries nor an enabled view imply a completed render.

## Block selection and identity

[`internal/views/views.go`](../../internal/views/views.go) defines the shared wire/cache identity:

- A fence opens with at least three backticks or tildes after trimming the line. The first word of the remaining info string, lowercased, is the language. A closing fence contains only the same marker character with at least the opening run's length. An unterminated fence consumes the remaining text.
- Input CRLF becomes LF. Block source is the lines between fences joined with LF, without an added trailing newline. Index is zero-based across **all** fenced blocks, including unnamed and nonmatching languages.
- Only nonempty languages in the view's configured list are sent. Ordinary source is event content. Kind `30618` instead reads the repository README at the commit referenced by its `HEAD` tag, trying `README.md`, `readme.md`, then `README`; missing/binary/empty files are skipped.
- Hash is lowercase hexadecimal SHA-256 of UTF-8 bytes `lang + "\n" + source`. It is not an artifact-body hash. Cache identity is view name plus hash; equal blocks across events share one artifact. Normal runs reuse existing artifacts; refresh bypasses reuse.

## Transform request and answer

One POST per source carries only pending matching blocks:

```json
{"relay":"https://relay.example","view":"diagrams","source":{"id":"<source key>","kind":30818},"blocks":[{"index":2,"lang":"mermaid","source":"graph TD;\n  A-->B;"}]}
```

`source.id` is the event ID except for kind 30618, where it is the address `30618:<state author pubkey>:<d>`. That special form exposes the repository state author/identifier and is not a 64-character event ID. No full event, separate tags, author or timestamp fields are transmitted; block content and source identity still can disclose private information.

Headers are `Content-Type: application/json`, `Accept: application/json`, `User-Agent: tinyrelay`, `X-Transform-View`, `X-Transform-Relay` and `X-Transform-Signature`. Their values are the view name, configured public relay URL, and `sha256=` plus lowercase hex HMAC-SHA256 over the exact body bytes with the view secret. Legacy `X-Tiny-View`, `X-Tiny-Relay` and `X-Tiny-Signature` carry identical values. New integrations should use `X-Transform-*`; existing aliases remain part of this version.

Receivers should verify the body MAC in constant time before rendering. This proves shared-secret possession, not source author approval or semantic correctness. No timestamp/nonce or response signature is negotiated. TLS authenticates the response endpoint. The relay waits up to 30 seconds, refuses redirects and uses the shared pinned-client address restrictions, with the configured local/private-host exception described in [Callbacks](callbacks.md#lifecycle-and-trust-boundaries).

A successful endpoint returns a 2xx status and JSON such as:

```json
{"artifacts":[{"block":2,"type":"image/svg+xml","body":"<svg ...></svg>","engine":"mermaid"}],"errors":[{"block":5,"error":"unknown shape"}]}
```

`artifacts` and `errors` are arrays of the shown objects. No extra response fields are required; unknown object fields are ignored. The body read is limited to `4 * (4 MiB) + 1 MiB`; truncated/malformed JSON is a run failure. `block` must identify a block actually sent. Unsent-block responses, duplicate successful blocks and invalid artifacts are refused individually. A response with no valid artifacts is a run failure. Missing artifacts leave code (or a prior artifact on refresh) available. `errors` is counted for diagnostics, not a signed decision or automatic retry directive.

### Artifact validation

[`checkArtifact` and `checkSVG`](../../internal/daemon/custom_views_validation.go) validate the media before [`storeArtifact`](../../internal/daemon/custom_views_transform.go) saves it:

- The type must be exactly `image/svg+xml` or `image/png`.
- SVG body length must fit `max_bytes`. XML must contain one SVG root. Scripts, event handlers, embedded documents, animation, document type declarations and stylesheet instructions, external link targets and external CSS `url()` references or imports are rejected. Links may target internal `#` fragments. CSS escapes and nested elements within styles are refused. The original body is retained without rewriting.
- PNG uses trimmed Base64 with the standard alphabet, padded or unpadded. Decoded bytes must fit `max_bytes`, the image must decode successfully, and dimensions must total at most 16,777,216 pixels. Storage normalizes Base64 padding.
- `engine` is optional, trimmed and truncated to 64 bytes before recording.

SVG is served under the sandbox policy below. Acceptance does not authorize arbitrary SVG execution or attest to the transform's interpretation of source code. A run records `ok` when all requested blocks produce valid artifacts, `partial` when some do, or `no valid artifacts` when none do. Partial success clears the failure count; no valid artifacts follows the normal retry and pause rules.

## Signed records and media routes

[`signArtifact`](../../internal/daemon/custom_views_transform.go) asks [`internal/records`](../../internal/records/identity.go) to sign kind 30078 with the durable relay identity, not the tenant operator or source author. The record content is SVG text or padded Base64 PNG. Tags are:

- `["-"]`, `["d","bind.ws/view/<name>/<hash>"]`, `["view","<name>"]`.
- One `e` source tag for an ordinary event ID, or `a` source tag for a repository-state address, for each recorded source.
- One `["block","<decimal index>"]` for each source, in the same source order. These block tags follow the source tags; they are not adjacent source/block pairs.
- `["type","image/svg+xml"]` or `["type","image/png"]`, optional `engine`, and optional `expiration`.

If all sources expire, artifact expiration is the latest source expiration. Any nonexpiring source removes the expiration tag. Source deletion/replacement/maintenance detaches obsolete references and removes an orphan artifact; pending transform work can defer pruning. Media and signed-record reads reject expired sources even before maintenance prunes them. Removing a view deletes all its artifacts. Refresh retains prior artifacts until valid replacements arrive.

Public artifacts use `records.Generate` and enter normal event storage. Members-only artifacts use `records.Sign` and remain signed in the artifact store without being published to normal event queries. The signature attests to the relay's generated output, not endorsement by any source author.

`GET` and `HEAD /views/<name>/<hash>` serve the stored media type; `.svg` and `.png` aliases are accepted only when they match that type. Invalid/missing paths or type mismatches return 404; other methods return 405. Authentication failures return 401; members-audience requests require role `member`, `moderator` or `owner` and otherwise return 403. **Role `agent` is not admitted by this members-only route.** Public artifacts require the relay's general browse-read rule. Both audiences also require a current, unexpired source visible to the requester. A repository-state source must still contain a block matching the artifact's hash. If no source qualifies, the route returns 404.

Responses set stored `Content-Type`, `Content-Length`, `X-Content-Type-Options: nosniff`, and `Content-Security-Policy: sandbox; default-src 'none'; style-src 'unsafe-inline'`. `ETag` is quoted lowercase SHA-256 of served bytes. Exact `If-None-Match` equality yields 304 after access checks. Cache control is `private, no-cache` because access depends on the requester and current source state. See [`custom_views_http.go`](../../internal/daemon/custom_views_http.go).

Signed artifact records require access to every source named in their tags, and the tags must match the current source records. An image shared by several sources can be served through any readable source because its bytes are identical; the signed record stays hidden when it would disclose another source. Source checks also apply through nested artifact references and reject cycles.

Transformation sends blocks from current, unexpired sources to the owner-configured endpoint. Reader access checks do not prevent that outbound disclosure. Configure transforms only for content you intend to send to their endpoints.

## Scheduling, errors and discovery

Write views capture registrations and queue optional work after source acceptance; repository-state work waits for available objects. [`event_followups.go`](../../internal/daemon/event_followups.go) bounds failed planning recovery separately from HTTP retries and excludes later/uncertain same-second registrations during failed-discovery recovery. Registration generation/fingerprint checks prevent old work from targeting a newly created view with the same name.

Hourly/manual runs consider at most the newest 500 events of the selected kinds; an hourly pass applies a last-run time window with overlap. This is not an unbounded backfill or guarantee to process every event. Transforms serialize per view. Before a POST, the worker checks enabled state, registration generation and whether the source is still current. Those checks do not recall an in-flight request.

Non-2xx, timeout, connection/read failure, invalid JSON or no valid artifacts produces a failed run. Two retries follow, after one minute and five minutes, for three attempts total. Twenty consecutive failures pause the view; success/resume clears the counter. Reasons include `HTTP <status>`, `timeout`, `connection failed`, `read failed` and `invalid response`. Management validation uses `invalid:`, authority uses `restricted: only the owner may ...`, and missing definitions use `not found: view`. See the [transport rules](README.md#shared-transport-boundary).

Discover management names with `supportedmethods`, MCP schemas with `tools/list`, and definitions with the owner-only list method/tool. The frontend receives only enabled view names/languages through `CustomViews`; that is presentation data, not endpoint/secret disclosure or authority to configure the view. There is no transform version capability or handshake.

## Evidence and audit limits

- [`views_test.go`](../../internal/views/views_test.go) exercises block parsing, identity, path and presentation behavior.
- [`custom_views_test.go`](../../internal/daemon/custom_views_test.go): `TestCustomViewManagementIsOwnerOnlyAndValidated`, `TestCustomViewTransformSendsBlocksAndKeepsSignedArtifacts`, replacement/reuse/rebuild, retries, artifact validation, members-only storage, hourly runs, repository README and MCP tools.
- [`custom_views_targets_test.go`](../../internal/daemon/custom_views_targets_test.go) tests recreated registration rejection and stable generation backfill; [`custom_views_media_test.go`](../../internal/daemon/custom_views_media_test.go) tests extensionless media routes.

Repeated identical blocks in one source share a `(view,hash,source)` record with one block index. Tests cover response validation and status accounting in [`custom_views_validation_test.go`](../../internal/daemon/custom_views_validation_test.go). Remote renderers remain responsible for rendering source code correctly.
