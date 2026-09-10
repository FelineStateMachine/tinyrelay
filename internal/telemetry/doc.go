// Package telemetry is the relay's process-local observability boundary.
//
// Telemetry owns an isolated Prometheus registry, OpenTelemetry tracer
// provider, structured logger and authenticated diagnostics handlers. Metrics
// use bounded operation and outcome labels. New configures sampling and an
// optional OTLP HTTP exporter; Close flushes and shuts down the tracer
// provider. HTTP exposes metrics and, when configured, token-protected
// diagnostics without registering process-wide handlers.
package telemetry
