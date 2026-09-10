package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
)

const defaultLease = 30 * time.Second

// ErrClaimLost denotes loss of worker lease ownership.
var ErrClaimLost = errors.New("work: claim lost")

type stopError struct{ err error }

func (e *stopError) Error() string {
	if e.err == nil {
		return "work: stopped"
	}
	return e.err.Error()
}

func (e *stopError) Unwrap() error { return e.err }

// Stop marks a handler failure as terminal. Workers cancel the claimed intent
// instead of retrying it, while retaining the underlying error for operators.
func Stop(err error) error { return &stopError{err: err} }

// Intent is a durable unit of work. ClaimToken identifies the current claim;
// ClaimUntil determines when another worker can claim an expired lease.
// State is pending, running, completed or cancelled.
type Intent struct {
	ID         string
	Kind       string
	EventID    string
	Target     string
	Payload    string
	State      string
	Attempts   int
	NextAt     time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastError  string
	ClaimToken string
	ClaimUntil time.Time
}

// Queue implements durable claiming and fencing over a storage Store.
type Queue struct {
	store *storage.Store
	depth *prometheus.GaugeVec
}

// New creates a queue. Metrics returns a collector that hosts may register.
func New(store *storage.Store) *Queue {
	return &Queue{store: store, depth: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "relay_work_pending", Help: "Pending durable work intents."}, []string{"kind"})}
}

// Metrics returns the per-kind pending gauge collector.
func (q *Queue) Metrics() prometheus.Collector { return q.depth }

// Enqueue inserts an intent idempotently.
func (q *Queue) Enqueue(ctx context.Context, intent Intent) (string, error) {
	if intent.Kind == "" || intent.EventID == "" || intent.Target == "" {
		return "", errors.New("work: kind, event id, and target are required")
	}
	if intent.ID == "" {
		intent.ID = storage.IntentID(intent.Kind, intent.EventID, intent.Target)
	}
	now := time.Now().Unix()
	if intent.NextAt.IsZero() {
		intent.NextAt = time.Unix(now, 0)
	}
	err := q.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intents(id,kind,event_id,target,payload,next_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, intent.ID, intent.Kind, intent.EventID, intent.Target, intent.Payload, intent.NextAt.Unix(), now, now)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("work: enqueue: %w", err)
	}
	if metricKind(intent.Kind) == "other" {
		q.refresh(ctx, "")
	} else {
		q.refresh(ctx, intent.Kind)
	}
	return intent.ID, nil
}

// Claim atomically claims the oldest due pending or expired intent.
func (q *Queue) Claim(ctx context.Context, now time.Time, lease time.Duration) (*Intent, error) {
	if lease <= 0 {
		lease = defaultLease
	}
	nowUnix := now.Unix()
	until := now.Add(lease).Unix()
	token, err := token()
	if err != nil {
		return nil, err
	}
	var result *Intent
	err = q.store.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `WITH candidate AS (
 SELECT id FROM work_intents
 WHERE (state='pending' AND next_at<=?) OR (state='running' AND claim_until<=?)
 ORDER BY next_at,created_at,id LIMIT 1
)
UPDATE work_intents SET state='running',attempts=attempts+1,updated_at=?,claim_token=?,claim_until=?
WHERE id=(SELECT id FROM candidate)
RETURNING id,kind,event_id,target,payload,state,attempts,next_at,created_at,updated_at,last_error,claim_token,claim_until`, nowUnix, nowUnix, nowUnix, token, until)
		intent := Intent{}
		err := scanIntent(row, &intent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = &intent
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("work: claim: %w", err)
	}
	if result != nil {
		if metricKind(result.Kind) == "other" {
			q.refresh(ctx, "")
		} else {
			q.refresh(ctx, result.Kind)
		}
	}
	return result, nil
}

// Renew extends a claim if the token still fences the caller.
func (q *Queue) Renew(ctx context.Context, id, claimToken string, now time.Time, lease time.Duration) (bool, error) {
	if lease <= 0 {
		lease = defaultLease
	}
	result, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET claim_until=?,updated_at=? WHERE id=? AND state='running' AND claim_token=?`, now.Add(lease).Unix(), now.Unix(), id, claimToken)
	if err != nil {
		return false, fmt.Errorf("work: renew: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// Complete marks a running intent complete if the token still fences the caller.
func (q *Queue) Complete(ctx context.Context, id, claimToken string, now time.Time) (bool, error) {
	result, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='completed',updated_at=?,claim_token='',claim_until=0 WHERE id=? AND state='running' AND claim_token=?`, now.Unix(), id, claimToken)
	if err != nil {
		return false, fmt.Errorf("work: complete: %w", err)
	}
	count, err := result.RowsAffected()
	if count == 1 {
		q.refresh(ctx, "")
	}
	return count == 1, err
}

// CompletePendingTx marks a pending intent complete in the caller's
// transaction. Running intents are left untouched so an active worker keeps
// ownership of its claim.
func CompletePendingTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='completed',updated_at=?,claim_token='',claim_until=0 WHERE id=? AND state='pending'`, now.Unix(), id)
	if err != nil {
		return false, fmt.Errorf("work: complete pending: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// CancelClaim marks a running intent terminal if the claim token still fences
// the caller.
func (q *Queue) CancelClaim(ctx context.Context, id, claimToken string, cause error, now time.Time) (bool, error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	result, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='cancelled',updated_at=?,last_error=?,claim_token='',claim_until=0 WHERE id=? AND state='running' AND claim_token=?`, now.Unix(), message, id, claimToken)
	if err != nil {
		return false, fmt.Errorf("work: cancel claim: %w", err)
	}
	count, err := result.RowsAffected()
	if count == 1 {
		q.refresh(ctx, "")
	}
	return count == 1, err
}

// Retry returns a failed claim to pending with unbounded exponential backoff.
func (q *Queue) Retry(ctx context.Context, id, claimToken string, cause error, now time.Time) (bool, error) {
	var attempts int
	err := q.store.DB().QueryRowContext(ctx, `SELECT attempts FROM work_intents WHERE id=? AND state='running' AND claim_token=?`, id, claimToken).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("work: retry lookup: %w", err)
	}
	delay := backoff(id, attempts)
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	result, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='pending',next_at=?,updated_at=?,last_error=?,claim_token='',claim_until=0 WHERE id=? AND state='running' AND claim_token=?`, now.Add(delay).Unix(), now.Unix(), message, id, claimToken)
	if err != nil {
		return false, fmt.Errorf("work: retry: %w", err)
	}
	count, err := result.RowsAffected()
	if count == 1 {
		q.refresh(ctx, "")
	}
	return count == 1, err
}

// RetryNow makes an intent immediately eligible, regardless of its current pending delay.
func (q *Queue) RetryNow(ctx context.Context, id string, now time.Time) error {
	_, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='pending',next_at=?,updated_at=?,claim_token='',claim_until=0 WHERE id=? AND state!='completed' AND state!='cancelled'`, now.Unix(), now.Unix(), id)
	if err == nil {
		q.refresh(ctx, "")
	}
	return err
}

// Cancel prevents future claims. A running claim can be cancelled administratively.
func (q *Queue) Cancel(ctx context.Context, id string, now time.Time) error {
	_, err := q.store.DB().ExecContext(ctx, `UPDATE work_intents SET state='cancelled',updated_at=?,claim_token='',claim_until=0 WHERE id=?`, now.Unix(), id)
	if err == nil {
		q.refresh(ctx, "")
	}
	return err
}

// Pending returns pending and running intents, optionally filtered by kind.
func (q *Queue) Pending(ctx context.Context, kind string) ([]Intent, error) {
	return q.list(ctx, kind, "pending,running")
}

// List returns intents in the requested state; an empty state lists all states.
func (q *Queue) List(ctx context.Context, state string) ([]Intent, error) {
	return q.list(ctx, "", state)
}

func (q *Queue) list(ctx context.Context, kind, state string) ([]Intent, error) {
	query := `SELECT id,kind,event_id,target,payload,state,attempts,next_at,created_at,updated_at,last_error,claim_token,claim_until FROM work_intents WHERE 1=1`
	args := []any{}
	if kind != "" {
		query += " AND kind=?"
		args = append(args, kind)
	}
	if state == "pending,running" {
		query += " AND state IN ('pending','running')"
	} else if state != "" {
		query += " AND state=?"
		args = append(args, state)
	}
	query += " ORDER BY next_at,created_at,id"
	rows, err := q.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Intent
	for rows.Next() {
		intent := Intent{}
		if err := scanIntent(rows, &intent); err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	return result, rows.Err()
}

func (q *Queue) refresh(ctx context.Context, kind string) {
	if kind == "" {
		rows, err := q.store.DB().QueryContext(ctx, `SELECT kind,count(*) FROM work_intents WHERE state='pending' GROUP BY kind`)
		if err != nil {
			return
		}
		counts := make(map[string]int)
		for rows.Next() {
			var value string
			var count int
			if rows.Scan(&value, &count) == nil {
				counts[metricKind(value)] += count
			}
		}
		_ = rows.Close()
		for _, value := range knownMetricKinds {
			if _, ok := counts[value]; !ok {
				counts[value] = 0
			}
		}
		for value, count := range counts {
			q.depth.WithLabelValues(value).Set(float64(count))
		}
		return
	}
	var count int
	if err := q.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE kind=? AND state='pending'`, kind).Scan(&count); err == nil {
		q.depth.WithLabelValues(metricKind(kind)).Set(float64(count))
	}
}

var knownMetricKinds = []string{"delivery", "push", "replication", "webhook", "job", "other"}

func metricKind(kind string) string {
	for _, known := range knownMetricKinds[:len(knownMetricKinds)-1] {
		if kind == known {
			return known
		}
	}
	return "other"
}

// Handler processes one claimed intent while the worker renews its lease.
// A nil error completes the claim; an error schedules a retry. Stop marks a
// terminal failure. The handler observes ctx cancellation so the worker can
// finish shutdown or release an attempt whose claim was lost. A retry can
// repeat an external effect after an interrupted acknowledgment; handlers own
// deduplication of those effects.
type Handler func(ctx context.Context, intent Intent) error

// WorkerOptions controls claim leases and optional handler timing observation.
// Lease and Renew default to 30 seconds and one third of the lease.
type WorkerOptions struct {
	Lease   time.Duration
	Renew   time.Duration
	Observe func(kind, outcome string, duration time.Duration)
}

// PoolOptions controls a pool of durable workers. Workers less than or equal
// to zero use the default of four.
type PoolOptions struct {
	Workers int
	Worker  WorkerOptions
}

const defaultWorkers = 4

// RunPool runs a worker pool until ctx is cancelled or a worker reports a
// queue error. The first queue error cancels the remaining workers and is
// returned after they have stopped.
func RunPool(ctx context.Context, queue *Queue, handlers map[string]Handler, options PoolOptions) error {
	workers := options.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := NewWorkerWithOptions(queue, handlers, options.Worker).Run(workerCtx); err != nil && workerCtx.Err() == nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}()
	}
	group.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

// Worker owns a queue lifecycle and processes one intent at a time.
type Worker struct {
	queue    *Queue
	handlers map[string]Handler
	options  WorkerOptions
}

// NewWorker creates a worker with copied handlers.
func NewWorker(queue *Queue, handlers map[string]Handler) *Worker {
	return NewWorkerWithOptions(queue, handlers, WorkerOptions{})
}

// NewWorkerWithOptions creates a worker with explicit lease behavior.
func NewWorkerWithOptions(queue *Queue, handlers map[string]Handler, options WorkerOptions) *Worker {
	copyHandlers := make(map[string]Handler, len(handlers))
	for kind, handler := range handlers {
		copyHandlers[kind] = handler
	}
	if options.Lease <= 0 {
		options.Lease = defaultLease
	}
	if options.Renew <= 0 {
		options.Renew = options.Lease / 3
	}
	if options.Renew <= 0 {
		options.Renew = time.Millisecond
	}
	return &Worker{queue: queue, handlers: copyHandlers, options: options}
}

// Run blocks until the context is cancelled or a durable queue error occurs.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		intent, err := w.queue.Claim(ctx, time.Now(), w.options.Lease)
		if err != nil {
			// SQLite may report a transaction cleanup error when cancellation
			// races a claim. The worker lifecycle is already stopping, so
			// preserve the caller's cancellation outcome.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if intent == nil {
			if err := wait(ctx, 100*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		handler := w.handlers[intent.Kind]
		if handler == nil {
			handler = func(context.Context, Intent) error { return errors.New("no handler registered") }
		}
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var lost atomic.Bool
		renewDone := make(chan struct{})
		go w.renew(attemptCtx, intent, cancelAttempt, &lost, renewDone)
		started := time.Now()
		err = handler(attemptCtx, *intent)
		cancelAttempt()
		<-renewDone
		if w.options.Observe != nil {
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			if lost.Load() {
				outcome = "fenced"
			}
			w.options.Observe(intent.Kind, outcome, time.Since(started))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if lost.Load() {
			continue
		}
		if err != nil {
			var terminal *stopError
			if errors.As(err, &terminal) {
				ok, cancelErr := w.queue.CancelClaim(ctx, intent.ID, intent.ClaimToken, terminal.err, time.Now())
				if cancelErr != nil {
					return cancelErr
				}
				if !ok {
					continue
				}
				continue
			}
			ok, retryErr := w.queue.Retry(ctx, intent.ID, intent.ClaimToken, err, time.Now())
			if retryErr != nil {
				return retryErr
			}
			if !ok {
				// A cancellation or another claimant may have fenced this
				// attempt while the handler was unwinding. Its result is stale;
				// keep the worker alive for other durable work.
				continue
			}
			continue
		}
		ok, err := w.queue.Complete(ctx, intent.ID, intent.ClaimToken, time.Now())
		if err != nil {
			return err
		}
		if !ok {
			// The claim can be fenced after the renewal loop's final check.
			// Discard this stale result and continue processing the queue.
			continue
		}
	}
}

func (w *Worker) renew(ctx context.Context, intent *Intent, cancel context.CancelFunc, lost *atomic.Bool, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(w.options.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			ok, err := w.queue.Renew(ctx, intent.ID, intent.ClaimToken, now, w.options.Lease)
			if err != nil || !ok {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type scanner interface{ Scan(...any) error }

func scanIntent(row scanner, intent *Intent) error {
	var next, created, updated, until int64
	err := row.Scan(&intent.ID, &intent.Kind, &intent.EventID, &intent.Target, &intent.Payload, &intent.State, &intent.Attempts, &next, &created, &updated, &intent.LastError, &intent.ClaimToken, &until)
	if err != nil {
		return err
	}
	intent.NextAt, intent.CreatedAt, intent.UpdatedAt, intent.ClaimUntil = time.Unix(next, 0), time.Unix(created, 0), time.Unix(updated, 0), time.Unix(until, 0)
	return nil
}

func token() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("work: claim token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func backoff(id string, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	seconds := math.Pow(2, float64(attempts))
	if seconds > float64(time.Hour/time.Second) {
		seconds = float64(time.Hour / time.Second)
	}
	sum := sha256.Sum256([]byte(id + fmt.Sprint(attempts)))
	jitter := (float64(sum[0])/255 - 0.5) * 0.4
	return time.Duration(seconds*(1+jitter)) * time.Second
}
