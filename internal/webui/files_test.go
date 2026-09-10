package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type fileLibraryBackend struct{ fakeBackend }

func (b *fileLibraryBackend) Query(_ context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	if method != "browsefiles" {
		return nil, nil
	}
	b.call, b.params = method+":"+actor, params
	var query map[string]any
	if err := json.Unmarshal(params[0], &query); err != nil {
		return nil, err
	}
	return map[string]any{
		"view": query["view"], "path": query["path"], "can_manage": true, "next_cursor": "next-file",
		"breadcrumbs": []map[string]string{{"name": "My files", "open": "/files"}, {"name": "Trip", "open": "/files?path=Trip"}},
		"items": []map[string]any{
			{"kind": "folder", "name": "Photos", "path": "Trip/Photos", "open": "/files?path=Trip%2FPhotos"},
			{"kind": "file", "name": "Snow & sun.jpg", "sha256": strings.Repeat("f", 64), "type": "image/jpeg", "size": 16384, "uploaded": 1789066800},
		},
	}, nil
}

func TestFilesRenderNamedFoldersAndPreserveBrowseQuery(t *testing.T) {
	backend := &fileLibraryBackend{fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/files", "/files?view=sites&path=Trip&q=snow&cursor=old"} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, response.Code)
		}
		var query map[string]any
		if err := json.Unmarshal(backend.params[0], &query); err != nil {
			t.Fatal(err)
		}
		body := response.Body.String()
		for _, want := range []string{`id="file-library"`, `aria-label="File collections"`, `Snow &amp; sun.jpg`, `Photos`, `16.0 KiB`, `loading="lazy"`, `next-file`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", target, want)
			}
		}
		if target == "/files" {
			if query["view"] != "library" || !strings.Contains(body, `href="/files" aria-current="page"`) {
				t.Fatalf("default view was not library: %#v", query)
			}
		} else if query["view"] != "sites" || query["path"] != "Trip" || query["q"] != "snow" || query["cursor"] != "old" || !strings.Contains(body, `path=Trip`) || !strings.Contains(body, `q=snow`) {
			t.Fatalf("browse query was lost: %#v", query)
		}
	}
}
