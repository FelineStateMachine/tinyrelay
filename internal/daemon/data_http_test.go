package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestParseJSONLEventsValidatesEveryLine(t *testing.T) {
	good := map[string]any{"id": strings.Repeat("a", 64), "pubkey": strings.Repeat("b", 64), "created_at": 1, "kind": 1, "tags": [][]string{}, "content": "", "sig": strings.Repeat("c", 128)}
	raw, _ := json.Marshal(good)
	if _, err := parseJSONLEvents(append(raw, '\n')); err == nil {
		t.Fatal("unsigned event accepted")
	}
	if _, err := parseJSONLEvents([]byte("{}\n{")); err == nil {
		t.Fatal("malformed later line accepted")
	}
}

func TestDataHTTPRequiresSignedOwnerForMutations(t *testing.T) {
	_, tenant := testTenant(t)
	body := []byte("{}\n")
	req := httptest.NewRequest(http.MethodPut, "http://relay.test/import", bytes.NewReader(body))
	res := httptest.NewRecorder()
	tenant.tryDataHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("cookie-less import status = %d, body=%s", res.Code, res.Body.String())
	}
}

func TestBackupPreviewSignedAndRestoreRequiresFreshMatchingOwner(t *testing.T) {
	_, tenant := testTenant(t)
	archive, err := replication.CreateBackupWithProvider(context.Background(), tenant.store, tenant.ReplicationBackupProvider(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/backups/preview", bytes.NewReader(body))
	signRequest(t, req, string(body))
	res := httptest.NewRecorder()
	tenant.tryDataHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", res.Code, res.Body.String())
	}
	item := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Tags: [][]string{}, Content: "already here"}
	if err := event.Sign(&item, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(context.Background(), item, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "http://relay.test/backups/restore", bytes.NewReader(body))
	signRequest(t, req, string(body))
	res = httptest.NewRecorder()
	tenant.tryDataHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("non-fresh restore status = %d, body=%s", res.Code, res.Body.String())
	}
}

func TestWithinPathRejectsEscape(t *testing.T) {
	if withinPath("/srv/relay", "/srv/relay/backup.json") == false {
		t.Fatal("in-root path rejected")
	}
	if withinPath("/srv/relay", "/srv/relay-other/backup.json") {
		t.Fatal("prefix escape accepted")
	}
}

func TestNativeBackupHTTPRoundTripPreservesPrivateStateAndFiles(t *testing.T) {
	ctx := context.Background()
	_, source := testTenant(t)
	app, target := testTenant(t)
	now := time.Now().Unix()
	item := event.Event{Kind: 1, CreatedAt: now, Tags: [][]string{}, Content: "hidden source history"}
	if err := event.Sign(&item, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.Save(ctx, item, storage.SaveOptions{Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.DB().ExecContext(ctx, `INSERT INTO hidden_events(id,reason) VALUES(?,?)`, item.ID, "test"); err != nil {
		t.Fatal(err)
	}
	entry, err := source.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("portable file"), Type: "text/plain", Uploader: source.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := replication.CreateBackupWithProvider(ctx, source.store, source.ReplicationBackupProvider(), now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://relay.test/r/main/backups/restore", bytes.NewReader(body))
	signRequest(t, req, string(body))
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("restore %d: %s", res.Code, res.Body.String())
	}
	var raw string
	if err := target.store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE id=?`, item.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var hidden int
	if err := target.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM hidden_events WHERE id=?`, item.ID).Scan(&hidden); err != nil || hidden != 1 {
		t.Fatalf("hidden state %d: %v", hidden, err)
	}
	if target.records.PublicKey() != source.records.PublicKey() {
		t.Fatal("native relay signing identity was not restored")
	}
	restored, reader, err := target.blobs.Get(ctx, entry.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := readUnbounded(reader)
	_ = reader.Close()
	if err != nil || string(contents) != "portable file" || restored.Type != entry.Type || restored.Uploader != entry.Uploader {
		t.Fatalf("restored blob %#v %q: %v", restored, contents, err)
	}
	// Owner-only downloads must remain scoped to the same tenant and uncacheable.
	name := "migration.backup.json"
	if err := os.WriteFile(filepath.Join(target.meta.Paths.Root, name), body, 0600); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://relay.test/r/main/backups/"+name, nil)
	signRequest(t, req, "")
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), body) || res.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("download %d: %s", res.Code, res.Body.String())
	}
}

func TestRestoreFreshnessIncludesInvisibleEventsAndUnindexedFiles(t *testing.T) {
	for _, fixture := range []string{"hidden", "pending", "expired", "blob", "git"} {
		t.Run(fixture, func(t *testing.T) {
			_, tenant := testTenant(t)
			ctx := context.Background()
			if fresh, err := tenant.restoreTargetFresh(ctx); err != nil || !fresh {
				t.Fatalf("new relay fresh=%v err=%v", fresh, err)
			}
			if fixture == "blob" || fixture == "git" {
				root := filepath.Join(tenant.meta.Paths.Root, "blobs")
				if fixture == "git" {
					root = tenant.meta.Paths.Git
				}
				if err := os.WriteFile(filepath.Join(root, "existing"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				e := event.Event{Kind: 1, CreatedAt: time.Now().Unix(), Tags: [][]string{}, Content: fixture}
				if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
					t.Fatal(err)
				}
				if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
					t.Fatal(err)
				}
				switch fixture {
				case "hidden":
					_, _ = tenant.store.DB().ExecContext(ctx, `INSERT INTO hidden_events(id,reason) VALUES(?,'test')`, e.ID)
				case "pending":
					_, _ = tenant.store.DB().ExecContext(ctx, `INSERT INTO pending_events(id,reason) VALUES(?,'git')`, e.ID)
				case "expired":
					_, _ = tenant.store.DB().ExecContext(ctx, `UPDATE events SET expires=1 WHERE id=?`, e.ID)
				}
			}
			if fresh, err := tenant.restoreTargetFresh(ctx); err != nil || fresh {
				t.Fatalf("used relay fresh=%v err=%v", fresh, err)
			}
		})
	}
}

func TestRestoreFreshnessOnlyExemptsRelayBootstrap30078(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	if fresh, err := tenant.restoreTargetFresh(ctx); err != nil || !fresh {
		t.Fatalf("new relay fresh=%v err=%v", fresh, err)
	}
	// Permanent ownership emits a relay-authored 30078 before the restore form
	// is available. That bootstrap row is the only 30078 row the freshness
	// check may ignore.
	if _, err := tenant.store.DB().ExecContext(ctx, `INSERT INTO events(id,pubkey,created_at,kind,d,raw) VALUES(?,?,?,?,?,?)`,
		"bootstrap-30078", tenant.records.PublicKey(), time.Now().Unix(), 30078, "", `{"id":"bootstrap-30078","pubkey":"relay","kind":30078}`); err != nil {
		t.Fatal(err)
	}
	if fresh, err := tenant.restoreTargetFresh(ctx); err != nil || !fresh {
		t.Fatalf("relay bootstrap made target stale: fresh=%v err=%v", fresh, err)
	}
	if _, err := tenant.store.DB().ExecContext(ctx, `INSERT INTO events(id,pubkey,created_at,kind,d,raw) VALUES(?,?,?,?,?,?)`,
		"user-30078", tenant.Policy().Owner, time.Now().Unix(), 30078, "", `{"id":"user-30078","pubkey":"owner","kind":30078}`); err != nil {
		t.Fatal(err)
	}
	if fresh, err := tenant.restoreTargetFresh(ctx); err != nil || fresh {
		t.Fatalf("user 30078 was incorrectly exempted: fresh=%v err=%v", fresh, err)
	}
}
