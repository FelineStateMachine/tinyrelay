package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testOwner = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCreateAndLifecycle(t *testing.T) {
	cat := openTestCatalog(t)
	tenant, err := cat.Create(context.Background(), CreateOptions{Name: "Example", Owner: testOwner, Template: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Status != StatusCreating || tenant.Owner != testOwner {
		t.Fatalf("unexpected tenant: %+v", tenant)
	}
	if tenant.Paths.Database != filepath.Join(tenant.Paths.Root, "relay.db") {
		t.Fatalf("database path is not stable: %+v", tenant.Paths)
	}
	if tenant.Paths.Blobs == tenant.Paths.Git || !strings.HasSuffix(tenant.Paths.Blobs, "blobs") {
		t.Fatalf("unexpected tenant paths: %+v", tenant.Paths)
	}
	if err := cat.MarkReady(context.Background(), tenant.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := cat.GetByID(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != StatusReady {
		t.Fatalf("status = %q, want ready", ready.Status)
	}
	if err := cat.Disable(context.Background(), tenant.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.Enable(context.Background(), tenant.ID); err != nil {
		t.Fatal(err)
	}
	newOwner := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	if err := cat.TransferOwner(context.Background(), tenant.ID, testOwner, newOwner); err != nil {
		t.Fatal(err)
	}
	updated, err := cat.GetByID(context.Background(), tenant.ID)
	if err != nil || updated.Owner != newOwner {
		t.Fatalf("owner after transfer = %q, err=%v", updated.Owner, err)
	}
}

func TestSQLiteDurabilityPragmas(t *testing.T) {
	cat := openTestCatalog(t)
	var synchronous string
	if err := cat.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != "2" {
		t.Fatalf("synchronous = %q, want FULL (2)", synchronous)
	}
}

func TestCreateRejectsInvalidOwnerAndUnsafeName(t *testing.T) {
	cat := openTestCatalog(t)
	for _, tc := range []struct {
		name  string
		owner string
	}{
		{"ok", "bad"},
		{"../escape", testOwner},
		{"", testOwner},
	} {
		_, err := cat.Create(context.Background(), CreateOptions{Name: tc.name, Owner: tc.owner})
		if err == nil {
			t.Errorf("Create(%q, %q) succeeded", tc.name, tc.owner)
		}
	}
}

func TestNameAndHostUniquenessAndNormalization(t *testing.T) {
	cat := openTestCatalog(t)
	one, err := cat.Create(context.Background(), CreateOptions{Name: "First", Owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Create(context.Background(), CreateOptions{Name: " first ", Owner: testOwner}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name error = %v, want ErrNameTaken", err)
	}
	if err := cat.SetHost(context.Background(), one.ID, "Relay.Example.COM."); err != nil {
		t.Fatal(err)
	}
	resolved, err := cat.ResolveHost(context.Background(), "relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != one.ID {
		t.Fatalf("resolved %q, want %q", resolved.ID, one.ID)
	}
	two, err := cat.Create(context.Background(), CreateOptions{Name: "Second", Owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetHost(context.Background(), two.ID, "relay.example.com"); !errors.Is(err, ErrHostTaken) {
		t.Fatalf("duplicate host error = %v, want ErrHostTaken", err)
	}
	if err := cat.ClearHost(context.Background(), one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ResolveHost(context.Background(), "relay.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared host error = %v, want ErrNotFound", err)
	}
}

func TestHostMappingsResolveAndRemainUnique(t *testing.T) {
	cat := openTestCatalog(t)
	one, err := cat.Create(context.Background(), CreateOptions{Name: "First", Owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	two, err := cat.Create(context.Background(), CreateOptions{Name: "Second", Owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := cat.AddHost(context.Background(), one.ID, "Relay.Alice.Example.", "site-label")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.Host != "relay.alice.example" || mapping.Site != "site-label" {
		t.Fatalf("mapping = %+v", mapping)
	}
	resolved, err := cat.ResolveHost(context.Background(), "relay.alice.example")
	if err != nil || resolved.ID != one.ID {
		t.Fatalf("resolved = %+v, err=%v", resolved, err)
	}
	if _, err := cat.AddHost(context.Background(), two.ID, "relay.alice.example", ""); !errors.Is(err, ErrHostTaken) {
		t.Fatalf("duplicate alias error = %v", err)
	}
	if err := cat.SetHost(context.Background(), two.ID, "relay.alice.example"); !errors.Is(err, ErrHostTaken) {
		t.Fatalf("primary/alias collision = %v", err)
	}
	updated, err := cat.SetHostSite(context.Background(), one.ID, "relay.alice.example", "other")
	if err != nil || updated.Site != "other" {
		t.Fatalf("updated = %+v, err=%v", updated, err)
	}
	if err := cat.RemoveHost(context.Background(), one.ID, "relay.alice.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ResolveHostMapping(context.Background(), "relay.alice.example"); !errors.Is(err, ErrHostMappingNotFound) {
		t.Fatalf("removed alias error = %v", err)
	}
}

func TestConcurrentCreatesDoNotDuplicateNames(t *testing.T) {
	cat := openTestCatalog(t)
	const workers = 12
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cat.Create(context.Background(), CreateOptions{Name: "same", Owner: testOwner})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrNameTaken) {
			t.Errorf("unexpected concurrent create error: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d tenants, want 1", created)
	}
}

func TestRestartKeepsCreatingTenant(t *testing.T) {
	root := t.TempDir()
	one, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := one.Create(context.Background(), CreateOptions{Name: "recover", Owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := one.Close(); err != nil {
		t.Fatal(err)
	}
	two, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := two.GetByID(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusCreating {
		t.Fatalf("status after restart = %q", recovered.Status)
	}
}

func openTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}
