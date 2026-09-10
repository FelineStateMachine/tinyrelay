package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scopeTable(t *testing.T, service *Service) {
	t.Helper()
	if _, err := service.store.DB().Exec(`CREATE TABLE attachment_scope (sha256 TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
}

func scopeCommit(fail bool, seen *[]bool) func(context.Context, *sql.Tx, Blob, bool) error {
	return func(ctx context.Context, tx *sql.Tx, entry Blob, created bool) error {
		*seen = append(*seen, created)
		if fail {
			return errors.New("scope rejected")
		}
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO attachment_scope(sha256) VALUES(?)", entry.SHA256)
		return err
	}
}

func TestScopedCommitNewIsAtomic(t *testing.T) {
	service := testService(t)
	scopeTable(t, service)
	body := []byte("atomic scoped upload")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	var seen []bool
	_, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: testUploader, Commit: scopeCommit(true, &seen)})
	if err == nil || !strings.Contains(err.Error(), "scope rejected") {
		t.Fatalf("failed scope commit error = %v", err)
	}
	if len(seen) != 1 || !seen[0] {
		t.Fatalf("commit states = %v, want [true]", seen)
	}
	var count int
	if err := service.store.DB().QueryRow("SELECT COUNT(*) FROM blobs WHERE sha256=?", hash).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back blob metadata count = %d", count)
	}
	if _, err := os.Stat(filepath.Join(service.root, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled back final file stat = %v", err)
	}
}

func TestScopedCommitDedupAndRecovery(t *testing.T) {
	service := testService(t)
	scopeTable(t, service)
	body := []byte("deduplicated scoped upload")
	var first []bool
	entry, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: testUploader, Commit: scopeCommit(false, &first)})
	if err != nil || len(first) != 1 || !first[0] {
		t.Fatalf("initial scoped upload entry=%+v states=%v err=%v", entry, first, err)
	}
	other := strings.Repeat("b", 64)
	var rejected []bool
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: other, Commit: scopeCommit(true, &rejected)}); err == nil {
		t.Fatal("rejected deduplicated scope commit succeeded")
	}
	var claims int
	if err := service.store.DB().QueryRow("SELECT COUNT(*) FROM blob_claims WHERE sha256=? AND uploader=?", entry.SHA256, other).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("rolled back deduplicated claim count = %d", claims)
	}
	var accepted []bool
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: other, Commit: scopeCommit(false, &accepted)}); err != nil || len(accepted) != 1 || accepted[0] {
		t.Fatalf("deduplicated scoped upload states=%v err=%v", accepted, err)
	}
	if err := os.Remove(filepath.Join(service.root, entry.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Put(context.Background(), PutOptions{Reader: strings.NewReader(string(body)), Type: "text/plain", Uploader: testUploader}); err != nil {
		t.Fatalf("recovery upload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.root, entry.SHA256)); err != nil {
		t.Fatalf("recovered final file: %v", err)
	}
}

func TestReconcileStagedBlobRequiresCommittedMetadata(t *testing.T) {
	service := testService(t)
	body := []byte("staged durable bytes")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	stage := filepath.Join(service.root, ".scoped-"+hash)
	if err := os.WriteFile(stage, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(context.Background(), service.store, service.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted stage stat = %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.root, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted final stat = %v", err)
	}
	if err := os.WriteFile(stage, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.DB().Exec("INSERT INTO blobs(sha256,size,type,uploader,uploaded) VALUES(?,?,?,?,?)", hash, len(body), "text/plain", "", 1); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(context.Background(), service.store, service.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(service.root, hash)); err != nil {
		t.Fatalf("committed final stat = %v", err)
	}
}
