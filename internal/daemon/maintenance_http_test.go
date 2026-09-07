package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
)

type waitingRequestBody struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *waitingRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (b *waitingRequestBody) Close() error { return nil }

func TestBackupDrainsActualHTTPPublishAndResumes(t *testing.T) {
	_, tenant := testTenant(t)
	e := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Content: "publish at snapshot boundary"}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	body := &waitingRequestBody{reader: strings.NewReader(string(raw)), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/events", body)
	signRequest(t, req, string(raw))
	res := httptest.NewRecorder()
	published := make(chan struct{})
	go func() { tenant.ServeHTTP(res, req); close(published) }()
	<-body.entered
	type snapshotResult struct {
		archive replication.BackupArchive
		err     error
	}
	snapshot := make(chan snapshotResult, 1)
	go func() {
		archive, err := replication.CreateBackupWithProvider(context.Background(), tenant.store, tenant.ReplicationBackupProvider(), time.Now().Unix())
		snapshot <- snapshotResult{archive, err}
	}()
	deadline := time.After(time.Second)
	for {
		tenant.maintenance.mu.Lock()
		exclusive := tenant.maintenance.maintenance
		tenant.maintenance.mu.Unlock()
		if exclusive {
			break
		}
		select {
		case <-deadline:
			close(body.release)
			t.Fatal("snapshot did not request maintenance")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-snapshot:
		close(body.release)
		t.Fatal("snapshot passed in-flight upload")
	default:
	}
	if _, err := tenant.Query(context.Background(), []event.Filter{{}}, relay.Session{}); !errors.Is(err, ErrMaintenance) {
		close(body.release)
		t.Fatalf("new query during snapshot: %v", err)
	}
	close(body.release)
	<-published
	if res.Code != http.StatusOK {
		t.Fatalf("in-flight publish %d: %s", res.Code, res.Body.String())
	}
	result := <-snapshot
	if result.err != nil {
		t.Fatal(result.err)
	}
	found := false
	for _, item := range result.archive.Events {
		if item.ID == e.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("snapshot lost the in-flight committed event")
	}
	if _, err := tenant.Query(context.Background(), []event.Filter{{IDs: []string{e.ID}}}, relay.Session{}); err != nil {
		t.Fatalf("query after snapshot: %v", err)
	}
}

func TestShutdownWaitsForExclusiveMaintenance(t *testing.T) {
	var gate maintenanceGate
	_, release, err := gate.beginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gate.markClosing()
	idle := make(chan error, 1)
	go func() { idle <- gate.waitIdle(context.Background()) }()
	select {
	case err := <-idle:
		t.Fatalf("shutdown passed active maintenance: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-idle:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not resume")
	}
}
