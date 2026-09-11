package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
)

func TestClientFileBrowsingPermissionsAndDownload(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("<script>secret source</script>"), Type: "text/html", Uploader: tenant.Policy().Owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Execute(ctx, "", "browsefiles", nil); err == nil {
		t.Fatal("anonymous uploader inventory exposed")
	}
	value, err := tenant.Execute(ctx, tenant.Policy().Owner, "browsefiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if !strings.Contains(string(raw), entry.SHA256) {
		t.Fatalf("missing blob %s", raw)
	}
	req := httptest.NewRequest(http.MethodGet, "http://relay.test/files/raw?hash="+entry.SHA256, nil)
	res := httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code != 200 || res.Body.String() != "<script>secret source</script>" {
		t.Fatalf("download %d %s", res.Code, res.Body.String())
	}
	if !strings.HasPrefix(res.Header().Get("Content-Disposition"), "attachment") || res.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatal("active download lacks isolation")
	}
	p := tenant.Policy()
	p.Reads = "members"
	if err := tenant.applyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	tenant.ServeHTTP(res, req)
	if res.Code == 200 {
		t.Fatal("private download exposed")
	}
}

func TestClientStatusRequiresOwnerAndPreservesErrors(t *testing.T) {
	_, tenant := testTenant(t)
	if _, err := tenant.Execute(context.Background(), "", "browsestatus", nil); err == nil {
		t.Fatal("anonymous operational status exposed")
	}
	value, err := tenant.Execute(context.Background(), tenant.Policy().Owner, "browsestatus", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	for _, field := range []string{"capabilities", "git_sync", "storage", "jobs", "delivery", "backups"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("missing %s: %s", field, raw)
		}
	}
}

func TestFileSearchPaginationPreservesEveryUploaderMatch(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	uploader := strings.Repeat("b", 64)
	var want []string
	for i, typ := range []string{"text/plain", "image/png", "text/plain", "text/plain", "text/plain"} {
		entry, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader(fmt.Sprintf("file %d", i)), Type: typ, Uploader: uploader})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tenant.store.DB().ExecContext(ctx, "UPDATE blobs SET uploaded=? WHERE sha256=?", 100-i, entry.SHA256); err != nil {
			t.Fatal(err)
		}
		if typ == "text/plain" {
			want = append(want, entry.SHA256)
		}
	}
	if _, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("another user's matching file"), Type: "text/plain", Uploader: tenant.Policy().Owner}); err != nil {
		t.Fatal(err)
	}
	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		params, err := json.Marshal(map[string]any{"q": "text/plain", "limit": 2, "cursor": cursor})
		if err != nil {
			t.Fatal(err)
		}
		result, err := tenant.Execute(ctx, uploader, "browsefiles", []json.RawMessage{params})
		if err != nil {
			t.Fatal(err)
		}
		row := result.(map[string]any)
		for _, item := range row["items"].([]map[string]any) {
			got = append(got, item["sha256"].(string))
		}
		cursor = row["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("matching files = %v, want %v", got, want)
	}
}

func TestOwnerFileSearchUsesLiteralTextBeforePagination(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	for i, typ := range []string{"text/plain", "application/x_foo", "image/png"} {
		if _, err := tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader(fmt.Sprintf("owner file %d", i)), Type: typ, Uploader: tenant.Policy().Owner}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tenant.Execute(ctx, tenant.Policy().Owner, "browsefiles", []json.RawMessage{json.RawMessage(`{"q":"_","limit":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	row := result.(map[string]any)
	items := row["items"].([]map[string]any)
	if len(items) != 1 || items[0]["type"] != "application/x_foo" || row["next_cursor"] != "" {
		t.Fatalf("literal search = %#v", row)
	}
}

func TestLibraryRowsGroupsLegacyUnnamedBinaryAndHidesMarkedChunks(t *testing.T) {
	rows := []map[string]any{
		{"sha256": strings.Repeat("a", 64), "name": "", "path": "", "type": "application/octet-stream"},
		{"sha256": strings.Repeat("b", 64), "name": "", "path": "", "type": "application/octet-stream", "purpose": "chunk"},
		{"sha256": strings.Repeat("c", 64), "name": "notes.txt", "path": "notes.txt", "type": "text/plain"},
	}
	got := libraryRows(rows, nil)
	if len(got) != 2 {
		t.Fatalf("library rows = %d, want 2", len(got))
	}
	if got[0]["path"] != "Unorganized uploads/aaaaaaaaaaaa" || got[0]["name"] != "aaaaaaaaaaaa" {
		t.Fatalf("legacy row = %#v", got[0])
	}
	if got[1]["path"] != "notes.txt" {
		t.Fatalf("named row changed = %#v", got[1])
	}
}
