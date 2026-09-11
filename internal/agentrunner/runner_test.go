package agentrunner

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRunnerSerializesBatchesAndOtherRoomsProgress(t *testing.T) {
	start := make(chan string, 3)
	release := make(chan struct{})
	var mu sync.Mutex
	var batches [][]Mention
	r, err := New(Options{AllowedRooms: []string{"one", "two"}, Handle: func(ctx context.Context, b []Mention) error {
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
		start <- b[0].EventID
		if b[0].EventID == "a" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	defer r.Stop()
	add := func(id, room string) {
		t.Helper()
		if err := r.Enqueue(Mention{EventID: id, Author: "alice", Room: room}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "one")
	<-start
	add("b", "one")
	add("c", "one")
	add("d", "two")
	select {
	case id := <-start:
		if id != "d" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("second room blocked")
	}
	if err := r.Enqueue(Mention{EventID: "b", Author: "alice", Room: "one"}); !errors.Is(err, ErrDuplicate) {
		t.Fatal(err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 3 || len(batches[2]) != 2 {
		t.Fatalf("batches=%v", batches)
	}
}
func TestRunnerStopsActiveTaskAndRestoresJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	started := make(chan struct{})
	r, err := New(Options{AllowedRooms: []string{"one"}, StatePath: path, Handle: func(ctx context.Context, b []Mention) error { close(started); <-ctx.Done(); return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	r.Enqueue(Mention{Room: "one", Author: "alice", EventID: "a"})
	<-started
	if r.CancelFrom("one", "other") {
		t.Fatal("another author canceled work")
	}
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop hung")
	}
	if err := r.Enqueue(Mention{Room: "one", Author: "alice", EventID: "b"}); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	restored, err := New(Options{AllowedRooms: []string{"one"}, StatePath: path, Handle: func(ctx context.Context, b []Mention) error { got <- b[0].EventID; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	restored.Start(context.Background())
	defer restored.Stop()
	select {
	case id := <-got:
		if id != "a" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("unfinished work not recovered")
	}
}
func TestRunnerRejectsUnconfiguredRoomsAndRecoversAfterFailure(t *testing.T) {
	got := make(chan string, 2)
	r, err := New(Options{AllowedRooms: []string{"one"}, QueueLimit: 1, Handle: func(ctx context.Context, b []Mention) error {
		got <- b[0].EventID
		return errors.New("process crashed")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Enqueue(Mention{Room: "two", Author: "a", EventID: "a"}); !errors.Is(err, ErrUnauthorizedRoom) {
		t.Fatal(err)
	}
	r.Enqueue(Mention{Room: "one", Author: "a", EventID: "a"})
	if err := r.Enqueue(Mention{Room: "one", Author: "a", EventID: "b"}); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	r.Start(context.Background())
	defer r.Stop()
	<-got
	r.Enqueue(Mention{Room: "one", Author: "a", EventID: "b"})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("queue died after process crash")
	}
}

func TestRunnerDoesNotMixThreadBatches(t *testing.T) {
	got := make(chan []Mention, 2)
	r, err := New(Options{AllowedRooms: []string{"one"}, Handle: func(ctx context.Context, batch []Mention) error {
		got <- batch
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	defer r.Stop()
	for _, m := range []Mention{{Room: "one", Author: "a", EventID: "a", Root: "root-a"}, {Room: "one", Author: "b", EventID: "b", Root: "root-a"}, {Room: "one", Author: "c", EventID: "c", Root: "root-b"}} {
		if err := r.Enqueue(m); err != nil {
			t.Fatal(err)
		}
	}
	first := <-got
	second := <-got
	if len(first) != 2 || first[0].Root != "root-a" || len(second) != 1 || second[0].Root != "root-b" {
		t.Fatalf("batches=%v,%v", first, second)
	}
}

func TestRunnerCancelFromKeepsOtherAuthorsQueued(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	r, err := New(Options{AllowedRooms: []string{"one"}, Handle: func(ctx context.Context, batch []Mention) error {
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background())
	defer r.Stop()
	if err := r.Enqueue(Mention{Room: "one", Author: "alice", EventID: "a", Root: "root"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.Enqueue(Mention{Room: "one", Author: "bob", EventID: "b", Root: "root-b"}); err != nil {
		t.Fatal(err)
	}
	if !r.CancelFrom("one", "alice") {
		t.Fatal("alice cancellation was not accepted")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Enqueue(Mention{Room: "one", Author: "bob", EventID: "b"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("bob event should remain seen: %v", err)
	}
}
