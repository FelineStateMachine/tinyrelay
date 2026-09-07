# Tiny parity ledger

`docs/parity.json` is the machine-readable completion ledger for the Go
implementation. Every retained item starts as `pending`; implementation work
may change it to `verified` only with a test or recorded acceptance check.

The source baseline is bindws revision `5d31267`. The registry inventories are
deliberately explicit: all retained management methods, relay templates,
connection templates and black-box conformance files are listed. Broad family
items point to the source modules that must be decomposed into individual
ledger entries as work lands.

The only intentional changes are the `/lease` and lease lifecycle, the relay
ownership `claim` method, fuel and billing endpoints, and hosted product
capacity entitlements. Membership
invitation claims (`createclaim`, `listclaims`, `deleteclaim` and invite join
flows) remain required. NIP-57 zap events remain ordinary supported Nostr
events; they do not create fuel credit.

## Acceptance

Run the black-box suite against a running local `tiny` instance with
`RELAY_URL=ws://host:port`. The conformance setup provisions a permanent,
owned tenant through `tiny tenant create` when configured. It never skips a
test because the relay has no `claim` method. A legacy bindws run is available
only with `LEGACY_CLAIM=1` for comparison.

Full completion requires every retained ledger entry to be `verified`, every
conformance file to pass, and an end-to-end browser matrix covering the plain
HTML replacement for all ten console families: People, Moderation, Rules,
Identity, Connect, Data, Sync, Views, Health and Owner. The matrix includes
NIP-07 and NIP-46 signing, role authorization, validation and error states,
progress/recovery, backup/restore, QR and app handoff. Disabled features still
need an implementation and an enabled-path test.

Current evidence includes the 20260907T170116Z artifact (14 configured files,
17 tests, no skips), passing `go test -race ./...` and `go vet ./...`, mounted
route acceptance tests, durable restart tests, distinct NIP-46 browser resume
evidence, browser backup restore/retry evidence, and six-repository Git
performance results. The ledger still separates those checks from the
remaining full route, browser navigation, restart coverage, and native Git
state semantics.

No `skipped` status is permitted. Removed fuel/lease tests are explicit
negative tests for absence; all other bindws behavior remains required.
