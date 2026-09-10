package daemon

import (
	"context"

	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

// workRunner captures the tenant-wide queue composition before a worker
// goroutine starts. Feature services contribute handlers; their jobs share
// the same admission gate, observation and worker lifetime.
func (t *Tenant) workRunner() func(context.Context) error {
	handlers := t.replication.Handlers()
	for kind, handler := range t.workHandlers() {
		handlers[kind] = handler
	}
	for kind, handler := range handlers {
		handlers[kind] = t.guardWork(handler)
	}
	queue := t.replication.Queue()
	options := work.PoolOptions{
		Worker: work.WorkerOptions{Observe: t.app.telemetry.ObserveWork},
	}
	return func(ctx context.Context) error {
		return work.RunPool(ctx, queue, handlers, options)
	}
}

// runWork composes a fresh runner for tests and explicit worker restarts.
func (t *Tenant) runWork(ctx context.Context) error {
	return t.workRunner()(ctx)
}
