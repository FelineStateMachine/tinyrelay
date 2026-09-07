package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/syncprotocol"
	"github.com/coder/websocket"
)

type testBackend struct{}

func (testBackend) Publish(context.Context, event.Event, Session) (string, error) { return "", nil }
func (testBackend) Query(context.Context, []event.Filter, Session) ([]event.Event, error) {
	return nil, nil
}
func (testBackend) Count(context.Context, []event.Filter, Session) (any, error) { return 0, nil }

type resultBackend struct{ reason string }

func (b resultBackend) Publish(context.Context, event.Event, Session) (string, error) {
	return b.reason, nil
}
func (resultBackend) Query(context.Context, []event.Filter, Session) ([]event.Event, error) {
	return nil, nil
}
func (resultBackend) Count(context.Context, []event.Filter, Session) (any, error) { return 0, nil }

type filterBackend struct{ resultBackend }

func (filterBackend) CanReadFilter(event.Event, Session, *event.Filter) bool { return false }

type gatedBackend struct {
	testBackend
	queryStarted chan struct{}
	allowQuery   chan struct{}
	publish      chan struct{}
	syncItems    []syncprotocol.Item
}

func (b *gatedBackend) Query(context.Context, []event.Filter, Session) ([]event.Event, error) {
	close(b.queryStarted)
	<-b.allowQuery
	return nil, nil
}
func (b *gatedBackend) Publish(context.Context, event.Event, Session) (string, error) {
	b.publish <- struct{}{}
	return "", nil
}
func (b *gatedBackend) Sync(context.Context, event.Filter, Session) ([]syncprotocol.Item, error) {
	return b.syncItems, nil
}

func TestPendingQueueRejectsSlowConsumerBeforeUnboundedGrowth(t *testing.T) {
	r := New(testBackend{}, Config{MaxPendingBytes: 4})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	if err := c.enqueue("1234"); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if err := c.enqueue("5"); err == nil {
		t.Fatal("expected slow-consumer rejection")
	}
	c.cancel()
}

func TestPublishDoesNotRebroadcastDuplicate(t *testing.T) {
	r := New(resultBackend{reason: "duplicate: already have this event"}, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.subs["sub"] = subscription{filters: []event.Filter{{Kinds: []int{1}}}}
	r.clientsMu.Lock()
	r.clients[c] = struct{}{}
	r.clientsMu.Unlock()
	_, err := r.Publish(context.Background(), event.Event{ID: strings.Repeat("a", 64), Kind: 1}, Session{})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) != 0 {
		t.Fatalf("duplicate publication was rebroadcast: %d queued messages", len(c.queue))
	}
}

func TestFanoutUsesFilterAwarePrivacyGate(t *testing.T) {
	r := New(filterBackend{}, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.subs["sub"] = subscription{filters: []event.Filter{{Kinds: []int{1}}}}
	r.clientsMu.Lock()
	r.clients[c] = struct{}{}
	r.clientsMu.Unlock()
	r.BroadcastGenerated(event.Event{ID: strings.Repeat("a", 64), Kind: 1})
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) != 0 {
		t.Fatal("filter-aware reader gate was bypassed")
	}
}

func TestPolicyChangeClosesSubscriptionsAndSyncSessions(t *testing.T) {
	r := New(testBackend{}, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.subs["sub"] = subscription{filters: []event.Filter{{Kinds: []int{1}}}}
	c.syncs["sync"] = &syncprotocol.Session{}
	r.clientsMu.Lock()
	r.clients[c] = struct{}{}
	r.clientsMu.Unlock()
	r.CloseSubscriptions("blocked: revoked")
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subs) != 0 || len(c.syncs) != 0 {
		t.Fatal("policy change retained a live subscription or sync session")
	}
	if len(c.queue) != 2 || !strings.Contains(string(c.queue[1].data), `"NEG-CLOSE","sync"`) {
		t.Fatalf("policy change did not close both protocol sessions: %+v", c.queue)
	}
}

func TestApplyACLChangeInvalidatesSyncUnderPublicationFence(t *testing.T) {
	r := New(testBackend{}, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.syncs["sync"] = &syncprotocol.Session{}
	r.clientsMu.Lock()
	r.clients[c] = struct{}{}
	r.clientsMu.Unlock()
	called := false
	if _, err := r.ApplyACLChange(context.Background(), func(context.Context) (any, error) {
		called = true
		return "ok", nil
	}, "blocked: membership revoked"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("ACL mutation callback did not run")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.syncs) != 0 || len(c.queue) != 1 || !strings.Contains(string(c.queue[0].data), `"NEG-CLOSE","sync"`) {
		t.Fatalf("sync session survived ACL mutation: %+v", c.queue)
	}
}

func TestAcceptedMembershipEventInvalidatesSyncBeforeFanout(t *testing.T) {
	r := New(testBackend{}, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.syncs["sync"] = &syncprotocol.Session{}
	c.subs["sub"] = subscription{filters: []event.Filter{{Kinds: []int{event.KIND_LEAVE}}}}
	r.clientsMu.Lock()
	r.clients[c] = struct{}{}
	r.clientsMu.Unlock()
	_, err := r.Publish(context.Background(), event.Event{ID: strings.Repeat("a", 64), Kind: event.KIND_LEAVE}, Session{})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.syncs) != 0 || len(c.subs) != 0 {
		t.Fatal("accepted membership event retained stale transport state")
	}
	if len(c.queue) < 2 || !queueContains(c.queue, `"NEG-CLOSE","sync"`) {
		t.Fatalf("membership revocation did not close sync before fanout: %+v", c.queue)
	}
}

func queueContains(queue []outbound, value string) bool {
	for _, item := range queue {
		if strings.Contains(string(item.data), value) {
			return true
		}
	}
	return false
}

func TestZeroMessageLimitIsUnlimited(t *testing.T) {
	r := New(testBackend{}, Config{MaxMessageBytes: 0})
	if r.cfg.MaxMessageBytes != 0 {
		t.Fatalf("zero message limit became %d", r.cfg.MaxMessageBytes)
	}
	if got := r.cfg.withDefaults().MaxMessageBytes; got != 0 {
		t.Fatalf("zero message limit became %d", got)
	}
}

func TestIdlePingPongKeepsSessionAliveForLaterTraffic(t *testing.T) {
	r := New(testBackend{}, Config{ReadLimit: 20 * time.Millisecond, PingInterval: 5 * time.Millisecond})
	server := httptest.NewServer(http.HandlerFunc(r.HandleHTTP))
	defer server.Close()
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	readCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	texts := make(chan []byte, 8)
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.Read(readCtx)
			if err != nil {
				readErr <- err
				return
			}
			if typ == websocket.MessageText {
				texts <- data
			}
		}
	}()
	select {
	case <-texts:
	case err := <-readErr:
		t.Fatalf("read AUTH: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for AUTH")
	}
	time.Sleep(50 * time.Millisecond)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`["REQ","idle",{}]`)); err != nil {
		t.Fatalf("write REQ after idle: %v", err)
	}
	select {
	case data := <-texts:
		if !strings.Contains(string(data), `"EOSE","idle"`) {
			t.Fatalf("response after idle = %s", data)
		}
	case err := <-readErr:
		t.Fatalf("session closed during healthy idle: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EOSE after idle")
	}
	secret := strings.Repeat("1", 64)
	e := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Content: "after idle"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]any{"EVENT", e})
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write EVENT after idle: %v", err)
	}
	select {
	case data := <-texts:
		if !strings.Contains(string(data), `"OK","`+e.ID+`",true`) {
			t.Fatalf("publish response after idle = %s", data)
		}
	case err := <-readErr:
		t.Fatalf("session closed before publish response: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for publish response after idle")
	}
}

func TestRemoteIPStripsPortWithoutTrustingForwardedHeaders(t *testing.T) {
	if got := remoteIP("192.0.2.10:7447"); got != "192.0.2.10" {
		t.Fatalf("remote IP = %q", got)
	}
	if got := remoteIP("[2001:db8::1]:7447"); got != "2001:db8::1" {
		t.Fatalf("IPv6 remote IP = %q", got)
	}
}

func TestAuthUsesConnectionRelayURL(t *testing.T) {
	r := New(testBackend{}, Config{
		RelayURL: "wss://canonical.example/tenant",
		RequestRelayURL: func(*http.Request) string {
			return "wss://custom.example/tenant"
		},
	})
	c := newClient(r, nil, httptest.NewRequest("GET", "https://custom.example/tenant", nil))
	secret := strings.Repeat("1", 64)
	auth := event.Event{CreatedAt: time.Now().Unix(), Kind: event.KIND_AUTH, Tags: [][]string{{"relay", "wss://custom.example/tenant/"}, {"challenge", c.challenge}}, Content: ""}
	if err := event.Sign(&auth, secret); err != nil {
		t.Fatal(err)
	}
	c.handleAuth([]json.RawMessage{mustRaw(auth)})
	c.mu.Lock()
	if !strings.Contains(string(c.queue[len(c.queue)-1].data), `"OK","`+auth.ID+`",true`) {
		t.Fatalf("custom relay AUTH was rejected: %s", c.queue[len(c.queue)-1].data)
	}
	c.mu.Unlock()

	bad := auth
	bad.Tags = [][]string{{"relay", "wss://other.example"}, {"challenge", c.challenge}}
	if err := event.Sign(&bad, secret); err != nil {
		t.Fatal(err)
	}
	c.handleAuth([]json.RawMessage{mustRaw(bad)})
	c.mu.Lock()
	defer c.mu.Unlock()
	if !strings.Contains(string(c.queue[len(c.queue)-1].data), `"OK","`+bad.ID+`",false`) {
		t.Fatalf("mismatched relay AUTH was accepted: %s", c.queue[len(c.queue)-1].data)
	}
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestNormalizeRelayIgnoresTrailingSlashOnly(t *testing.T) {
	if normalizeRelay("WSS://Relay.Example:8448/path///") != "relay.example" {
		t.Fatal("relay URL normalization should compare the relay host")
	}
}

func TestREQQueryAndPublishShareTheHistoricalLiveFence(t *testing.T) {
	b := &gatedBackend{queryStarted: make(chan struct{}), allowQuery: make(chan struct{}), publish: make(chan struct{}, 1)}
	r := New(b, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	requestDone := make(chan struct{})
	go func() {
		c.handleRequest("REQ", []json.RawMessage{json.RawMessage(`"sub"`), json.RawMessage(`{}`)})
		close(requestDone)
	}()
	<-b.queryStarted
	publishDone := make(chan struct{})
	go func() {
		r.mu.Lock()
		_, _ = r.backend.Publish(context.Background(), event.Event{}, c.snapshot())
		r.mu.Unlock()
		close(publishDone)
	}()
	select {
	case <-b.publish:
		t.Fatal("publish crossed the historical query fence")
	case <-time.After(20 * time.Millisecond):
	}
	close(b.allowQuery)
	<-requestDone
	<-publishDone
	c.cancel()
}

func TestCommitGeneratedSharesHistoricalLiveFence(t *testing.T) {
	b := &gatedBackend{queryStarted: make(chan struct{}), allowQuery: make(chan struct{}), publish: make(chan struct{}, 1)}
	r := New(b, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	queryDone := make(chan struct{})
	go func() {
		c.handleRequest("REQ", []json.RawMessage{json.RawMessage(`"sub"`), json.RawMessage(`{}`)})
		close(queryDone)
	}()
	<-b.queryStarted
	commitDone := make(chan struct{})
	go func() {
		_, _ = r.CommitGenerated(context.Background(), func(context.Context) (event.Event, error) {
			close(commitDone)
			return event.Event{}, nil
		})
	}()
	select {
	case <-commitDone:
		t.Fatal("generated commit crossed the historical query fence")
	case <-time.After(20 * time.Millisecond):
	}
	close(b.allowQuery)
	<-queryDone
	select {
	case <-commitDone:
	case <-time.After(time.Second):
		t.Fatal("generated commit did not proceed after query")
	}
	c.cancel()
}

func TestNegOpenUsesAuthorizedSnapshotAndReportsProtocolResult(t *testing.T) {
	initiator, err := syncprotocol.NewSession([]syncprotocol.Item{{ID: strings.Repeat("a", 64), Timestamp: 1}}, true)
	if err != nil {
		t.Fatal(err)
	}
	message, err := initiator.Start()
	if err != nil {
		t.Fatal(err)
	}
	b := &gatedBackend{syncItems: []syncprotocol.Item{{ID: strings.Repeat("b", 64), Timestamp: 2}}}
	r := New(b, Config{})
	c := newClient(r, nil, httptest.NewRequest("GET", "http://relay.example/", nil))
	c.handleSync("NEG-OPEN", []json.RawMessage{json.RawMessage(`"sync"`), json.RawMessage(`{}`), json.RawMessage(`"` + message + `"`)})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.syncs["sync"] == nil {
		t.Fatal("NEG-OPEN did not retain a session after a non-empty response")
	}
	if len(c.queue) == 0 || !strings.Contains(string(c.queue[len(c.queue)-1].data), `"NEG-MSG"`) {
		t.Fatal("NEG-OPEN did not enqueue NEG-MSG")
	}
}
