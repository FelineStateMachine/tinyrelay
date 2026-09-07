package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

type negCloseDialer struct {
	mu    sync.Mutex
	calls int
	sock  *negCloseSocket
}

type negCloseSocket struct {
	mu        sync.Mutex
	responses [][]byte
	requests  int
}

type batchQueryDialer struct {
	mu      sync.Mutex
	sockets []*batchQuerySocket
	events  map[string]event.Event
}

type batchQuerySocket struct {
	mu        sync.Mutex
	responses [][]byte
	filters   []event.Filter
	events    map[string]event.Event
}

func (d *batchQueryDialer) Dial(context.Context, string) (Socket, *http.Response, error) {
	socket := &batchQuerySocket{events: d.events}
	d.mu.Lock()
	d.sockets = append(d.sockets, socket)
	d.mu.Unlock()
	return socket, nil, nil
}

func (s *batchQuerySocket) Read(ctx context.Context) ([]byte, error) {
	for {
		s.mu.Lock()
		if len(s.responses) > 0 {
			data := s.responses[0]
			s.responses = s.responses[1:]
			s.mu.Unlock()
			return data, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (s *batchQuerySocket) Write(_ context.Context, data []byte) error {
	var message []json.RawMessage
	if err := json.Unmarshal(data, &message); err != nil || len(message) < 3 {
		return nil
	}
	var kind, id string
	_ = json.Unmarshal(message[0], &kind)
	_ = json.Unmarshal(message[1], &id)
	if kind != "REQ" {
		return nil
	}
	filter, err := event.ParseFilter(message[2])
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.filters = append(s.filters, filter)
	for _, eventID := range filter.IDs {
		if item, ok := s.events[eventID]; ok {
			response, _ := json.Marshal([]any{"EVENT", id, item})
			s.responses = append(s.responses, response)
		}
	}
	response, _ := json.Marshal([]any{"EOSE", id})
	s.responses = append(s.responses, response)
	s.mu.Unlock()
	return nil
}

func (s *batchQuerySocket) Close(error) error { return nil }

func (d *negCloseDialer) Dial(context.Context, string) (Socket, *http.Response, error) {
	d.mu.Lock()
	d.calls++
	if d.sock == nil {
		d.sock = &negCloseSocket{}
	}
	sock := d.sock
	d.mu.Unlock()
	return sock, nil, nil
}

func (s *negCloseSocket) Read(ctx context.Context) ([]byte, error) {
	for {
		s.mu.Lock()
		if len(s.responses) > 0 {
			data := s.responses[0]
			s.responses = s.responses[1:]
			s.mu.Unlock()
			return data, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (s *negCloseSocket) Write(_ context.Context, data []byte) error {
	var message []json.RawMessage
	if err := json.Unmarshal(data, &message); err != nil || len(message) < 2 {
		return nil
	}
	var kind, id string
	_ = json.Unmarshal(message[0], &kind)
	_ = json.Unmarshal(message[1], &id)
	var response []any
	switch kind {
	case "NEG-OPEN":
		response = []any{"NEG-CLOSE", id}
	case "REQ":
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		response = []any{"EOSE", id}
	default:
		return nil
	}
	raw, _ := json.Marshal(response)
	s.mu.Lock()
	s.responses = append(s.responses, raw)
	s.mu.Unlock()
	return nil
}

func (s *negCloseSocket) Close(error) error { return nil }

func TestQueryNegentropyEqualInventoryCloseCompletesImmediately(t *testing.T) {
	dialer := &negCloseDialer{}
	transport := &NostrTransport{Dialer: dialer, Timeout: 2 * time.Second}
	started := time.Now()
	events, err := transport.QueryNegentropy(context.Background(), "ws://relay.example", event.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("equal inventory returned %d events", len(events))
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("equal inventory close took %s; likely waited for timeout", elapsed)
	}
	dialer.mu.Lock()
	calls := dialer.calls
	sock := dialer.sock
	dialer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("transport dials = %d, want negentropy only", calls)
	}
	sock.mu.Lock()
	requests := sock.requests
	sock.mu.Unlock()
	if requests != 0 {
		t.Fatalf("equal inventory issued %d unrestricted REQ requests", requests)
	}
}

func TestNegentropyNeededIDsAreFetchedInBoundedFilteredBatches(t *testing.T) {
	const total = negentropyFetchBatch + 1
	secret := "1111111111111111111111111111111111111111111111111111111111111111"
	dialer := &batchQueryDialer{events: make(map[string]event.Event, total)}
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		item := event.Event{Kind: 1, CreatedAt: int64(i + 1), Content: fmt.Sprintf("item-%d", i), Tags: [][]string{{"p", "recipient"}}}
		if err := event.Sign(&item, secret); err != nil {
			t.Fatal(err)
		}
		dialer.events[item.ID] = item
		ids = append(ids, item.ID)
	}
	filter := event.Filter{Kinds: []int{1}, Tags: map[string][]string{"p": {"recipient"}}}
	transport := &NostrTransport{Dialer: dialer, Timeout: time.Second}
	got, err := transport.queryNegentropyIDs(context.Background(), "ws://relay.example", filter, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("fetched %d events, want %d", len(got), total)
	}
	dialer.mu.Lock()
	sockets := append([]*batchQuerySocket(nil), dialer.sockets...)
	dialer.mu.Unlock()
	if len(sockets) != 2 {
		t.Fatalf("query opened %d sockets, want two batches", len(sockets))
	}
	for i, socket := range sockets {
		socket.mu.Lock()
		if len(socket.filters) != 1 {
			socket.mu.Unlock()
			t.Fatalf("batch %d recorded %d filters", i, len(socket.filters))
		}
		batch := socket.filters[0]
		socket.mu.Unlock()
		if len(batch.IDs) == 0 || len(batch.IDs) > negentropyFetchBatch || batch.Limit == nil || *batch.Limit != len(batch.IDs) || len(batch.Kinds) != 1 || batch.Kinds[0] != 1 || len(batch.Tags["p"]) != 1 || batch.Tags["p"][0] != "recipient" {
			t.Fatalf("batch %d did not preserve bounded membership: %#v", i, batch)
		}
	}
}

type tiePushTransport struct{ sent []string }

func (p *tiePushTransport) Send(context.Context, string, event.Event) (DeliveryResult, error) {
	p.sent = append(p.sent, "sent")
	return DeliveryResult{Accepted: true}, nil
}

func TestPushCursorUsesSequenceWhenCreatedAtTies(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, t.TempDir()+"/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret := "1111111111111111111111111111111111111111111111111111111111111111"
	for i := 0; i < 2; i++ {
		e := event.Event{Kind: 1, CreatedAt: 100, Content: string(rune('a' + i))}
		if err := event.Sign(&e, secret); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(ctx, e, storage.SaveOptions{Now: 100}); err != nil {
			t.Fatal(err)
		}
	}
	transport := &tiePushTransport{}
	stats, cursor, err := PushAfter(ctx, store, transport, "ws://relay.example", 0, event.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 2 || len(transport.sent) != 2 || cursor < 2 {
		t.Fatalf("same-timestamp push = stats=%+v sent=%d cursor=%d", stats, len(transport.sent), cursor)
	}
	stats, next, err := PushAfter(ctx, store, transport, "ws://relay.example", cursor, event.Filter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 || next != cursor || len(transport.sent) != 2 {
		t.Fatalf("cursor replay = stats=%+v next=%d sent=%d", stats, next, len(transport.sent))
	}
}
