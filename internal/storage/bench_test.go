package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func BenchmarkSave(b *testing.B) {
	store := benchmarkStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Save(ctx, benchmarkEvent(i, "benchmark"), SaveOptions{Now: int64(i + 1), SearchMode: "full"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuery(b *testing.B) {
	store := benchmarkStore(b)
	seedEvents(b, store, 1000)
	filter := event.Filter{Kinds: []int{1}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Query(context.Background(), filter, QueryOptions{Now: 2_000, Limit: 100}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTextSearch(b *testing.B) {
	store := benchmarkStore(b)
	seedEvents(b, store, 1000)
	filter := event.Filter{Search: "needle"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Query(context.Background(), filter, QueryOptions{Now: 2_000, Limit: 100}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	store, err := Open(context.Background(), b.TempDir()+"/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func seedEvents(b *testing.B, store *Store, count int) {
	b.Helper()
	for i := 0; i < count; i++ {
		if _, err := store.Save(context.Background(), benchmarkEvent(i, "seed needle text"), SaveOptions{Now: int64(i + 1), SearchMode: "full"}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkEvent(index int, content string) event.Event {
	return event.Event{ID: fmt.Sprintf("%064x", index+1), PubKey: fmt.Sprintf("%064x", 1), CreatedAt: int64(index + 1), Kind: 1, Tags: [][]string{}, Content: content, Sig: fmt.Sprintf("%0128x", index+1)}
}
