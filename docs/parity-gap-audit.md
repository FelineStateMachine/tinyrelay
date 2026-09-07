# Parity gap audit

Compared with bindws revision `5d31267`. This is a behavior ledger, not a
claim that package compilation proves full UX parity. The native contract
removes hosted leases/trials, ownership provisioning claims, fuel/billing, and
product entitlement caps. Membership invite claims remain required.

## Evidence recorded

- The retained 90-method source registry is exposed through the tenant
  dispatcher. `internal/daemon/management_golden_test.go` invokes every method
  on a fresh tenant and fails on `unsupported`; representative read methods
  must succeed.
- `internal/daemon/ux_contract_test.go` makes signed HTTP calls for invite
  creation/listing, member mutation, moderation, IP rules, policy and identity
  changes, config export, fork, and deletion. It also checks guided Sync
  argument shapes.
- Domain/NIP-AD validation, custom host catalog mappings, retention sweeps,
  records scheduling/privacy, callback registration validation, backup sealing,
  Git behavior, and replication queues have focused package tests.
- The current conformance artifact `20260907T170116Z` runs all 14 configured
  files (the source NIP-11 file is replaced by the quota-free compatibility
  test) and records 17 passing tests with no skips. Earlier artifacts remain
  historical evidence only.
- The follow-up Git artifact `20260907T154227Z` passes its Git case. A
  separate browser run also exercised the remote-signer backup flow. These
  close those observed cases only, not the entire GRASP or browser families.
- The latest retained protocol artifact `20260907T170116Z` again passes all
  17 tests with no skips. `go test -race ./...` and `go vet ./...` also pass.
- `routes_acceptance_test.go` covers mounted NIP-11, NIP-86, bridge auth,
  NIP-05, NIP-AD auth behavior, public pages, CORS, and data-route denial.
  `durable_acceptance_test.go` covers restart recovery for a dump job, view
  checkpoint, and notification intent. `git_push_test.go` covers immediate
  30-ref HTTP push after router publication.
- Browser evidence covers a distinct NIP-46 signer reconnect and signed
  mutation (`output/playwright/nip46-distinct-resume-evidence.json`) and a
  valid backup restore plus malformed-archive retry
  (`artifacts/browser-restore-qa.json`). The restore run used a page-local NIP-07 fixture, so its subsequent search route
  lost that injected signer and required authentication again. The retained NIP-46
  browser evidence now closes navigation persistence for the supported remote
  signer path with distinct remote and user keys; NIP-07 extension persistence
  remains browser-managed and is not emulated by the page.
- Git performance evidence covers six real repositories through all harness
  phases in `artifacts/git-performance/results-after`.
- `hosting_routes_acceptance_test.go` verifies per-tenant site isolation and a
  custom host mapping. `callback_restart_acceptance_test.go` verifies durable
  callback recovery and revocation. `git_private_restart_test.go` verifies
  pending private state recovery, unsigned denial, signed HTTP discovery and
  receive-pack negotiation after restart, and tampered-payload rejection.
- Public transfers pass all six fixtures; private nzip and Atlas transfers
  also pass. Authenticated upload spooling keeps Go RSS independent of pack
  size. Tail acknowledgement stalls remain visible in the performance report.
- The complete Linux race suite passes at
  `artifacts/slate/tiny-private-git-final-20260907/20260907T165524Z`.
  The subsequent Git policy changes pass the daemon race suite on Linux at
  `artifacts/slate/tiny-git-policy-final-20260907/20260907T165928Z`.

## Remaining acceptance work

1. HTTP portable import now has signed mounted-route acceptance for final
   imported counts and persisted content, alongside durable import-job restart
   coverage. The combined Git-containing HTTP backup/restore journey also has
   focused acceptance coverage. A streamed progress UI remains outside the
   current native route contract. Positive signed NIP-AD and NIP-96
   upload/get/delete, site isolation/custom mapping, Smart HTTP push, and
   backup preview/restore are already covered by focused tests.
2. Complete browser tree traversal and moderation recovery acceptance.
   Invite claim, Sync progress/retry, view publication and fork navigation
   have recorded checks in `output/playwright/browser-checks.md`; deletion
   recovery also has focused daemon tests. The
   NIP-46 reconnect and signed mutation path is covered; the NIP-07 restore
   fixture intentionally loses its page-local signer after navigation and is
   not evidence of a native signer persistence failure.
3. Restart coverage now includes inbox/outbox delivery retry, callback
   revocation, dump and backup jobs, view checkpoints, notification intents,
   and terminal replication-job failure status with retry attempts. Portable
   import progress remains an unverified restart case.
4. Complete a private object transfer and full inventory assertion after
   restart. Signed private HTTP discovery/negotiation and pending inventory
   survive restart; the separate real private transfer run passes too. Native state projection,
   stale-worker protection, dropped-ref cleanup, journal recovery, immediate
   multi-ref push, and private-state hiding already have focused tests.

The complete source inventory and explicit exclusions remain machine-readable
in `docs/parity.json`. Items stay `pending` when their implementation has
focused tests but lacks the route, browser, restart, or source conformance
evidence named above.
