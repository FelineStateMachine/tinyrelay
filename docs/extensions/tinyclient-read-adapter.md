# Tinyclient read adapter

| Field | Value |
| --- | --- |
| Identifier | `tinyclient-read-adapter` |
| Status | Experimental |
| Contract revision | 1, tied to this repository revision; no version negotiation |
| Owner | `tinyclient` defines the presentation contract; the combined `tiny` backend owns data and authorization. |
| Transport | HTTP GET, JSON success bodies, plain-text error bodies. |
| Endpoint | `/api/tinyclient`, under the selected tenant prefix when present. |

This project extension supports a separately running frontend. It is not a Nostr NIP, a general relay query protocol or a replacement for signed mutation APIs. Run compatible frontend and backend revisions together. No event kinds, tags or signing formats are introduced.

## Authentication and scope

The backend resolves the caller using its normal actor resolver. A public-key query parameter or header is not authentication. The standalone frontend forwards the browser session cookie when loading the presentation snapshot. For method and typed reads, an empty requested actor means anonymous access: the frontend omits the cookie even if the browser is signed in. A nonempty requested actor must match the actor authenticated in that request's snapshot; a mismatch fails before contacting the backend. Every backend query and typed read still resolves authentication and applies its own access checks.

Keep the frontend's public origin and tenant path the same as the backend's configured public URL. The private backend address may differ. Existing NIP-98 proofs remain bound to their original URL, method and payload; the frontend proxies signed requests without translating or replaying them as adapter reads. Signed HTML requests use the integrated renderer.

Responses carry `Cache-Control: private, no-store`. The frontend must not cache private snapshots for use after an authorization or backend failure. It refuses an unexpected public Host and does not follow redirects when fetching read-adapter responses.

## Request forms

All forms use GET. If `op` is nonempty it selects a typed read before `method` is considered.

### Presentation snapshot

With no `op` or `method`, the response is a JSON object with:

- `policy`: Presentation policy, using the public `Policy` structure.
- `url`, `slug`, `identity`: Relay presentation identity.
- `actor`: Public key resolved by the backend, or an empty string for an anonymous caller.
- `version`, `revision`: Backend build metadata.
- `rooms`, `activity`: Whether the backend provides the corresponding optional typed interfaces.
- `share_presence`: The authenticated caller's presence preference.
- `views`: Optional custom-view presentation definitions.
- `read_error`: Optional private-access failure used to render a restricted page.

Nonowners receive a reduced policy without owner-only operational settings. A caller barred from private relay reads receives a masked presentation snapshot with `read_error`, rather than private relay identity or configuration. The snapshot does not grant permission to perform later reads.

The authoritative field definitions are [`remoteSnapshot`](../../tinyclient/remote.go), [`Policy`](../../tinyclient/contracts.go) and their referenced public types. Readers should tolerate additional object fields. Changes to existing field meanings require a contract revision and compatibility review.

### Method reads

Supply `method=<name>` and optional `params=<JSON-array>`. Method names must appear in the read-only allowlist in [`remoteReadMethods`](../../tinyclient/remote.go). The adapter passes the decoded parameters and authenticated actor to `Backend.Query` and returns its JSON result directly, without a JSON-RPC envelope.

Methods cover presentation reads such as repository browsing, wiki, rooms, social content, approvals and management lists. The allowlist does not override method authorization. Mutations are never dispatched through this contract. Method parameter and result semantics remain those of the corresponding backend read operation; this revision is coupled to the repository's backend contracts rather than a standalone third-party API.

### Typed reads

| `op` | Parameters | Backend contract |
| --- | --- | --- |
| `rooms` | `cursor`, `limit` | `RoomsReader.ListRooms` |
| `room` | `room`, `cursor`, `limit` | `RoomsReader.ReadRoom` |
| `thread` | `room`, `root`, `cursor`, `limit` | `RoomsReader.ReadThread` |
| `activity` | `room`, `root` | `ChatActivityReader.ReadChatActivity` |

Results use the typed interfaces in [`read_contracts.go`](../../tinyclient/read_contracts.go) and [`chat-activity.go`](../../tinyclient/chat-activity.go). The backend owns room access, cursor validation and limit bounds. The current adapter converts an absent or unparseable limit to zero; callers should send a valid decimal limit rather than rely on that fallback.

## Errors

| Status | Meaning |
| --- | --- |
| 200 | A successful JSON read, including a deliberately masked private snapshot. |
| 400 | A method outside the allowlist or malformed JSON method parameters. |
| 401 | The actor resolver rejected authentication. |
| 403 | Private access or a backend operation was refused; also used for an unsupported typed `op`. |
| 405 | Non-GET method; response includes `Allow: GET`. |
| 501 | Requested typed interface is unavailable. |

Do not parse plain-text errors as stable machine identifiers. These statuses describe the current binding, not a universal error model for the project. The standalone frontend returns 502 if it cannot load a valid snapshot, rather than rendering stale private data.

## Discovery and compatibility

The endpoint is configured as part of a compatible tinyrelay deployment; it is not advertised as a NIP capability. `rooms` and `activity` describe optional read interfaces, not full protocol compliance. Clients that do not use the adapter continue using the integrated website and existing relay interfaces.

The adapter does not migrate sessions across origins, accept another tenant path, own a database or hold a relay signing key. Each standalone frontend process serves one tenant. A root-mounted frontend returns 404 for reserved `/r/<name>` paths before rendering or forwarding any request, including signed requests and WebSocket upgrades. A prefixed frontend accepts only its configured tenant path. Unknown method names fail closed; adding a method requires an authorization review and tests before it enters the allowlist.

## Verification

The contract is exercised by [`remote_auth_test.go`](../../tinyclient/remote_auth_test.go), [`remote_rooms_test.go`](../../tinyclient/remote_rooms_test.go), [`remote_proxy_test.go`](../../tinyclient/remote_proxy_test.go), [`remote_browser_test.go`](../../tinyclient/remote_browser_test.go) and the [real-backend integration test](../../internal/daemon/tinyclient_integration_test.go).

Tests cover valid reads, invalid methods, forged actors, private snapshots, session revocation, signed operation forwarding and frontend rendering parity. Regression coverage in [`remote_feed_test.go`](../../tinyclient/remote_feed_test.go), [`remote_actor_test.go`](../../tinyclient/remote_actor_test.go) and [`remote_scope_test.go`](../../tinyclient/remote_scope_test.go) verifies anonymous feeds with a browser session, actor preservation across method and typed reads, and tenant isolation before rendering or proxying. Run the commands in the [tinyclient guide](../../tinyclient/README.md#test-and-rebuild-browser-bundles).
