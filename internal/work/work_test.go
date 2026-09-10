package work

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
)

func TestQueueClaimsFencesAndRetries(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	queue := New(store)
	now := time.Unix(100, 0)
	id, err := queue.Enqueue(ctx, Intent{Kind: "delivery", EventID: "event-1", Target: "target", Payload: "{}", NextAt: now})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, now, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if claimed.ID != id || claimed.Attempts != 1 || claimed.ClaimToken == "" {
		t.Fatalf("bad claim: %#v", claimed)
	}
	ok, err := queue.Complete(ctx, claimed.ID, "wrong", now)
	if err != nil || ok {
		t.Fatal("wrong token completed claim")
	}
	ok, err = queue.Retry(ctx, claimed.ID, claimed.ClaimToken, errors.New("temporary"), now)
	if err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}
	items, err := queue.Pending(ctx, "delivery")
	if err != nil || len(items) != 1 {
		t.Fatalf("pending = %#v, %v", items, err)
	}
	if items[0].LastError != "temporary" || !items[0].NextAt.After(now) {
		t.Fatalf("retry metadata = %#v", items[0])
	}
}

func TestEnqueueUsesSharedIntentID(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	intent := Intent{Kind: "delivery", EventID: "event-1", Target: "target"}
	id, err := queue.Enqueue(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if want := storage.IntentID(intent.Kind, intent.EventID, intent.Target); id != want {
		t.Fatalf("intent id = %q, want %q", id, want)
	}
}

func TestCompletePendingTxDoesNotTouchRunningIntent(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	ctx := context.Background()
	pendingID, err := queue.Enqueue(ctx, Intent{Kind: "pending", EventID: "pending", Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	var completed bool
	if err := store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		completed, err = CompletePendingTx(ctx, tx, pendingID, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("pending intent was not completed")
	}

	runningID, err := queue.Enqueue(ctx, Intent{Kind: "running", EventID: "running", Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, time.Now(), time.Minute)
	if err != nil || claimed == nil || claimed.ID != runningID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := store.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := CompletePendingTx(ctx, tx, runningID, time.Now())
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("running intent was completed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items, err := queue.List(ctx, "running")
	if err != nil || len(items) != 1 {
		t.Fatalf("running intents = %#v, %v", items, err)
	}
}

func TestCancelClaimRequiresCurrentToken(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	ctx := context.Background()
	id, err := queue.Enqueue(ctx, Intent{Kind: "terminal", EventID: "event", Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, time.Now(), time.Minute)
	if err != nil || claimed == nil || claimed.ID != id {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	ok, err := queue.CancelClaim(ctx, id, "stale-token", errors.New("stale"), time.Now())
	if err != nil || ok {
		t.Fatalf("stale cancel = %v, %v", ok, err)
	}
	if ok, err := queue.CancelClaim(ctx, id, claimed.ClaimToken, errors.New("terminal"), time.Now()); err != nil || !ok {
		t.Fatalf("current cancel = %v, %v", ok, err)
	}
	items, err := queue.List(ctx, "cancelled")
	if err != nil || len(items) != 1 || items[0].LastError != "terminal" {
		t.Fatalf("cancelled intents = %#v, %v", items, err)
	}
}

func TestWorkerStopCancelsClaimWithoutRetry(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	ctx := context.Background()
	if _, err := queue.Enqueue(ctx, Intent{Kind: "terminal", EventID: "event", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewWorker(queue, map[string]Handler{"terminal": func(context.Context, Intent) error {
			return Stop(errors.New("permanent"))
		}}).Run(workerCtx)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		items, err := queue.List(ctx, "cancelled")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 1 {
			if items[0].LastError != "permanent" {
				t.Fatalf("last error = %q", items[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not cancel terminal intent")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v", err)
	}
}

func TestExpiredClaimCanBeReclaimed(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	queue := New(store)
	now := time.Unix(100, 0)
	if _, err := queue.Enqueue(ctx, Intent{Kind: "push", EventID: "event-1", Target: "target", NextAt: now}); err != nil {
		t.Fatal(err)
	}
	one, err := queue.Claim(ctx, now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	two, err := queue.Claim(ctx, now.Add(2*time.Second), time.Second)
	if err != nil || two == nil || two.ClaimToken == one.ClaimToken {
		t.Fatalf("reclaim = %#v, %v", two, err)
	}
	ok, err := queue.Complete(ctx, one.ID, one.ClaimToken, now.Add(2*time.Second))
	if err != nil || ok {
		t.Fatal("expired token completed reclaimed work")
	}
}

func TestWorkerLeavesClaimReclaimableOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := openStore(t)
	queue := New(store)
	if _, err := queue.Enqueue(context.Background(), Intent{Kind: "slow", EventID: "event-1", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	worker := NewWorker(queue, map[string]Handler{"slow": func(context.Context, Intent) error { close(started); <-ctx.Done(); return ctx.Err() }})
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v", err)
	}
	items, err := queue.List(context.Background(), "running")
	if err != nil || len(items) != 1 {
		t.Fatalf("running = %#v, %v", items, err)
	}
}

func TestWorkersRenewLongAttemptWithoutConcurrentDuplicate(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	if _, err := queue.Enqueue(context.Background(), Intent{Kind: "slow", EventID: "event-1", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active atomic.Int32
	var maximum atomic.Int32
	var calls atomic.Int32
	handler := func(handlerCtx context.Context, _ Intent) error {
		calls.Add(1)
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		defer active.Add(-1)
		<-handlerCtx.Done()
		return handlerCtx.Err()
	}
	workerOptions := WorkerOptions{Lease: 2 * time.Second, Renew: 500 * time.Millisecond}
	workers := []*Worker{NewWorkerWithOptions(queue, map[string]Handler{"slow": handler}, workerOptions), NewWorkerWithOptions(queue, map[string]Handler{"slow": handler}, workerOptions)}
	errs := make(chan error, len(workers))
	for _, worker := range workers {
		go func() { errs <- worker.Run(ctx) }()
	}
	deadline := time.After(4 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not start")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(2500 * time.Millisecond)
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent handlers = %d", got)
	}
	cancel()
	for range workers {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("worker error = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}

func TestRunPoolCancelsWorkersOnQueueError(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	done := make(chan error, 1)
	go func() {
		done <- RunPool(context.Background(), queue, map[string]Handler{}, PoolOptions{Workers: 2})
	}()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunPool returned nil after queue failure")
		}
	case <-time.After(time.Second):
		t.Fatal("RunPool did not stop after queue failure")
	}
}

func TestRunPoolWaitsForActiveHandlersOnCancellation(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	if _, err := queue.Enqueue(context.Background(), Intent{Kind: "slow", EventID: "event-1", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunPool(ctx, queue, map[string]Handler{
			"slow": func(handlerCtx context.Context, _ Intent) error {
				close(started)
				<-handlerCtx.Done()
				return handlerCtx.Err()
			},
		}, PoolOptions{Workers: 1})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pool did not start handler")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunPool error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunPool did not wait for active handler")
	}
}

func TestWorkerCancellationFencesActiveAttempt(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	intentID, err := queue.Enqueue(context.Background(), Intent{Kind: "cancel", EventID: "event-1", Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	worker := NewWorkerWithOptions(queue, map[string]Handler{"cancel": func(handlerCtx context.Context, _ Intent) error {
		close(started)
		<-handlerCtx.Done()
		return handlerCtx.Err()
	}}, WorkerOptions{Lease: 2 * time.Second, Renew: 100 * time.Millisecond})
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if err := queue.Cancel(context.Background(), intentID, time.Now()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v", err)
	}
	items, err := queue.List(context.Background(), "cancelled")
	if err != nil || len(items) != 1 {
		t.Fatalf("cancelled = %#v, %v", items, err)
	}
}

func TestWorkerContinuesAfterCancellationFencesAttempt(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	firstID, err := queue.Enqueue(context.Background(), Intent{Kind: "cancel", EventID: "first", Target: "target"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	secondDone := make(chan struct{})
	worker := NewWorkerWithOptions(queue, map[string]Handler{"cancel": func(handlerCtx context.Context, intent Intent) error {
		if intent.EventID == "first" {
			close(started)
			<-handlerCtx.Done()
			return handlerCtx.Err()
		}
		close(secondDone)
		return nil
	}}, WorkerOptions{Lease: 2 * time.Second, Renew: 50 * time.Millisecond})
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start first intent")
	}
	if err := queue.Cancel(context.Background(), firstID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(context.Background(), Intent{Kind: "cancel", EventID: "second", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker stopped after fenced cancellation")
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v", err)
	}
}

func TestQueueMetricsNormalizeUnknownKinds(t *testing.T) {
	store := openStore(t)
	queue := New(store)
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(queue.Metrics()); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(context.Background(), Intent{Kind: "tenant-supplied-kind", EventID: "event-1", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "relay_work_pending" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "kind" && label.GetValue() == "tenant-supplied-kind" {
					t.Fatal("unbounded work kind label")
				}
			}
		}
	}
}

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
