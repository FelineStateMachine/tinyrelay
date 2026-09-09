// Package telemetry contains the relay's low-cardinality observability boundary.
package telemetry

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config controls optional telemetry integrations. OTLPEndpoint is reserved for
// the OTLP exporter integration; the base package has no collector dependency.
type Config struct {
	Version          string
	Revision         string
	OTLPEndpoint     string
	TraceSampleRatio float64
	DebugToken       string
}

// Telemetry owns process-local metrics, tracing, and diagnostics handlers.
type Telemetry struct {
	registry       *prometheus.Registry
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	token          string
	logger         *slog.Logger

	connections   prometheus.Gauge
	subscriptions prometheus.Gauge
	messageCount  *prometheus.CounterVec
	messageTime   *prometheus.HistogramVec
	storageCount  *prometheus.CounterVec
	storageTime   *prometheus.HistogramVec
	deliveryCount *prometheus.CounterVec
	deliveryTime  *prometheus.HistogramVec
	queueDepth    *prometheus.GaugeVec
	workCount     *prometheus.CounterVec
	workTime      *prometheus.HistogramVec
}

// New creates an isolated telemetry instance. It does not mutate global
// OpenTelemetry or Prometheus registries.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	if ctx == nil {
		return nil, errors.New("telemetry: nil context")
	}
	ratio := cfg.TraceSampleRatio
	if ratio <= 0 {
		ratio = 0.01
	}
	if ratio > 1 {
		ratio = 1
	}
	provider, err := newTracerProvider(ctx, cfg, ratio)
	if err != nil {
		return nil, err
	}
	t := &Telemetry{
		registry:       prometheus.NewRegistry(),
		tracerProvider: provider,
		tracer:         provider.Tracer("tinyrelay/telemetry"),
		token:          cfg.DebugToken,
		logger:         slog.Default().With("service", "tinyrelay", "version", cfg.Version, "revision", cfg.Revision),
		connections:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "relay_connections", Help: "Current relay connections."}),
		subscriptions:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "relay_subscriptions", Help: "Current relay subscriptions."}),
		messageCount:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relay_messages_received_total", Help: "Received relay messages."}, []string{"transport", "outcome"}),
		messageTime:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "relay_message_duration_seconds", Help: "Relay message handling duration.", Buckets: latencyBuckets()}, []string{"operation", "outcome"}),
		storageCount:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relay_storage_operations_total", Help: "Storage operations."}, []string{"operation", "outcome"}),
		storageTime:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "relay_storage_operation_duration_seconds", Help: "Storage operation duration.", Buckets: latencyBuckets()}, []string{"operation", "outcome"}),
		deliveryCount:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relay_delivery_attempts_total", Help: "Outbox delivery attempts."}, []string{"outcome"}),
		deliveryTime:   prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "relay_delivery_duration_seconds", Help: "Outbox delivery duration.", Buckets: latencyBuckets()}, []string{"outcome"}),
		queueDepth:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "relay_queue_depth", Help: "Current queue depth."}, []string{"queue"}),
		workCount:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relay_work_attempts_total", Help: "Durable work handler attempts."}, []string{"kind", "outcome"}),
		workTime:       prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "relay_work_duration_seconds", Help: "Durable work handler duration.", Buckets: latencyBuckets()}, []string{"kind", "outcome"}),
	}
	if err := t.register(); err != nil {
		return nil, err
	}
	return t, nil
}

func newTracerProvider(ctx context.Context, cfg Config, ratio float64) (*sdktrace.TracerProvider, error) {
	options := []sdktrace.TracerProviderOption{sdktrace.WithSampler(sdktrace.TraceIDRatioBased(ratio))}
	if cfg.OTLPEndpoint == "" {
		return sdktrace.NewTracerProvider(options...), nil
	}
	exporterOptions, err := exporterOptions(cfg.OTLPEndpoint)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create OTLP exporter: %w", err)
	}
	options = append(options, sdktrace.WithBatcher(exporter))
	return sdktrace.NewTracerProvider(options...), nil
}

func exporterOptions(endpoint string) ([]otlptracehttp.Option, error) {
	if !strings.Contains(endpoint, "://") {
		if endpoint == "" {
			return nil, fmt.Errorf("telemetry: invalid OTLP endpoint %q", endpoint)
		}
		return []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("telemetry: invalid OTLP endpoint %q", endpoint)
	}
	host := parsed.Host
	if host == "" {
		return nil, fmt.Errorf("telemetry: invalid OTLP endpoint %q", endpoint)
	}
	options := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	if parsed.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	if parsed.Path != "" && parsed.Path != "/" {
		options = append(options, otlptracehttp.WithURLPath(parsed.Path))
	}
	return options, nil
}

func (t *Telemetry) register() error {
	collectors := []prometheus.Collector{t.connections, t.subscriptions, t.messageCount, t.messageTime, t.storageCount, t.storageTime, t.deliveryCount, t.deliveryTime, t.queueDepth, t.workCount, t.workTime, prometheus.NewGoCollector()}
	for _, collector := range collectors {
		if err := t.registry.Register(collector); err != nil {
			return fmt.Errorf("telemetry: register collector: %w", err)
		}
	}
	return nil
}

// Register adds a host-owned collector to this telemetry instance's private registry.
func (t *Telemetry) Register(collector prometheus.Collector) error {
	if collector == nil {
		return errors.New("telemetry: nil collector")
	}
	return t.registry.Register(collector)
}

// Close flushes and shuts down the tracer provider.
func (t *Telemetry) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("telemetry: nil context")
	}
	return t.tracerProvider.Shutdown(ctx)
}

// Logger returns the structured logger associated with this telemetry instance.
func (t *Telemetry) Logger() *slog.Logger { return t.logger }

// Handler returns an authenticated diagnostics handler.
func (t *Telemetry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return t.auth(mux)
}

func (t *Telemetry) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if t.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(t.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start creates a span for a bounded relay operation.
func (t *Telemetry) Start(ctx context.Context, operation string) (context.Context, func(string)) {
	name := bounded(operation, operationNames)
	spanCtx, span := t.tracer.Start(ctx, "relay."+name)
	started := time.Now()
	return spanCtx, func(outcome string) {
		result := bounded(outcome, outcomeNames)
		span.End()
		t.messageTime.WithLabelValues(name, result).Observe(time.Since(started).Seconds())
		t.messageCount.WithLabelValues(transport(name), result).Inc()
	}
}

// ObserveStorage records a storage operation.
func (t *Telemetry) ObserveStorage(operation, outcome string, duration time.Duration) {
	t.storageCount.WithLabelValues(bounded(operation, operationNames), bounded(outcome, outcomeNames)).Inc()
	t.storageTime.WithLabelValues(bounded(operation, operationNames), bounded(outcome, outcomeNames)).Observe(duration.Seconds())
}

// Connection changes the current connection gauge.
func (t *Telemetry) Connection(delta int) { t.connections.Add(float64(delta)) }

// Subscription changes the current subscription gauge.
func (t *Telemetry) Subscription(delta int) { t.subscriptions.Add(float64(delta)) }

// Delivery records an outbox delivery attempt.
func (t *Telemetry) Delivery(outcome string, duration time.Duration) {
	result := bounded(outcome, outcomeNames)
	t.deliveryCount.WithLabelValues(result).Inc()
	t.deliveryTime.WithLabelValues(result).Observe(duration.Seconds())
}

// Queue records a bounded queue depth.
func (t *Telemetry) Queue(name string, depth int) {
	t.queueDepth.WithLabelValues(bounded(name, queueNames)).Set(float64(depth))
}

// ObserveWork records a durable work handler attempt.
func (t *Telemetry) ObserveWork(kind, outcome string, duration time.Duration) {
	result := bounded(outcome, outcomeNames)
	name := bounded(kind, workNames)
	t.workCount.WithLabelValues(name, result).Inc()
	t.workTime.WithLabelValues(name, result).Observe(duration.Seconds())
}

func latencyBuckets() []float64 {
	return []float64{.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
}

var operationNames = map[string]struct{}{"http": {}, "websocket": {}, "publish": {}, "subscribe": {}, "query": {}, "count": {}, "fanout": {}, "delivery": {}, "commit": {}, "save": {}, "parse": {}, "authorize": {}, "upgrade": {}, "callback": {}, "delivery-discovery": {}, "notification-broadcast": {}, "browserepos": {}, "browserepo": {}, "browsefiles": {}, "browsefile": {}, "browsestatus": {}, "browseissues": {}, "browsepulls": {}, "browseissue": {}, "browsepull": {}, "browse-download": {}, "push": {}, "mcp": {}, "rooms": {}}
var outcomeNames = map[string]struct{}{"success": {}, "ok": {}, "invalid": {}, "unauthorized": {}, "timeout": {}, "busy": {}, "closed": {}, "error": {}, "paused": {}}
var queueNames = map[string]struct{}{"inbox": {}, "outbox": {}, "write": {}, "delivery": {}}
var workNames = map[string]struct{}{"delivery": {}, "push": {}, "replication": {}, "webhook": {}, "job": {}, "records-projection": {}, "site-mirror": {}, "git-metadata": {}, "catalog-owner": {}, "callback": {}, "delivery-discovery": {}, "notification-delivery": {}, "notification-broadcast": {}, "notification-push": {}, "notification": {}, "callback-delivery": {}}

func bounded(value string, allowed map[string]struct{}) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return "other"
}

func transport(operation string) string {
	if operation == "http" {
		return "http"
	}
	if operation == "websocket" {
		return "websocket"
	}
	return "internal"
}
