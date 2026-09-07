package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/coder/websocket"
)

// TestDaemonRestartResumesUnconsumedJobAndViewCheckpoint exercises the daemon
// boundary: a tenant is stopped with durable work still pending, then opened
// by a new App. The job worker and view scheduler must consume the old rows.
func TestDaemonRestartResumesUnconsumedJobAndViewCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ownerSecret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	tenant := app.tenants[meta.ID]
	if tenant == nil {
		t.Fatal("tenant was not started")
	}
	// Stop only the tenant workers. This lets us enqueue work at a known
	// unconsumed point while still using the production queue and service.
	tenant.workCancel()
	tenant.workWG.Wait()
	e := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Content: "restart durable"}
	if err := event.Sign(&e, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.replication.AddJob(ctx, replication.JobSpec{ID: "restart-dump", Kind: replication.JobDump, Relays: []string{"restart.jsonl"}}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.records.MarkView(ctx, "articles", time.Now().Unix()-9); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	app, err = New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	reloaded := app.tenants[meta.ID]
	if reloaded == nil {
		t.Fatal("tenant was not recovered")
	}
	if !waitFor(t, 8*time.Second, func() bool {
		statuses, statusErr := reloaded.replication.ListJobStatus(ctx)
		return statusErr == nil && len(statuses) == 1 && statuses[0].Phase == "complete"
	}) {
		statuses, _ := reloaded.replication.ListJobStatus(ctx)
		t.Fatalf("restart job did not resume: %#v", statuses)
	}
	if _, err := os.Stat(filepath.Join(meta.Paths.Root, "restart.jsonl")); err != nil {
		t.Fatalf("restart artifact missing: %v", err)
	}
	var checkpoint int64
	if err := reloaded.store.GetSetting(ctx, "records.view.articles.dirty", &checkpoint); err != nil || checkpoint == 0 {
		t.Fatalf("unconsumed view checkpoint was lost across restart: %d %v", checkpoint, err)
	}
}

// TestDaemonRestartDrainsDurableNotificationIntent verifies that generated
// notification work is persisted before shutdown and handled after recovery.
func TestDaemonRestartDrainsDurableNotificationIntent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secret := strings.Repeat("0", 63) + "1"
	owner, err := event.PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received int
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var message []json.RawMessage
		if json.Unmarshal(raw, &message) == nil && len(message) > 1 {
			var kind string
			_ = json.Unmarshal(message[0], &kind)
			if kind == "EVENT" {
				var item event.Event
				if json.Unmarshal(message[1], &item) == nil {
					mu.Lock()
					received++
					mu.Unlock()
					response, _ := json.Marshal([]any{"OK", item.ID, true, ""})
					_ = conn.Write(r.Context(), websocket.MessageText, response)
				}
			}
		}
	}))
	defer remote.Close()
	remoteURL := "ws" + strings.TrimPrefix(remote.URL, "http")
	app, err := New(ctx, Config{DataDir: dir, DefaultTenant: "main", AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	tenant := app.tenants[meta.ID]
	tenant.workCancel()
	tenant.workWG.Wait()
	inbox := event.Event{Kind: 10050, CreatedAt: time.Now().Unix(), Tags: [][]string{{"relay", remoteURL}}}
	if err := event.Sign(&inbox, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, inbox, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	wrap := event.Event{Kind: event.KIND_WRAP, CreatedAt: time.Now().Unix(), Tags: [][]string{{"p", owner}}, Content: "cipher"}
	if err := event.Sign(&wrap, secret); err != nil {
		t.Fatal(err)
	}
	if err := tenant.deliverNotification(ctx, wrap, owner); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	app, err = New(ctx, Config{DataDir: dir, DefaultTenant: "main", AllowPrivateRelays: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if err := app.Start(ctx, "http://relay.test"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 8*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return received == 1 }) {
		t.Fatal("durable notification intent was not delivered after restart")
	}
}
