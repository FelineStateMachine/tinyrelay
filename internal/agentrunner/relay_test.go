package agentrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/coder/websocket"
)

const relayTestSecret = "0000000000000000000000000000000000000000000000000000000000000001"

func TestResolveRootQueriesUnseenParentsWithoutDispatchingThem(t *testing.T) {
	room := "room"
	root := relayTestEvent(t, 11, room, "")
	middle := relayTestEvent(t, 12, room, root.ID)
	leaf := relayTestEvent(t, 12, room, middle.ID)
	events := map[string]event.Event{root.ID: root, middle.ID: middle, leaf.ID: leaf}
	var mu sync.Mutex
	var closed []string
	ready := make(chan struct{}, 1)
	server := relayTestServer(t, events, &mu, &closed, ready)
	defer server.Close()

	client := RelayClient{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Secret: relayTestSecret, Rooms: []string{room}}
	var dispatched int
	client.OnEvent = func(event.Event) { dispatched++ }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish its initial subscription")
	}

	queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
	defer queryCancel()
	got, err := client.ResolveRoot(queryCtx, room, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != root.ID {
		t.Fatalf("root=%s, want %s", got, root.ID)
	}
	if dispatched != 0 {
		t.Fatalf("query events dispatched to agent callback: %d", dispatched)
	}
	mu.Lock()
	closeCount := len(closed)
	mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for closeCount < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		closeCount = len(closed)
		mu.Unlock()
	}
	if closeCount != 3 {
		t.Fatalf("CLOSE frames=%d, want 3", closeCount)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
}

func TestResolveRootRejectsInvalidQueriedEvents(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T) (string, event.Event)
		want string
	}{
		{"wrong room", func(t *testing.T) (string, event.Event) {
			e := relayTestEvent(t, 11, "other", "")
			return "room", e
		}, "another room"},
		{"invalid kind", func(t *testing.T) (string, event.Event) {
			e := relayTestEvent(t, 1, "room", "")
			return "room", e
		}, "invalid kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			room, e := tt.make(t)
			server := relayTestServer(t, map[string]event.Event{e.ID: e}, nil, nil, make(chan struct{}, 1))
			defer server.Close()
			client, stop := runningTestRelay(t, server, room)
			defer stop()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := client.ResolveRoot(ctx, room, e.ID)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestResolveRootTimesOutAndClosesQuery(t *testing.T) {
	root := relayTestEvent(t, 11, "room", "")
	server := relayTestServer(t, map[string]event.Event{}, nil, nil, make(chan struct{}, 1))
	defer server.Close()
	client, stop := runningTestRelay(t, server, "room")
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.ResolveRoot(ctx, "room", root.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
}

func relayTestEvent(t *testing.T, kind int, room, parent string) event.Event {
	t.Helper()
	tags := [][]string{{"h", room}}
	if parent != "" {
		tags = append(tags, []string{"e", parent, "", "root"})
	}
	e := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: tags, Content: "test"}
	if err := event.Sign(&e, relayTestSecret); err != nil {
		t.Fatal(err)
	}
	return e
}

func runningTestRelay(t *testing.T, server *httptest.Server, room string) (*RelayClient, func()) {
	t.Helper()
	client := &RelayClient{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Secret: relayTestSecret, Rooms: []string{room}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		client.mu.Lock()
		connected := client.conn != nil
		client.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not connect")
		}
		time.Sleep(time.Millisecond)
	}
	return client, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("relay did not stop")
		}
	}
}

func relayTestServer(t *testing.T, events map[string]event.Event, mu *sync.Mutex, closed *[]string, ready chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var frame []json.RawMessage
			if json.Unmarshal(raw, &frame) != nil || len(frame) < 2 {
				continue
			}
			var kind, sub string
			_ = json.Unmarshal(frame[0], &kind)
			_ = json.Unmarshal(frame[1], &sub)
			switch kind {
			case "REQ":
				if sub == "tiny-agent" {
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`["EOSE","tiny-agent"]`))
					select {
					case ready <- struct{}{}:
					default:
					}
					continue
				}
				var filter map[string]any
				if len(frame) > 2 {
					_ = json.Unmarshal(frame[2], &filter)
				}
				var id string
				if value, ok := filter["ids"].([]any); ok && len(value) > 0 {
					id, _ = value[0].(string)
				}
				if e, ok := events[id]; ok {
					payload, _ := json.Marshal([]any{"EVENT", sub, e})
					_ = conn.Write(r.Context(), websocket.MessageText, payload)
				}
				if _, found := events[id]; found {
					payload, _ := json.Marshal([]any{"EOSE", sub})
					_ = conn.Write(r.Context(), websocket.MessageText, payload)
				}
			case "CLOSE":
				payload, _ := json.Marshal([]any{"CLOSED", sub, "subscription closed"})
				_ = conn.Write(r.Context(), websocket.MessageText, payload)
				if mu != nil && closed != nil {
					mu.Lock()
					*closed = append(*closed, sub)
					mu.Unlock()
				}
			}
		}
	}))
}
