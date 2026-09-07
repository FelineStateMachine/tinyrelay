package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/coder/websocket"
)

type acceptanceRelay struct {
	mu       sync.Mutex
	seen     []event.Event
	failNext bool
}

func (r *acceptanceRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	_, data, err := conn.Read(req.Context())
	if err != nil {
		return
	}
	var message []json.RawMessage
	if json.Unmarshal(data, &message) != nil || len(message) < 2 {
		return
	}
	var kind string
	_ = json.Unmarshal(message[0], &kind)
	if kind != "EVENT" {
		return
	}
	var item event.Event
	if len(message) < 2 || json.Unmarshal(message[1], &item) != nil {
		return
	}
	r.mu.Lock()
	r.seen = append(r.seen, item)
	reject := r.failNext
	r.failNext = false
	r.mu.Unlock()
	accepted := !reject
	reason := ""
	if reject {
		reason = "temporary upstream failure"
	}
	response, _ := json.Marshal([]any{"OK", item.ID, accepted, reason})
	_ = conn.Write(req.Context(), websocket.MessageText, response)
}

func TestDaemonDeliveryPersistsAcrossRestartAndExcludesImports(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := &acceptanceRelay{failNext: true}
	remoteServer := httptest.NewServer(remote)
	t.Cleanup(remoteServer.Close)
	remoteTarget := "ws" + strings.TrimPrefix(remoteServer.URL, "http")

	app, err := New(ctx, Config{DataDir: root, DefaultTenant: "main", AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	ownerSecret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "outbox"})
	if err != nil {
		t.Fatal(err)
	}
	sourceServer := httptest.NewServer(app)
	t.Cleanup(sourceServer.Close)
	tenant, err := app.tenant(ctx, meta, sourceServer.URL+"/r/main")
	if err != nil {
		t.Fatal(err)
	}
	if tenant == nil {
		t.Fatal("created tenant was not loaded")
	}
	// The real daemon websocket endpoint is used for source admission; its
	// durable worker then delivers through a second real websocket server.
	sourceTarget := "ws" + strings.TrimPrefix(sourceServer.URL, "http") + "/r/main"

	announcement := event.Event{Kind: 10002, CreatedAt: time.Now().Unix(), Tags: [][]string{{"r", remoteTarget}}, Content: ""}
	if err := event.Sign(&announcement, ownerSecret); err != nil {
		t.Fatal(err)
	}
	transport := &replication.NostrTransport{Dialer: replication.WebsocketDialer{AllowPrivate: true}}
	if _, err := tenant.store.Save(ctx, announcement, storage.SaveOptions{Now: announcement.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
		t.Fatalf("store relay directory announcement: %v", err)
	}

	eventToDeliver := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Tags: [][]string{}, Content: "durable outbox"}
	if err := event.Sign(&eventToDeliver, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if result, err := transport.Send(ctx, sourceTarget, eventToDeliver); err != nil || !result.Accepted {
		t.Fatalf("source relay rejected publish: %+v %v", result, err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		pending, pendingErr := tenant.replication.Queue().Pending(ctx, "delivery")
		return pendingErr == nil && len(pending) > 0 && pending[0].State == "pending" && pending[0].Attempts > 0 && remoteCount(remote) >= 1
	}) {
		t.Fatalf("failed delivery was not durable; remote attempts=%d", remoteCount(remote))
	}
	items, err := tenant.replication.Queue().Pending(ctx, "delivery")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("failed delivery was not durable: %d pending intents", len(items))
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	app, err = New(ctx, Config{DataDir: root, DefaultTenant: "main", AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(ctx) })
	if err := app.Start(ctx, sourceServer.URL); err != nil {
		t.Fatal(err)
	}
	reloaded, err := app.tenant(ctx, meta, sourceServer.URL+"/r/main")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil {
		t.Fatal("tenant did not reopen")
	}
	if !waitFor(t, 8*time.Second, func() bool {
		pending, pendingErr := reloaded.replication.Queue().Pending(ctx, "delivery")
		return pendingErr == nil && len(pending) == 0 && remoteCount(remote) >= 3
	}) {
		t.Fatalf("restart retry did not complete: remote=%d", remoteCount(remote))
	}
	if got := remoteCount(remote); got != 3 {
		t.Fatalf("restart delivery count = %d, want failed list plus accepted list and event", got)
	}

	for _, kind := range []int{1, event.KIND_WRAP} {
		imported := event.Event{Kind: kind, CreatedAt: time.Now().Unix(), Tags: [][]string{}, Content: "imported"}
		if kind == event.KIND_WRAP {
			imported.Tags = [][]string{{"p", owner}}
		}
		if err := event.Sign(&imported, ownerSecret); err != nil {
			t.Fatal(err)
		}
		if err := reloaded.ingest(ctx, imported, replication.OriginImport); err != nil {
			t.Fatalf("import kind %d: %v", kind, err)
		}
	}
	items, err = reloaded.replication.Queue().Pending(ctx, "delivery")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || remoteCount(remote) != 3 {
		t.Fatalf("imported events echoed to outbox: pending=%d remote=%d", len(items), remoteCount(remote))
	}
}

func remoteCount(remote *acceptanceRelay) int {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	return len(remote.seen)
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}
