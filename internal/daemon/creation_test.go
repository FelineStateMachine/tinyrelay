package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

const creationOwner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAfterCreateSeedsRecurringSourceBeforeReady(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Catalog().Create(ctx, catalog.CreateOptions{Name: "search", Owner: creationOwner, Template: "search", Source: "wss://source.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AfterCreate(ctx, meta, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var state, payload string
	store, err := openCreationStore(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.DB().QueryRow("SELECT state,payload FROM replication_jobs WHERE id=?", "source-"+meta.ID).Scan(&state, &payload); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || payload == "" {
		t.Fatalf("source job = %q %q", state, payload)
	}
	if err := app.Catalog().MarkReady(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAfterCreateRequiresSearchSourceAndRecoveryLeavesItCreating(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Catalog().Create(ctx, catalog.CreateOptions{Name: "search", Owner: creationOwner, Template: "search"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RecoverCreating(ctx); err == nil {
		t.Fatal("recovery accepted missing required source")
	}
	got, err := app.Catalog().GetByID(ctx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusCreating {
		t.Fatalf("status = %q, want creating", got.Status)
	}
}

func TestInboxSourceFollowsOwnerWithHourlyPull(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Catalog().Create(ctx, catalog.CreateOptions{Name: "inbox", Owner: creationOwner, Template: "inbox", Source: "https://slate.example/relay"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AfterCreate(ctx, meta, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store, err := openCreationStore(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var payload string
	if err := store.DB().QueryRow("SELECT payload FROM replication_jobs WHERE id=?", "source-"+meta.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"every":1`) || !strings.Contains(payload, "#p") || !strings.Contains(payload, creationOwner) {
		t.Fatalf("inbox source job payload = %s", payload)
	}
	if strings.Contains(payload, "https://") || !strings.Contains(payload, "wss://slate.example/relay") {
		t.Fatalf("inbox source URL was not converted: %s", payload)
	}
}

func TestInboxWithoutSourceSeedsDormantOwnerReadRelayDiscovery(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Catalog().Create(ctx, catalog.CreateOptions{Name: "inbox", Owner: creationOwner, Template: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AfterCreate(ctx, meta, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store, err := openCreationStore(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var payload string
	if err := store.DB().QueryRow("SELECT payload FROM replication_jobs WHERE id=?", "source-"+meta.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"discoverPubKey":"`+creationOwner+`"`) || !strings.Contains(payload, `"every":1`) || strings.Contains(payload, `"relays"`) {
		t.Fatalf("inbox discovery job payload = %s", payload)
	}
}

func TestInboxWithoutSourceJobUsesRuntimeDiscoverySeeds(t *testing.T) {
	ctx := context.Background()
	app, err := New(ctx, Config{DataDir: t.TempDir(), DefaultTenant: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	meta, err := app.Catalog().Create(ctx, catalog.CreateOptions{Name: "inbox", Owner: creationOwner, Template: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AfterCreate(ctx, meta, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	store, err := openCreationStore(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state, payload string
	if err := store.DB().QueryRow("SELECT state,payload FROM replication_jobs WHERE id=?", "source-"+meta.ID).Scan(&state, &payload); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || !strings.Contains(payload, `"discoverPubKey":"`+creationOwner+`"`) {
		t.Fatalf("runtime discovery job = %q %s", state, payload)
	}
}

func openCreationStore(ctx context.Context, meta catalog.Tenant) (*storage.Store, error) {
	return storage.Open(ctx, meta.Paths.Database)
}
