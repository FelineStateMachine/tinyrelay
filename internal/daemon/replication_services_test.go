package daemon

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/replication"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestReplicationProviderSnapshotsSQLiteTablesWithoutWALCopy(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`CREATE TABLE provider_fixture (id TEXT PRIMARY KEY, body TEXT NOT NULL); INSERT INTO provider_fixture VALUES ('one','value')`); err != nil {
		t.Fatal(err)
	}
	tables, err := snapshotTables(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, table := range tables {
		if table.Name == "provider_fixture" && len(table.Rows) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture missing from snapshot: %#v", tables)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "blob-a"), []byte("blob"), 0600); err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{store: store, publicURL: "https://relay.example", meta: catalog.Tenant{Paths: catalog.Paths{Root: root, Git: filepath.Join(root, "git")}}}
	provider := tenantReplicationProvider{tenant: tenant}
	state, err := provider.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blobs) != 1 {
		t.Fatalf("blobs = %#v", state.Blobs)
	}
}

func TestLegacyBindWSBackupRestoresGitCloneAndState(t *testing.T) {
	ctx := context.Background()
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	secret := strings.Repeat("0", 63) + "1"
	e := event.Event{CreatedAt: 20, Kind: 1, Tags: [][]string{}, Content: "legacy event"}
	if err := event.Sign(&e, secret); err != nil {
		t.Fatal(err)
	}
	gitBackup := makeLegacyGitBackup(t, owner)
	pendingUntil := time.Now().Unix() + 3600
	blob := []byte("legacy blob")
	blobSum := sha256Hex(blob)
	fixture := map[string]any{
		"format":   "bind.ws/relay-backup/1",
		"manifest": map[string]any{"createdAt": 20, "owner": owner, "relayIdentity": owner, "slug": "legacy", "archiveSha256": ""},
		"config": map[string]any{
			"format": "bind.ws/relay-config/2", "policy": map[string]any{"features": map[string]any{"search": "prose"}},
			"members": []any{}, "bans": []any{}, "addresses": []any{}, "banned_events": []any{},
			"kinds": map[string]any{"allow": []int{}, "block": []int{}}, "retention": []any{}, "connections": []any{}, "jobs": []any{},
		},
		"events": []string{mustJSON(t, e)},
		"blobs":  []any{map[string]any{"sha256": blobSum, "type": "application/octet-stream", "uploader": owner, "uploaded": int64(21), "data": base64.StdEncoding.EncodeToString(blob)}},
		"git":    []any{}, "sqlGit": gitBackup,
		"state": map[string]any{"listHistory": []string{}, "hidden": []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "hosted": []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, "pending": []any{map[string]any{"id": e.ID, "until": pendingUntil}}, "prRefs": []any{}},
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(legacyFixtureWithChecksum(string(raw)))
	archive, err := replication.ReadCompatibleBackup(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replication.RestoreBackupWithProvider(ctx, tenant.store, tenant.ReplicationBackupProvider(), archive, 30); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM events WHERE id=?`, e.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("restored event = %d, %v", events, err)
	}
	var hidden int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM hidden_events WHERE id='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`).Scan(&hidden); err != nil || hidden != 1 {
		t.Fatalf("hidden state = %d, %v", hidden, err)
	}
	var pending int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM pending_events WHERE id=?`, e.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending state = %d, %v", pending, err)
	}
	var uploader string
	var uploaded int64
	if err := tenant.store.DB().QueryRow(`SELECT uploader,uploaded FROM blobs WHERE sha256=?`, blobSum).Scan(&uploader, &uploaded); err != nil || uploader != owner || uploaded != 21 {
		t.Fatalf("blob metadata = %q/%d, %v", uploader, uploaded, err)
	}
	gitDir := filepath.Join(tenant.meta.Paths.Git, owner, "legacy.git")
	if out, err := exec.Command("git", "--git-dir", gitDir, "rev-parse", "refs/heads/main").CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) != 40 {
		t.Fatalf("restored git ref = %q, %v", out, err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", gitDir, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone failed: %v: %s", err, out)
	}
	content, err := os.ReadFile(filepath.Join(clone, "README.md"))
	if err != nil || string(content) != "legacy git\n" {
		t.Fatalf("cloned content = %q, %v", content, err)
	}
}

func TestReplicationPendingAndPRDeadlinesExpireAtSchedulerSweep(t *testing.T) {
	ctx := context.Background()
	_, tenant := testTenant(t)
	e := event.Event{CreatedAt: 20, Kind: 30618, Tags: [][]string{}, Content: "pending"}
	if err := event.Sign(&e, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.store.Save(ctx, e, storage.SaveOptions{Now: 20}); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"pending": []any{map[string]any{"id": e.ID, "until": int64(10)}}, "prRefs": []any{map[string]any{"repo": "pr:" + tenant.Policy().Owner + ":demo", "ref": "refs/nostr/" + e.ID, "until": int64(10)}}})
	if err := restoreRetainedState(ctx, tenant.store, state); err != nil {
		t.Fatal(err)
	}
	if err := tenant.sweepReplicationState(ctx, 11); err != nil {
		t.Fatal(err)
	}
	var events, refs int
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM events WHERE id=?`, e.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := tenant.store.DB().QueryRow(`SELECT count(*) FROM replication_pr_refs`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if events != 0 || refs != 0 {
		t.Fatalf("expired state survived: events=%d refs=%d", events, refs)
	}
}

func makeLegacyGitBackup(t *testing.T, owner string) map[string]any {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{{"init", work}, {"-C", work, "config", "user.email", "test@example.com"}, {"-C", work, "config", "user.name", "Test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("legacy git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", work, "add", "README.md"}, {"-C", work, "commit", "-m", "legacy"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	refOut, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(refOut))
	objectsOut, err := exec.Command("git", "-C", work, "rev-list", "--objects", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	objects := []any{}
	for _, line := range strings.Split(strings.TrimSpace(string(objectsOut)), "\n") {
		oid := strings.Fields(line)[0]
		body := gitObjectBytes(t, work, oid)
		typOut, err := exec.Command("git", "-C", work, "cat-file", "-t", oid).Output()
		if err != nil {
			t.Fatal(err)
		}
		typ := strings.TrimSpace(string(typOut))
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		_, _ = zw.Write(body)
		_ = zw.Close()
		objects = append(objects, map[string]any{"oid": oid, "type": typ, "size": len(body), "chunks": []string{base64.StdEncoding.EncodeToString(compressed.Bytes())}})
	}
	return map[string]any{"format": "bind.ws/git-sqlite/1", "repositories": []any{map[string]any{"key": strings.Repeat("a", 64), "owner": owner, "identifier": "legacy", "alternative": false, "objects": objects, "refs": map[string]string{"refs/heads/main": commit}}}}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
}

func legacyFixtureWithChecksum(raw string) string {
	key := `"archiveSha256":"`
	at := strings.Index(raw, key)
	start := at + len(key)
	end := strings.IndexByte(raw[start:], '"') + start
	unsigned := raw[:start] + raw[end:]
	return raw[:start] + sha256Hex([]byte(unsigned)) + raw[end:]
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func gitObjectBytes(t *testing.T, work, oid string) []byte {
	t.Helper()
	cmd := exec.Command("git", "-C", work, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintln(stdin, oid)
	_ = stdin.Close()
	header := make([]byte, 0, 128)
	buf := make([]byte, 1)
	for {
		if _, err := stdout.Read(buf); err != nil {
			t.Fatal(err)
		}
		header = append(header, buf[0])
		if buf[0] == '\n' {
			break
		}
	}
	parts := strings.Fields(string(header))
	if len(parts) < 3 {
		t.Fatalf("bad git batch header %q", header)
	}
	n := 0
	if _, err := fmt.Sscanf(parts[2], "%d", &n); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(stdout, body); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestReplicationProviderRejectsEscapingBackupFiles(t *testing.T) {
	root := t.TempDir()
	if err := restoreFiles(root, []replication.BackupObject{{Name: "../outside", SHA256: "", Content: []byte("bad")}}); err == nil {
		t.Fatal("backup path escaped tenant root")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside")); err == nil {
		t.Fatal("outside file created")
	}
}

func TestTenantBackupRestoreRoundTripWithFilesAndFTS(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(ctx, filepath.Join(root, "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := event.Event{CreatedAt: 10, Kind: 1, Tags: [][]string{}, Content: "restorable phrase"}
	if err := event.Sign(&e, "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, e, storage.SaveOptions{Now: 11, SearchMode: "full"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "blobs", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "nested", "blob"), []byte("blob-original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "git", "object"), []byte("git-original"), 0600); err != nil {
		t.Fatal(err)
	}
	tenant := &Tenant{store: store, publicURL: "https://relay.example", meta: catalog.Tenant{Paths: catalog.Paths{Root: root, Git: filepath.Join(root, "git")}}}
	provider := tenantReplicationProvider{tenant: tenant}
	state, err := provider.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "nested", "blob"), []byte("mutated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "extra"), []byte("remove-me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "git", "object"), []byte("git-mutated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(ctx, state); err != nil {
		t.Fatal(err)
	}
	result, err := store.Query(ctx, event.Filter{Search: "restorable", Tags: map[string][]string{}}, storage.QueryOptions{Now: 11, Access: storage.Access{All: true}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("restored search results = %d", len(result.Events))
	}
	blob, _ := os.ReadFile(filepath.Join(root, "blobs", "nested", "blob"))
	if string(blob) != "blob-original" {
		t.Fatalf("blob = %q", blob)
	}
	git, _ := os.ReadFile(filepath.Join(root, "git", "object"))
	if string(git) != "git-original" {
		t.Fatalf("git = %q", git)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", "extra")); !os.IsNotExist(err) {
		t.Fatal("extra blob survived restore")
	}
	bad := replication.BackupArchive{Format: replication.BackupFormat, Events: []event.Event{e}, State: state}
	bad.State.Blobs = append(bad.State.Blobs, replication.BackupObject{Name: "broken", SHA256: "bad", Content: []byte("bad")})
	raw, _ := json.Marshal(bad)
	if _, err := replication.ReadBackup(bytes.NewReader(raw)); err == nil {
		t.Fatal("bad archive passed integrity check")
	}
	if _, err := store.DB().Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", "nested", "blob"), []byte("rollback-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(ctx, bad.State); err == nil {
		t.Fatal("corrupt file restore unexpectedly succeeded")
	}
	rolledBack, err := store.Query(ctx, event.Filter{IDs: []string{e.ID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: 11, Access: storage.Access{All: true}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rolledBack.Events) != 0 {
		t.Fatal("database rollback restored the rejected archive")
	}
	marker, _ := os.ReadFile(filepath.Join(root, "blobs", "nested", "blob"))
	if string(marker) != "rollback-marker" {
		t.Fatalf("filesystem rollback = %q", marker)
	}
}

func TestRestoreRetainedStateIsTransactionalAndPreservesAllFamilies(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := event.Event{CreatedAt: 10, Kind: 30023, Tags: [][]string{{"d", "post"}}, Content: "history"}
	if err := event.Sign(&e, "1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	rawEvent, _ := json.Marshal(e)
	state, _ := json.Marshal(map[string]any{
		"listHistory": []string{string(rawEvent)},
		"hidden":      []string{"hidden-id"},
		"hosted":      []string{"hosted-id"},
		"pending":     []map[string]any{{"id": "pending-id", "until": int64(42)}},
		"prRefs":      []map[string]any{{"repo": "repo", "ref": "refs/heads/main", "until": int64(43)}},
	})
	if err := restoreRetainedState(ctx, store, state); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM list_history`,
		`SELECT count(*) FROM hidden_events WHERE id='hidden-id'`,
		`SELECT count(*) FROM replication_hosted_events WHERE id='hosted-id'`,
		`SELECT count(*) FROM replication_pending_until WHERE id='pending-id' AND until=42`,
		`SELECT count(*) FROM replication_pr_refs WHERE repo='repo' AND ref='refs/heads/main' AND until=43`,
	} {
		var n int
		if err := store.DB().QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s count=%d", query, n)
		}
	}
	bad, _ := json.Marshal(map[string]any{"listHistory": []string{"not-json"}, "hidden": []string{"should-not-commit"}})
	if err := restoreRetainedState(ctx, store, bad); err == nil {
		t.Fatal("malformed retained state restored")
	}
	var n int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM hidden_events WHERE id='should-not-commit'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("retained state partially committed")
	}
}
