# Conformance suite

Black-box NIP and GRASP tests that speak plain websocket and HTTP to a running relay. Start `tiny` on a local URL, then run:

```sh
RELAY_URL=ws://127.0.0.1:7447 node scripts/conformance/run.mjs
```

Setup provisions a tenant with `tiny tenant create --name NAME --owner PUBKEY --template default` before the tests start. Set `TINY_BIN`, `TINY_TENANT`, `TINY_TEMPLATE`, or `TINY_DATA_DIR` to adapt that command, or `TINY_PREPROVISIONED=1` to skip it. The owner key is deterministic unless `CLAIM_SK` is supplied.

Each run preserves `environment.txt`, `stdout.log`, `stderr.log`, and `status` under `artifacts/conformance/<UTC-stamp>/`. Set `CONFORMANCE_ARTIFACTS` to choose another destination. Pass extra vitest arguments after the script name to select files, for example `node scripts/conformance/run.mjs nip01`.

The NIP-11 test does not require a numeric `limitation.max_limit` because self-hosted relays have no hosted quota.

The GRASP-08 stock Git test is opt-in because it changes the tenant policy to a members-only private service. Run it against a disposable tenant with `TINY_GRASP08=1 TINY_PREPROVISIONED=1 CLAIM_SK=<owner-secret> RELAY_URL=ws://127.0.0.1:<port> node scripts/conformance/run.mjs grasp08`. It publishes temporary repository metadata and uses one reusable repository-root proof for push, clone and fetch.
