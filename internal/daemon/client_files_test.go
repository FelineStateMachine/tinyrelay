package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

func fileItems(t *testing.T, value any) []map[string]any {
	t.Helper()
	row, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("files result = %T", value)
	}
	items, ok := row["items"].([]map[string]any)
	if !ok {
		t.Fatalf("files items = %T", row["items"])
	}
	return items
}

func TestFolderItemsKeepsFoldersAndFilesPageable(t *testing.T) {
	rows := []map[string]any{
		{"sha256": "aaaaaaaa", "type": "text/plain", "uploader": "alice", "uploaded": int64(2), "name": "readme.txt", "path": "docs/readme.txt"},
		{"sha256": "bbbbbbbb", "type": "image/png", "uploader": "alice", "uploaded": int64(1), "name": "cover.png", "path": "cover.png"},
	}
	first := (&Tenant{}).folderItems(rows, map[string]bool{}, clientBrowseRequest{BrowseRequest: browseRequestForTest(1)}, "library", true)
	items := first["items"].([]map[string]any)
	if len(items) != 1 || items[0]["kind"] != "folder" || first["next_cursor"] == "" {
		t.Fatalf("first page = %#v", first)
	}
	second := (&Tenant{}).folderItems(rows, map[string]bool{}, clientBrowseRequest{BrowseRequest: browseRequestForTest(1), Cursor: first["next_cursor"].(string)}, "library", true)
	items = second["items"].([]map[string]any)
	if len(items) != 1 || items[0]["kind"] != "file" || items[0]["name"] != "cover.png" {
		t.Fatalf("second page = %#v", second)
	}
}

func browseRequestForTest(limit int) gitrelay.BrowseRequest {
	return gitrelay.BrowseRequest{Limit: limit}
}

func TestContextualRowsRetainsDistinctPathsForSharedHash(t *testing.T) {
	rows := []map[string]any{{"sha256": "same", "name": "old", "path": "old"}}
	out := contextualRows(rows, map[string][]string{"same": {"site-a/index.html", "site-b/index.html"}}, "site")
	if len(out) != 2 || out[0]["path"] == out[1]["path"] {
		t.Fatalf("contextual rows = %#v", out)
	}
}

func TestContextualFilesRespectClaimNamesAndPaginateLibrary(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	other := strings.Repeat("b", 64)
	shared, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("shared"), Type: "text/plain", Uploader: owner, Name: "owner.txt", Path: "docs/owner.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("shared"), Type: "text/plain", Uploader: other, Name: "private.txt", Path: "private/private.txt"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if _, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader(fmt.Sprintf("library-%d", i)), Type: "text/plain", Uploader: owner, Name: fmt.Sprintf("file-%02d.txt", i), Path: fmt.Sprintf("folder-%02d/file-%02d.txt", i, i)}); err != nil {
			t.Fatal(err)
		}
	}
	firstValue, err := tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "library", "limit": 50})})
	if err != nil {
		t.Fatal(err)
	}
	first := fileItems(t, firstValue)
	firstRow := firstValue.(map[string]any)
	if len(first) != 50 || firstRow["next_cursor"] == "" {
		t.Fatalf("library first page = %d, next=%v", len(first), firstRow["next_cursor"])
	}
	secondValue, err := tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "library", "limit": 50, "cursor": firstRow["next_cursor"]})})
	if err != nil {
		t.Fatal(err)
	}
	second := fileItems(t, secondValue)
	if len(second) != 11 {
		t.Fatalf("library second page = %d, want 11", len(second))
	}
	seen := map[string]bool{}
	for _, item := range append(first, second...) {
		key := item["path"].(string)
		if seen[key] {
			t.Fatalf("duplicate library path %q", key)
		}
		seen[key] = true
	}
	ownerFile, err := tenant.Execute(ctx, owner, "browsefile", []json.RawMessage{rawJSON(map[string]any{"hash": shared.SHA256})})
	if err != nil {
		t.Fatal(err)
	}
	if ownerFile.(map[string]any)["name"] != "owner.txt" || ownerFile.(map[string]any)["path"] != "docs/owner.txt" {
		t.Fatalf("owner file metadata = %#v", ownerFile)
	}
	otherFile, err := tenant.Execute(ctx, other, "browsefile", []json.RawMessage{rawJSON(map[string]any{"hash": shared.SHA256})})
	if err != nil {
		t.Fatal(err)
	}
	if otherFile.(map[string]any)["name"] == "owner.txt" || otherFile.(map[string]any)["path"] == "docs/owner.txt" {
		t.Fatalf("other uploader saw owner's name: %#v", otherFile)
	}
}

func TestStorageViewDeduplicatesSharedBlobClaims(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	other := strings.Repeat("c", 64)
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("same bytes"), Type: "text/plain", Uploader: owner, Name: "owner.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("same bytes"), Type: "text/plain", Uploader: other, Name: "other.txt"}); err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "storage", "limit": 50})})
	if err != nil {
		t.Fatal(err)
	}
	items := fileItems(t, value)
	count := 0
	for _, item := range items {
		if item["sha256"] == entry.SHA256 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared blob appears %d times in storage view: %#v", count, items)
	}
}

func TestSiteFilesRetainSharedHashPaths(t *testing.T) {
	app, tenant := testTenant(t)
	defer app.Close(context.Background())
	ctx := context.Background()
	ownerSecret := strings.Repeat("0", 63) + "1"
	owner := tenant.Policy().Owner
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("site bytes"), Type: "text/html", Uploader: owner})
	if err != nil {
		t.Fatal(err)
	}
	manifest := event.Event{Kind: sites.KindSite, CreatedAt: time.Now().Unix(), Tags: [][]string{{"path", "/index.html", entry.SHA256}, {"path", "/mirror.html", entry.SHA256}}}
	if err := event.Sign(&manifest, ownerSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Publish(ctx, manifest, relay.Session{PubKeys: []string{owner}}); err != nil {
		t.Fatal(err)
	}
	value, err := tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "sites", "limit": 50})})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, folder := range fileItems(t, value) {
		if folder["kind"] != "folder" && folder["kind"] != "site" {
			continue
		}
		child, err := tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "sites", "path": folder["path"], "limit": 50})})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range fileItems(t, child) {
			if item["sha256"] == entry.SHA256 {
				paths[item["path"].(string)] = true
			}
		}
	}
	if len(paths) != 2 {
		t.Fatalf("site paths for shared hash = %#v", paths)
	}
}

func TestRoomFilesRespectPrivateAccessAndSameNameRooms(t *testing.T) {
	h := newRoomHarness(t)
	ctx := h.ctx
	owner := h.keys["owner"]
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "private-one"}, {"name", "Twin"}, {"closed"}}, "")
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "private-two"}, {"name", "Twin"}, {"closed"}}, "")
	for i, roomID := range []string{"private-one", "private-two"} {
		room, err := h.tenant.community.Room(ctx, roomID)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := h.tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader(fmt.Sprintf("private room %d", i)), Type: "text/plain", Uploader: h.keys["alice"]})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.tenant.store.DB().ExecContext(ctx, "INSERT INTO room_attachments(room_id,room_event_id,sha256,filename,created_at) VALUES(?,?,?,?,unixepoch())", roomID, room.EventID, entry.SHA256, fmt.Sprintf("note-%d.txt", i)); err != nil {
			t.Fatal(err)
		}
	}
	ownerValue, err := h.tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "rooms", "limit": 50})})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, folder := range fileItems(t, ownerValue) {
		if folder["kind"] != "folder" && folder["kind"] != "room" {
			continue
		}
		child, err := h.tenant.Execute(ctx, owner, "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "rooms", "path": folder["path"], "limit": 50})})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range fileItems(t, child) {
			if item["kind"] == "file" {
				paths[item["path"].(string)] = true
			}
		}
	}
	if len(paths) != 2 {
		t.Fatalf("same-name room paths = %#v", paths)
	}
	bobValue, err := h.tenant.Execute(ctx, h.keys["bob"], "browsefiles", []json.RawMessage{rawJSON(map[string]any{"view": "rooms", "limit": 50})})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fileItems(t, bobValue)); got != 0 {
		t.Fatalf("private room files visible to outsider: %d", got)
	}
}
