package catalog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteRequiresDisabledAndRemovesTenantData(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	meta, err := cat.Create(ctx, CreateOptions{Name: "erase-me", Owner: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Delete(ctx, meta.ID); err != ErrInvalidTransition {
		t.Fatalf("delete while creating = %v, want invalid transition", err)
	}
	if err := cat.MarkReady(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.Disable(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.Delete(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(meta.Paths.Root); !os.IsNotExist(err) {
		t.Fatalf("tenant root still exists: %v", err)
	}
	items, err := cat.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == meta.ID {
			t.Fatalf("deleted tenant remains in catalog")
		}
	}
}

func TestDeleteAtomicallyRemovesHostMappings(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	meta, err := cat.Create(ctx, CreateOptions{Name: "host-delete", Owner: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkReady(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.Disable(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.AddHost(ctx, meta.ID, "delete.example", ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.Delete(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ResolveHostMapping(ctx, "delete.example"); err != ErrHostMappingNotFound {
		t.Fatalf("host mapping after delete = %v", err)
	}
}

func TestOpenRecoversInterruptedDeletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cat, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := cat.Create(ctx, CreateOptions{Name: "recover-delete", Owner: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkReady(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.Disable(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(filepath.Dir(meta.Paths.Root), ".deleting-"+meta.ID)
	if err := cat.dbTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE tenants SET status=?,updated_at=? WHERE id=?", StatusDeleting, time.Now().UnixNano(), meta.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO tenant_deletions(id,root,staging,started_at) VALUES(?,?,?,?)", meta.ID, meta.Paths.Root, staging, time.Now().UnixNano())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(meta.Paths.Root, staging); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetByID(ctx, meta.ID); err != ErrNotFound {
		t.Fatalf("recovered deleted tenant = %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

func TestOpenMigratesLegacyTenantStatusConstraint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tenants (id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE, owner TEXT NOT NULL, template TEXT NOT NULL, status TEXT NOT NULL, host TEXT COLLATE NOCASE UNIQUE, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, CHECK(status IN ('creating','ready','disabled'))); INSERT INTO tenants(id,name,owner,template,status,created_at,updated_at) VALUES('legacy','legacy','0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef','default','disabled',1,1); CREATE TABLE tenant_hosts (tenant_id TEXT NOT NULL, host TEXT PRIMARY KEY COLLATE NOCASE, site TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'configured', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.beginDeletion(ctx, "legacy", cat.tenantPaths("legacy").Root, filepath.Join(root, "tenants", ".deleting-legacy")); err != nil {
		t.Fatalf("legacy deleting status was not migrated: %v", err)
	}
}
