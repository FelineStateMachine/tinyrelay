# Conformance suite

Black-box NIP and GRASP tests that speak plain websocket and HTTP to a running relay. Start `tiny` on a local URL, then run:

```sh
RELAY_URL=ws://127.0.0.1:7447 node scripts/conformance/run.mjs
```

Setup provisions a tenant with `tiny tenant create --name NAME --owner PUBKEY --template default` before the tests start. Set `TINY_BIN`, `TINY_TENANT`, `TINY_TEMPLATE`, or `TINY_DATA_DIR` to adapt that command, or `TINY_PREPROVISIONED=1` to skip it. The owner key is deterministic unless `CLAIM_SK` is supplied.

Each run preserves `environment.txt`, `stdout.log`, `stderr.log`, and `status` under `artifacts/conformance/<UTC-stamp>/`. Set `CONFORMANCE_ARTIFACTS` to choose another destination. Pass extra vitest arguments after the script name to select files, for example `node scripts/conformance/run.mjs nip01`.

The NIP-11 test does not require a numeric `limitation.max_limit` because self-hosted relays have no hosted quota.

The GRASP-08 stock Git test is opt-in because it changes the tenant policy to a members-only private service. Run it against a disposable tenant with `TINY_GRASP08=1 TINY_PREPROVISIONED=1 CLAIM_SK=<owner-secret> RELAY_URL=ws://127.0.0.1:<port> node scripts/conformance/run.mjs grasp08`. It publishes temporary repository metadata and uses one reusable repository-root proof for push, clone and fetch.

The combined-host actor profile is opt-in and mutates membership, policy and agent state on a disposable tenant. It uses deterministic process-local keys derived from `CONFORMANCE_SEED` plus the labels `member`, `agent` and `outsider`; `CLAIM_SK` is the local owner key. Run it only against the combined host, with the tenant already provisioned:

```sh
CONFORMANCE_ACTORS=1 CONFORMANCE_SEED=slate-docker-2026-09-13 \
  CLAIM_SK=<local-owner-secret> TINY_PREPROVISIONED=1 \
  RELAY_URL=ws://127.0.0.1:17477 \
  node scripts/conformance/run.mjs actor-authz
```

The profile checks owner, member and outsider visibility over WebSocket, the HTTP bridge and MCP, then grants and revokes an agent and checks NIP-98 method, body and path binding. It restores the original policy in cleanup. Use a disposable tenant and set `RELAY_URL` to its configured canonical URL.

The cross-instance profile uses four local signing identities and an independent TypeScript negentropy client to reconcile 192 events split between two relays. Set `RELAY_SECONDARY_URL` to a second disposable relay and run `node scripts/conformance/run.mjs cross-relay` with `RELAY_URL`, `CLAIM_SK` and `TINY_PREPROVISIONED=1`. It authenticates each identity on both connections, transfers missing events in both directions and checks that both histories converge. The primary relay must allow these test authors to publish.

See [relay stress checks](../qa/relay-stress.md) for concurrent actor runs, slow readers and native protocol fuzzing.
