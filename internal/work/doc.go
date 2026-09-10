// Package work provides durable, fenced work intents backed by a storage.Store.
//
// Queue.Enqueue is idempotent by intent ID. Claim selects the oldest due
// pending intent, or a running intent whose lease expired, and assigns a
// random claim token. Renew, Complete, Retry and CancelClaim update an intent
// only while that token owns the claim, fencing updates from earlier attempts.
//
// Worker invokes the handler for a claim while renewing its lease. A nil or
// missing handler is an ordinary failure and is retried. Handler errors are
// retried with deterministic jittered exponential backoff; Stop marks the
// intent cancelled and records the underlying error. If lease renewal fails,
// the handler context is cancelled and its result is discarded as fenced.
// RunPool starts the requested number of workers, defaults to four, and stops
// the pool when its context is cancelled or a queue operation fails.
package work
