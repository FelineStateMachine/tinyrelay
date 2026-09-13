# tinyclient

`tinyclient` is the relay website. It includes the HTML templates, styles,
JavaScript modules and embedded browser bundles used by the integrated `tiny`
backend. Pages render on the server and remain readable without JavaScript.
Signing, encrypted messages, uploads and live updates use browser enhancements.

## Build and run

Run these commands from the repository root:

```sh
go build -o tinyclient-server ./cmd/tinyclient
./tinyclient-server \
  -listen 127.0.0.1:8081 \
  -backend http://127.0.0.1:7447 \
  -public-url https://relay.example
```

The backend must be the combined `tiny` host (`tiny serve`) from this
repository. Set its `--public-url` to the same `https://relay.example` address.
Route that public
origin to tinyclient through your HTTPS reverse proxy, preserving the Host
header. Keep the backend listener on the private network. The frontend renders
pages and assets locally and forwards relay APIs, WebSocket connections,
streams, signed requests and mutations to the backend.

For a path-based tenant, include the same path in both URLs:

```sh
./tinyclient-server \
  -backend http://127.0.0.1:7447/r/team \
  -public-url https://relay.example/r/team
```

Each process serves one tenant. The root path and `/r/<name>` are supported;
arbitrary mount paths and different frontend and backend tenant paths are not.
A root-mounted frontend rejects `/r/<name>` requests before rendering or
forwarding them. A prefixed frontend accepts only its configured tenant path.
The default listener is `127.0.0.1:8081`. Run `tinyclient-server -help` for flags.

The binary embeds its assets and needs no working directory, SQLite database,
Git installation or relay signing key at runtime. It requires a running backend
for relay data and session validation. Embedded scripts and icons remain available
when the backend is unavailable. Failure to load the relay snapshot
returns HTTP 502 rather than serving cached private data. It is not an offline client or a frontend for
an arbitrary Nostr relay.

## Authentication

Keep the relay's public origin and tenant path unchanged when splitting the
processes. NIP-98 signatures bind the exact URL, method and body. Tinyclient does
not re-sign requests, accept a public key as authentication or translate proofs
between origins. Incoming requests for a different public Host are rejected.
Configure the relay's HTTPS public URL even when the private hop uses HTTP so
it verifies the original scheme and issues secure cookies.

Browser sessions remain owned, checked and revoked by the relay. The frontend
loads the presentation snapshot through the read-only contract at
`/api/tinyclient` using the browser's session cookie. Anonymous method and typed
reads omit that cookie, including public article feeds requested by signed-in
users. Authenticated reads require the requested actor to match the snapshot's
authenticated actor. The relay derives the actor from the session, checks access
on data reads and redacts private relay metadata. Owner-only settings are not
included in anonymous or nonowner snapshots. Signed operations and relay
protocol traffic retain their authorization headers, paths, query strings and
bodies as they pass upstream. Signed page requests are handled by the integrated
backend renderer rather than replaying a signature across several reads.

Private pages and authenticated no-JavaScript navigation are supported with a
valid browser session. Creating that session still requires a browser signer.
Pointing the frontend at a different public origin is not a supported migration
path for existing sessions or signed URLs.

## Embed

Import `github.com/FelineStateMachine/tinyrelay/tinyclient` and call `New` with a
`Backend` and `Options`. Public `Policy`, `Features`, `CustomView` and room types
let another package implement the interfaces without importing relay-internal
types. `DefaultPolicy` supplies the normal feature settings.

`Backend.Query` owns authorization for method calls. `Options.Actor` resolves
authenticated requests; it must verify a session or proof, never a caller's
claimed public key. Optional `RoomsReader`, `PrivateReader`,
`AccountPreferencesReader`, `CustomViewSource` and `ChatActivityReader`
interfaces provide typed room reads, membership checks and richer page data.
The remote adapter preserves typed room summaries, folded replies, reaction
counts, activity cards and account preferences.

`App.BackendHandler()` exposes the read contract for an embedded data service.
`App.ServeHTTP` mounts it automatically. `NewRemote` returns the standalone HTTP
handler for use in another executable.

The `/api/tinyclient` contract is a project-specific, read-only HTTP extension,
not a Nostr NIP. See the [read-adapter specification](../docs/extensions/tinyclient-read-adapter.md)
for request forms, authorization, errors and compatibility. GET without a method returns the authorized presentation
snapshot. `method` and JSON-array `params` select an allowlisted backend read;
`op=rooms|room|thread|activity` selects a typed read with its room, root, cursor
and limit parameters. Every call resolves authentication at the tinyrelay
backend. This adapter introduces no event kinds, signing formats or storage
identifiers. Frontend validation improves feedback; the backend remains the
authority for protocol validation, membership and mutation permissions.

## Test and rebuild browser bundles

```sh
go test ./tinyclient ./cmd/tinyclient
go test ./internal/daemon -run TestStandaloneTinyclient
npm ci
npm run test:tinyclient
npm run build:tinyclient
```

Go builds use the checked-in browser bundles, so Node.js is not required to build
or run the binary. Rebuild the bundles when their JavaScript sources or pinned
dependencies change. Bundle verification compares regenerated bytes with the
checked-in assets.
