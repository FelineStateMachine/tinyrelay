# tinyrelay

`tinyrelay` is a standalone Nostr relay. It accepts ordinary Nostr clients over WebSocket and owns its event store, relay metadata, authentication policy and connection lifecycle. It does not start the web UI, Git hosting or tenant services.

Build and run it directly:

```sh
make build-tinyrelay
./bin/tinyrelay --data-dir ./relay-data --listen :7447 \
  --public-url wss://relay.example --name "Example relay"
```

The same runner is available as `tiny relay`. Use `--help` for all options. `--public-url` accepts `http`, `https`, `ws` or `wss`; HTTP schemes are normalized to the corresponding WebSocket scheme. When omitted, the relay uses the bound listener address.

The default profile is a public event relay. It accepts opaque Nostr events across kinds and applies protocol validity, expiration and author checks without tenant ACLs. Ephemeral events are delivered live without being stored. Protected events require the author's NIP-42 authentication. `--auth-required` requires authentication for all reads and publications; any authenticated key can read. `--owner` identifies the operator in NIP-11 metadata only.

The server exposes the WebSocket relay and NIP-11 information at `/` (or the configured public URL path), plus `/healthz` and `/readyz`. It advertises the supported NIP-1, NIP-11, NIP-42, NIP-45 and NIP-77 profile. WebSocket messages default to a 1 MiB limit and each connection's queued output defaults to 4 MiB; configure these with `--max-message-bytes` and `--max-pending-bytes`.

Each standalone process owns one data directory. The directory contains the event database and an exclusive lock, so two processes cannot open the same relay data at once. Use a separate directory for each relay and keep it on durable local storage.

## Embedding

Use `OpenServer` when the relay should own persistence and be exposed as an `http.Handler`:

```go
import "github.com/FelineStateMachine/tinyrelay/tinyrelay"

server, err := tinyrelay.OpenServer(ctx, tinyrelay.ServerConfig{
    DataDir:    "./relay-data",
    PublicURL:  "wss://relay.example",
    Name:       "Example relay",
    Owner:      ownerPublicKey,
})
if err != nil {
    return err
}
defer server.Close()
```

`New` is for a host that owns persistence. Supply a `Backend` implementation to commit events and answer authorized queries, then mount the returned relay as an HTTP handler. The combined `tiny` host uses this form so its tenant projections and relay publication remain coordinated.

The public package contains protocol contracts and lifecycle types. Storage adapters and host assembly remain in `internal/`, while `cmd/` contains thin process entry points.
