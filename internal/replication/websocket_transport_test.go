package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

type silentNegentropyDialer struct{ calls int }

type silentNegentropySocket struct{ responses chan []byte }

func (d *silentNegentropyDialer) Dial(_ context.Context, _ string) (Socket, *http.Response, error) {
	d.calls++
	return &silentNegentropySocket{responses: make(chan []byte, 1)}, nil, nil
}

func (s *silentNegentropySocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case response := <-s.responses:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *silentNegentropySocket) Write(_ context.Context, data []byte) error {
	var message []json.RawMessage
	if err := json.Unmarshal(data, &message); err != nil || len(message) < 2 {
		return nil
	}
	var kind, id string
	_ = json.Unmarshal(message[0], &kind)
	_ = json.Unmarshal(message[1], &id)
	if kind != "REQ" {
		return nil // legacy peer silently ignores NEG-OPEN
	}
	response, _ := json.Marshal([]any{"EOSE", id})
	s.responses <- response
	return nil
}

func (s *silentNegentropySocket) Close(error) error { return nil }

func TestQuerySynchronizedFallsBackAfterSilentLegacyNegentropyPeer(t *testing.T) {
	dialer := &silentNegentropyDialer{}
	transport := &NostrTransport{Dialer: dialer, Timeout: 10 * time.Millisecond}
	items, err := transport.QuerySynchronized(context.Background(), "ws://relay.example", event.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("fallback returned %d events", len(items))
	}
	if dialer.calls != 2 {
		t.Fatalf("transport dials = %d, want negentropy probe plus REQ fallback", dialer.calls)
	}
}

func TestQuerySynchronizedCachesSilentLegacyPeerPerTransport(t *testing.T) {
	dialer := &silentNegentropyDialer{}
	cache := &LegacyCache{}
	for i := 0; i < 2; i++ {
		transport := &NostrTransport{Dialer: dialer, Timeout: 10 * time.Millisecond, LegacyCache: cache}
		if _, err := transport.QuerySynchronized(context.Background(), "ws://legacy.example", event.Filter{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if dialer.calls != 3 {
		t.Fatalf("transport dials = %d, want one negentropy probe and two REQ pulls", dialer.calls)
	}
}

func TestQueryNegentropyIDsReportsMissingRequestedEvents(t *testing.T) {
	dialer := &batchQueryDialer{events: map[string]event.Event{}}
	transport := &NostrTransport{Dialer: dialer, Timeout: time.Second}
	items, err := transport.queryNegentropyIDs(context.Background(), "ws://relay.example", event.Filter{}, []string{"missing"})
	if err != ErrPullIncomplete {
		t.Fatalf("error = %v, want ErrPullIncomplete", err)
	}
	if len(items) != 0 {
		t.Fatalf("returned %d events for missing ID", len(items))
	}
}

func TestLegacyCacheExpiresAndBoundsEntries(t *testing.T) {
	cache := &LegacyCache{entries: map[string]time.Time{
		"ws://expired.example": time.Now().Add(-legacyProbeTTL - time.Second),
	}}
	if cache.known("ws://expired.example") {
		t.Fatal("expired legacy probe was reused")
	}
	for i := 0; i < 65; i++ {
		cache.remember(fmt.Sprintf("ws://relay-%d.example", i))
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 64 {
		t.Fatalf("cache entries = %d, want 64", len(cache.entries))
	}
}
