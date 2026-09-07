# Testing and the Linux host

The normal development loop is local:

```sh
make test-internal-race
make test
```

The repository also contains a multistage Docker build. `make docker-test` runs `TEST_PACKAGES` (default `./...`) under Go 1.27.1 on Debian Bookworm with the race detector. `make docker-build` builds the self-hosted runtime image.

## Remote Linux host

`scripts/test-linux.sh` runs the same internal race test in an isolated Docker build on the configured Linux host. It synchronizes the exact worktree, including untracked files, over SSH into a unique directory under `~/tinyrelay-tests/` in the remote home directory; it never runs global Docker cleanup and never edits host configuration.

```sh
LINUX_HOST=<host> LINUX_NAME=tinyrelay-review-1 ./scripts/test-linux.sh
```

Useful overrides are `LINUX_PACKAGES` (the default internal package list) and `LINUX_ARTIFACTS` (local artifact destination). The script records UTC time, host, kernel, CPU count, Docker server version, Go version, target platform, and SQLite module version beside the Docker build log, including a status file when the build fails. A unique `LINUX_NAME` is required when multiple runs share a host.

To run the complete tree on Linux, set `LINUX_PACKAGES='./...'`.

The runtime image is a small Debian image containing the static Go binary, `ca-certificates`, and `git`. It runs as UID 10001 (`relay`) with `/data` as the writable volume. Bind the relay and diagnostics listeners explicitly; diagnostics should stay on loopback or a protected Tailscale address and must not be published publicly.

Performance runs should use a separate artifact name and record workload seed, client version, host CPU, Go version, SQLite version, storage medium, configuration, raw logs, and profiles. Capacity claims are meaningful only when those inputs are kept with the result.

The checked-in benchmarks provide a repeatable first baseline without claiming an optimized capacity:

```sh
make benchmark
LINUX_HOST=<host> ./scripts/bench-linux.sh
```

`BenchmarkSave`, `BenchmarkQuery`, and `BenchmarkTextSearch` use a local SQLite WAL store with representative seeded events. `BenchmarkFanout` measures only the in-memory broadcast and enqueue path to 10, 100, and 1,000 subscribed clients; it excludes websocket network writes and client-reader scheduling. The benchmark script records the Linux host, CPU count, Docker/Go/SQLite versions, benchmark output, and exit status under `artifacts/bench/`; use `BENCH_PACKAGES` and `BENCHTIME` for controlled comparisons. Treat these as baselines until a workload and hardware matrix supports a capacity claim.

For Git workloads that include child processes inside Docker, `scripts/observe-git-benchmark.py` captures the benchmark output, host load/memory, and Docker cgroup v2 CPU, memory, I/O, and PID samples every 500ms. It also snapshots authenticated `/metrics`, heap, and goroutine profiles before and after the run. The optional `--cpu-profile` captures one bounded 60-second Go CPU profile; cgroup samples remain the source for child CPU accounting.

```sh
python3 scripts/observe-git-benchmark.py --out artifacts/git/baseline \
  --container tiny-git-perf-baseline \
  --diagnostics http://127.0.0.1:17448 --token "$TINY_DIAGNOSTICS_TOKEN" \
  -- ./scripts/git-performance.mjs
```

The conformance harness runs the copied bindws-compatible cases against a running local daemon and preserves stdout, stderr, status, and environment artifacts. Start the daemon, then run `node scripts/conformance/run.mjs`; the NIP-11 compatibility assertion reflects the quota-free self-hosted contract.

## Real Git repositories

The Git performance harness below records raw profiles and results under `artifacts/git-performance/`.

Prepare a new corpus from committed local branches and tags. This leaves the source repositories unchanged and refuses an existing output directory:

```sh
python3 scripts/prepare-git-performance.py \
  --source-directory "$HOME/Developer" --out /tmp/tiny-git-corpus
```

Create a dedicated test tenant using the public benchmark fixture identity, then start a separate test daemon. Never use this identity for a real relay:

```sh
tiny tenant create --data-dir /tmp/tiny-git-test --name main \
  --owner 5ac640e5df8f7945381c31f435288ba3f587fbd68efb27abaf941792f9c36369
tiny serve --data-dir /tmp/tiny-git-test --default-tenant main \
  --listen 127.0.0.1:17447 --allow-private-relays
```

In another terminal, enable Git hosting and run the workload:

```sh
node scripts/configure-git-performance.mjs http://127.0.0.1:17447
node scripts/git-performance.mjs --relay ws://127.0.0.1:17447 \
  --repos-dir /tmp/tiny-git-corpus --out artifacts/git-performance/local-run \
  --repeats 3
```

Use `--only strudel,atlas` to select fixtures. Each run creates distinct signed repository identities, checks all transferred refs and HEAD, runs `git fsck`, and measures relay EVENT/REQ traffic throughout the Git phases. The incremental push adds a commit with an unchanged tree; it is a small metadata update, not a new binary upload.

Add `--private` to test private repository announcements and NIP-98 Git authorization. The harness authenticates its control WebSocket with the repository signer and starts a loopback signing proxy for native Git. That proxy hashes each request through a temporary file and signs the exact URL, method and body; its overhead is included in private timings. Private server authorization also spools before invoking Git, so malformed payloads cannot mutate the repository and upload size does not determine Go heap size. Do not interpret private timings as a like-for-like unauthenticated native Git comparison.

For native Git comparisons, `go run ./scripts/git-http-baseline --root /path/to/fresh-bare-repos --listen 127.0.0.1:17449` exposes a benchmark-only HTTP server. Initialize one empty bare `<name>.git` per fixture, set its symbolic HEAD to the corpus manifest's `head_branch`, and enable `http.receivepack`. Add `--baseline http://127.0.0.1:17449` to the workload. Use fresh baseline repositories for each run so initial pushes transfer the complete corpus.

The raw JSON records failures as failures; interrupted or absent phases are not successful samples. The observer separates Go process memory from container memory, which also includes native Git and file cache. Run performance measurements separately from compilation and test suites on the same host.
