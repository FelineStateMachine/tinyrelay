// Package replication plans and executes durable relay synchronization.
//
// Planning is separate from transport. RelayDirectory and Discovery provide
// routing inputs; PrepareIntents turns them into idempotent storage.Intent
// values that can be committed with the event that caused them. Delivery,
// PullTransport and PushTransport perform network operations and are owned by
// the application that constructs the Service or calls the standalone sync
// functions. Those callers retain ownership of their transports and stores.
//
// Service owns replication's durable work queue and job state. Handlers can be
// composed with other work handlers and run by work.RunPool. A handler error
// is retried by the work package with backoff; a terminal error can be marked
// with work.Stop. Successful recurring jobs enqueue a new intent, preserving a
// completed record for the prior run.
//
// WebSocket transports validate relay URLs and public address resolution before
// dialing. Backup and artifact helpers write complete files through temporary
// files and same-directory rename.
package replication
