# Observability

The `internal/telemetry` package is the relay's process-local observability boundary. It owns an isolated Prometheus registry, OpenTelemetry tracing, structured logging, and an authenticated diagnostics handler. A self-hosted binary can run without a collector or observability database.

## Host integration

```go
tel, err := telemetry.New(ctx, telemetry.Config{
	Version:          buildVersion,
	Revision:         buildRevision,
	TraceSampleRatio: 0.01,
	DebugToken:       os.Getenv("TINYRELAY_DEBUG_TOKEN"),
})
if err != nil {
	return err
}
defer tel.Close(context.Background())

diagnostics := http.Server{Addr: "127.0.0.1:9090", Handler: tel.Handler()}
```

Bind the diagnostics server separately from the public relay listener. Every endpoint (`/metrics`, `/health`, and `/debug/pprof/*`) requires `Authorization: Bearer <DebugToken>`. Use a strong secret and restrict the listener at the network layer as well.

The main listener exposes `/healthz` for process liveness and `/readyz` for readiness. Readiness requires the serving lifecycle to have started and a bounded catalog storage probe to succeed; it does not require a default tenant. Tenant paths expose `/r/<name>/healthz` and `/r/<name>/readyz`, which perform a bounded local SQLite probe and return unavailable during tenant maintenance. Remote replication and delivery failures do not make the process ready state fail.

The package API is:

```go
type Config struct {
	Version, Revision, OTLPEndpoint, DebugToken string
	TraceSampleRatio float64
}
func New(context.Context, Config) (*Telemetry, error)
func (*Telemetry) Close(context.Context) error
func (*Telemetry) Logger() *slog.Logger
func (*Telemetry) Handler() http.Handler
func (*Telemetry) Start(context.Context, string) (context.Context, func(string))
func (*Telemetry) ObserveStorage(string, string, time.Duration)
func (*Telemetry) Connection(int)
func (*Telemetry) Subscription(int)
func (*Telemetry) Delivery(string, time.Duration)
func (*Telemetry) Queue(string, int)
```

Operation, outcome, and queue labels are normalized to bounded vocabularies. Never add tenant IDs, pubkeys, event IDs, subscription IDs, raw filters, or arbitrary URLs to metric labels. Per-tenant inspection belongs in a separately authenticated, short-lived administrative view.

The registry exports relay metrics plus Go runtime collectors. The OTel tracer is always present and uses a ratio sampler; exporting is optional. When `OTLPEndpoint` is set, the package uses the pinned OTLP HTTP exporter and flushes spans during `Close`; no collector is required for the relay itself.

Register durable work metrics explicitly after constructing the queue:

```go
queue := work.New(store)
if err := tel.Register(queue.Metrics()); err != nil {
	return err
}
worker := work.NewWorkerWithOptions(queue, handlers, work.WorkerOptions{
	Lease:   30 * time.Second,
	Renew:   10 * time.Second,
	Observe: tel.ObserveWork,
})
```

Each active handler attempt has a renewal loop at the configured interval. If renewal loses the fencing token or an administrator cancels the intent, the handler context is cancelled and the worker waits for the renewal loop before attempting any completion or retry. A stale attempt cannot commit after another worker reclaims the intent. Work metric kind labels use a fixed allowlist and collapse unknown kinds to `other`.

Use pprof and execution traces only during bounded diagnostic captures. Profile one mode at a time because some Go diagnostic tools interfere with one another. Treat metrics as the continuous signal and profiles/traces as evidence for a specific optimization.

The first dashboards should cover traffic and protocol outcomes, connection/fan-out pressure, inbox/outbox lag and retries, storage latency/locks/WAL/checkpoints, runtime health, and slow/error exemplars. Optimize only after a metric, trace, or profile identifies the dominant cost; preserve a reproducible benchmark workload and compare correctness, tail latency, CPU, memory, allocations, goroutines, WAL growth, checkpoint cost, and disk writes together.

## Capability and peer status

`Tenant.Capabilities(peers)` returns the bounded operator registry used to assemble protocol capability status. Only entries with `status == "enabled"` should be advertised as supported NIPs. Git and GRASP entries require both the tenant policy and an initialized Git service. Blob doors report their concrete BUD-01, BUD-02, BUD-04, BUD-09, BUD-11, and BUD-12 status only when the file service is mounted; unsupported BUD documents are not advertised.

NIP-66 monitoring is opt-in and accepts only the peer URLs supplied in `PeerMonitorConfig`. `App.PeerMonitorRun(ctx)` owns the probe lifecycle when `Config.PeerMonitor` is set. The CLI publication path requires both `--peer-publish` and an explicit `--peer-publish-tenant NAME`; it signs and stores kind 30166 records with that tenant's relay identity, then uses the tenant's configured durable outbox for any delivery. A publication request remains `configured` until the signed publisher is bound, so a switch alone cannot claim that signed NIP-66 events are being published. Probe callbacks expose only bounded `ok`/`error` outcomes and duration; peer URLs belong in operator status, never metric labels.

## Git workload captures

The [real repository experiment](git-performance.md) follows this workflow. The original Git HTTP wrapper buffered entire pack responses; process RSS and the CPU profile exposed allocation, clearing and copying costs. Streaming the response and batching signed-ref/object checks removed the measured bottlenecks. The harness checks EVENT acknowledgement and exact-ID query latency while push, clone and fetch operations run, so a fast Git transfer cannot conceal a stalled relay.

Use `scripts/observe-git-benchmark.py` for repeatable captures. Keep Go process RSS separate from cgroup memory: native Git children and filesystem cache are charged to the container, and do not appear in a Go heap profile. Likewise, Go CPU profiles exclude Git child CPU; use cgroup CPU deltas to account for the whole server. Record concurrency, repository object/ref counts, Git versions, page-cache conditions and the native Git baseline with each result. Instructions and comparison limits are in [testing.md](testing.md#real-git-repositories).
