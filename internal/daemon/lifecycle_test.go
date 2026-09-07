package daemon

import (
	"context"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"strings"
	"testing"
	"time"
)

func TestServingLifecycleRecoversAndReconcilesCatalog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app, err := New(ctx, Config{DataDir: dir, DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := event.PublicKey(strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	main, err := app.Create(ctx, CreateOptions{Name: "main", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.tenants) != 0 {
		t.Fatal("catalog-only create started tenant workers")
	}
	if err := app.Start(ctx, "http://127.0.0.1:7447"); err != nil {
		t.Fatal(err)
	}
	if app.tenants[main.ID].publicURL != "http://127.0.0.1:7447" {
		t.Fatal("default relay URL lost its listener port")
	}
	other, err := app.Create(ctx, CreateOptions{Name: "other", Owner: owner, Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if app.tenants[other.ID].publicURL != "http://127.0.0.1:7447/r/other" {
		t.Fatal("new relay was not started with its tenant path")
	}
	second, err := New(ctx, Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	if err := second.Start(ctx, "http://127.0.0.1:7448"); err == nil {
		t.Fatal("second serving process acquired the directory")
	}
	old := app.tenants[other.ID]
	if err := app.Catalog().Disable(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileTenants(ctx, "http://127.0.0.1:7447"); err != nil {
		t.Fatal(err)
	}
	if old.workCtx.Err() == nil {
		t.Fatal("disabled tenant workers still run")
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(ctx, "http://127.0.0.1:7448"); err != nil {
		t.Fatal(err)
	}
	if second.tenants[main.ID] == nil || second.tenants[other.ID] != nil {
		t.Fatal("restart did not eagerly recover exactly the ready tenants")
	}
	if err := second.Catalog().Enable(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		second.mu.Lock()
		loaded := second.tenants[other.ID] != nil
		second.mu.Unlock()
		if loaded {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enabled tenant did not restart through catalog reconciliation")
}

func TestProcessLockExcludesSecondServer(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireProcessLock(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second serving process acquired the lock")
	}
	if second != nil {
		t.Fatal("failed lock returned a handle")
	}
}

func TestProcessLockCanBeReacquiredAfterClose(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
