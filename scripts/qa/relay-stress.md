# Relay stress checks

Run these checks against disposable combined or standalone relays. They publish signed events and retain them in the target database. Use a separate data directory and locally generated test identities.

Generate 20 identities without printing their secrets:

```sh
mkdir -p artifacts/relay-stress
node --input-type=module <<'JS'
import { writeFileSync } from "node:fs";
import { generateSecretKey, getPublicKey } from "nostr-tools/pure";
const actors = Array.from({ length: 20 }, (_, i) => {
  const key = generateSecretKey();
  return { name: `actor-${i}`, secret: Buffer.from(key).toString("hex"), pubkey: getPublicKey(key) };
});
writeFileSync("artifacts/relay-stress/identities.json", JSON.stringify({ actors }), { mode: 0o600, flag: "wx" });
JS
```

Start the relay with a 1 MiB message limit and a 4 MiB pending-output limit, then run:

```sh
node scripts/qa/relay-stress.mjs \
  --relay ws://127.0.0.1:7447 \
  --identities artifacts/relay-stress/identities.json \
  --out artifacts/relay-stress/result.json \
  --actors 16 --events 200 --seed 1 --slow-reader
```

Add `--auth-required` when the standalone relay requires authentication. This also checks anonymous reads and writes, event author binding, incorrect relay and challenge proofs, multiple authenticated keys, and proof reuse across connections. A combined tenant with members-only reads needs a separate membership test; the stress actors are not automatically enrolled.

The harness checks concurrent publication, exact fanout to four subscribers, duplicate suppression, historical ordering, overlapping filter limits, unique counts, replacement, deletion, protected events, invalid signatures, malformed input recovery and reconnects. Payloads range from empty to about 8 KiB. The slow-reader phase adds 192 unique events of about 64 KiB and checks that a healthy client remains responsive while the stalled client disconnects or catches up after resuming.

The JSON report contains public actor identities, counts, failures and publish acknowledgment latency percentiles. It excludes signing secrets. Latencies measure each send through its acknowledgment, excluding the actor's local send queue. They reflect the test machine, relay and network conditions; this is a correctness stress check, not a capacity benchmark. Repeat with different seeds to vary signed payloads.

Keep the identity file on the test client when using a remote relay. An SSH tunnel can connect the client to an isolated Docker instance without exposing a public port. Remove the temporary instance after collecting its logs and resource measurements.

## Protocol fuzzing

Go fuzz targets cover event and filter parsing, NIP-98 and Blossom proof parsing and request binding, and NIP-42 connection boundaries. For example:

```sh
go test ./protocol/nostr -run '^$' -fuzz '^FuzzParseFilter$' -fuzztime=60s -parallel=2
go test ./protocol/auth -run '^$' -fuzz '^FuzzNIP98Binding$' -fuzztime=60s -parallel=2
go test ./protocol/auth -run '^$' -fuzz '^FuzzBlossomBinding$' -fuzztime=60s -parallel=2
go test ./tinyrelay -run '^$' -fuzz '^FuzzStandaloneAuthTagParsing$' -fuzztime=60s -parallel=2
```

These targets use local keys and temporary storage. Run the [conformance suite](../conformance/README.md) separately for HTTP, MCP, Git, Blossom and cross-relay integration coverage.
