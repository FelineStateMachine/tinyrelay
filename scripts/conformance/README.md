# Conformance harness

Start `tiny` on a local URL, then run:

```sh
RELAY_URL=ws://127.0.0.1:7447 node scripts/conformance/run.mjs
```

The harness executes the original bindws black-box files from `../bindws`
without modifying that checkout. By default setup provisions a permanent
tenant with `tiny tenant create --name NAME --owner PUBKEY --template default`.
Set `TINY_BIN`, `TINY_TENANT`, or `TINY_TEMPLATE` to adapt the command. The
owner key is deterministic unless `CLAIM_SK` is supplied.

For comparison only, `LEGACY_CLAIM=1` invokes bindws's old NIP-86 `claim`
setup. This compatibility path is not a tiny requirement and does not skip or
alter test assertions.

Each run preserves `environment.txt`, `stdout.log`, `stderr.log`, and `status` under `artifacts/conformance/<UTC-stamp>/`. Set `CONFORMANCE_ARTIFACTS` to choose another destination. The source conformance files remain unchanged. The hosted NIP-11 file is replaced in this harness by `nip11.compat.test.ts`, which preserves every assertion except the numeric `limitation.max_limit` requirement because self-hosted relays have no hosted quota. Other self-hosting differences belong in separately named compatibility tests.
