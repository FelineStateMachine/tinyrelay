# tinygit

`tinygit` owns signed repository metadata, Git object storage, smart HTTP, repair and synchronization contracts. The integrated `tiny` host uses this same engine. The standalone executable serves public repositories without starting the relay daemon, frontend or replication workers.

## Build and run

From the repository root:

```sh
make build-tinygit
./bin/tinygit -listen 127.0.0.1:8082 \
  -data ./tinygit-data \
  -owner <64-character-lowercase-hex-public-key> \
  -public-url https://git.example
```

Go and native Git are required to build and test the service. The running service needs native Git and a POSIX shell for smart HTTP and receive hooks. Use a dedicated data directory, not a live tinyrelay tenant's database or Git directory. Put TLS and appropriate request limits in front of the listener for public hosting. `GET /healthz` reports readiness.

The data directory holds `events.db` and bare repositories under `git/`. Existing journal, hook and ref-file names retain their `tinyrelay` spelling for compatibility.

## Publish and push

1. Sign a kind 30617 repository announcement with the configured owner's key. Include its identifier in `d`, and advertise its clone URL as `https://git.example/<owner-npub>/<identifier>.git`. Use `maintainers` to name additional authorized repository signers.
2. POST the signed event as JSON to `/events`.
3. Sign a kind 30618 state event with the owner or an announced maintainer. Include the same `d`, `HEAD` with `ref: refs/heads/main` and `refs/heads/main` with the complete Git commit ID. Include every ref the state should retain.
4. POST the signed state to `/events`. Missing objects leave the state pending rather than making unavailable refs visible.
5. Push the announced objects using Git smart HTTP and a NIP-98 repository-root proof. The existing `tiny git-token` command can mint that proof:

```sh
git -c "$(tiny git-token --repo https://git.example/<owner-npub>/<identifier>.git \
  --key-env TINY_AGENT_KEY --format git)" \
  push https://git.example/<owner-npub>/<identifier>.git main
```

Set `TINY_AGENT_KEY` in the calling process to the authorized signer's hex key or nsec. Do not put secret keys in repository configuration. The proof covers the exact repository root and uses the literal GET method for Git transport authorization.

A successful `/events` response contains the event `id` and a boolean `pending`. The endpoint limits bodies to 1 MiB and accepts only signed repository announcement and state events. Repeating an accepted event retries its Git transition safely. A push cannot update a ref unless the signed state authorizes its exact object ID. After the required objects arrive, pending refs are promoted and available to clone and fetch.

This JSON endpoint is the standalone service's HTTP binding, not a Nostr WebSocket relay. No relay subscription service is started.

## Standalone profile

The standalone service admits public repositories owned by the configured key and state signed by their authorized maintainers. It rejects private repository announcements. It does not provide agent grants, collaboration pages, relay event delivery or automatic peer synchronization.

Those features remain available through the integrated tinyrelay host and its existing adapters. A standalone build is not a claim of full integrated feature parity or support for every GRASP capability.

## Embed

Import `github.com/FelineStateMachine/tinyrelay/tinygit`.

- `OpenServer` owns the standalone service's store and returns an HTTP handler. Stop serving requests before calling `Close`.
- `New` constructs the lower-level engine using `Config`. The caller owns its store, policy, access decisions, synchronization transports and lifecycle.
- `OpenStore`, `Event`, `Policy` and `Store` make the public API usable from another Go module.
- `ParseMetadata` parses repository claims and validates their supported shape without checking signatures, host authority or Git objects. Call `nostr.Validate` separately before trusting a claimed author.
- `Validate`, `CommitAfterStore` and the promotion callback separate admission, durable event persistence and visible Git state. Integration code must preserve that ordering and its transaction guarantees.

The engine uses public `protocol/nostr` event types and `protocol/auth` proof verification. It still shares internal policy, feature helpers and SQLite implementations with tinyrelay. It is a public package in the root Go module, not an independent dependency-free Go module. See [Module boundaries and protocol contracts](../docs/module-contracts.md) for the next dependency seams.

## Verify

```sh
go test -race ./tinygit ./cmd/tinygit
go vet ./tinygit ./cmd/tinygit
go test ./internal/daemon -run 'Git|Repository|PrivatePeer|PrivateGit|Collaboration'
```

The command acceptance test builds and starts the binary, publishes signed metadata, pushes and clones real objects, rejects unsigned ref changes and checks persistence after restart. An external-module test checks that consumers can use the public API.
