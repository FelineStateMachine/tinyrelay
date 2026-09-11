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

type socialBackend struct {
	fakeBackend
	method string
	query  map[string]any
}

func (b *socialBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	if method != "browsesocial" && method != "browsesocialthread" {
		return b.fakeBackend.Query(ctx, method, params, actor)
	}
	b.method = method
	if len(params) > 0 {
		if err := json.Unmarshal(params[0], &b.query); err != nil {
			return nil, err
		}
	}
	post := map[string]any{"id": strings.Repeat("b", 64), "kind": 1, "pubkey": strings.Repeat("a", 64), "created_at": 1789000000, "content": "A small moment worth sharing.", "author_name": "River", "author_picture": "https://example.com/avatar.png", "reply_count": 2, "reactions": []any{map[string]any{"content": "+", "count": 3, "reacted": false}, map[string]any{"content": ":party:", "image": "https://example.com/party.png", "count": 1}}, "read_path": "/social/" + strings.Repeat("b", 64)}
	if method == "browsesocialthread" {
		return map[string]any{"post": post, "comments": []any{}, "next_cursor": ""}, nil
	}
	return map[string]any{"items": []any{post}, "next_cursor": "older-page", "stats": map[string]any{"notes": 1, "articles": 0, "comments": 2}}, nil
}

func TestSocialPageRendersFeedWithoutJavaScript(t *testing.T) {
	b := &socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	a, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/social?kind=notes&author="+strings.Repeat("a", 64)+"&cursor=older&q=moment", nil))
	for _, want := range []string{"Social", "A small moment worth sharing.", "River", "/social", "older-page", `data-reaction-emoji`, `alt=":party:"`} {
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), want) {
			t.Fatalf("missing %q: status=%d body=%s", want, r.Code, r.Body.String())
		}
	}
	if b.method != "browsesocial" || b.query["kind"] != "notes" || b.query["cursor"] != "older" || b.query["q"] != "moment" {
		t.Fatalf("query: %s %#v", b.method, b.query)
	}
}

func TestSocialThreadAndLegacyArticleRoute(t *testing.T) {
	b := &socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	a, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/social/"+strings.Repeat("b", 64), nil))
	if r.Code != http.StatusOK || b.method != "browsesocialthread" || b.query["id"] != strings.Repeat("b", 64) || !strings.Contains(r.Body.String(), "A small moment worth sharing.") {
		t.Fatalf("thread: %d %s", r.Code, r.Body.String())
	}
	r = httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/articles?author=alice", nil))
	if r.Code != http.StatusMovedPermanently || r.Header().Get("Location") != "/social?author=alice" {
		t.Fatalf("legacy: %d %s", r.Code, r.Header().Get("Location"))
	}
}

func TestNostrReferenceRoutesUseSocialThread(t *testing.T) {
	owner := strings.Repeat("a", 64)
	b := &socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}}
	a, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 64)
	r := httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/e/"+id, nil))
	if r.Code != http.StatusOK || b.method != "browsesocialthread" || b.query["id"] != id || !strings.Contains(r.Body.String(), "A small moment worth sharing.") {
		t.Fatalf("event reference: status=%d method=%s query=%#v", r.Code, b.method, b.query)
	}

	b.method, b.query = "", nil
	address := "30023:" + owner + ":hello"
	r = httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/a/"+address, nil))
	if r.Code != http.StatusOK || b.method != "browsesocialthread" || b.query["address"] != address {
		t.Fatalf("article reference: status=%d method=%s query=%#v", r.Code, b.method, b.query)
	}
}

func TestNostrReferenceRoutesKeepLegacyFallback(t *testing.T) {
	owner := strings.Repeat("a", 64)
	b := &fakeBackend{policy: policy.Defaults(owner)}
	a, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/a/legacy-name", nil))
	if r.Code != http.StatusOK || b.call != "queryevents:" {
		t.Fatalf("legacy article fallback: status=%d call=%q", r.Code, b.call)
	}
}

func TestSocialRespectsPagesFeature(t *testing.T) {
	b := &socialBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	b.policy.Features.Pages = false
	a, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/social", "/social/" + strings.Repeat("b", 64), "/social.json", "/articles"} {
		r := httptest.NewRecorder()
		a.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusNotFound {
			t.Fatalf("%s status %d", path, r.Code)
		}
	}
}

func TestSocialContextLinksToParentAndRoot(t *testing.T) {
	root, parent := strings.Repeat("a", 64), strings.Repeat("b", 64)
	data := PageData{Tab: "social-thread", Event: map[string]any{"post": map[string]any{"id": root}}}
	row := socialContext(map[string]any{"id": strings.Repeat("c", 64), "parent_id": parent, "root_id": root}, data)
	if row["parent_url"] != "/social/"+parent || row["root_url"] != "/social/"+root || row["nested"] != true {
		t.Fatalf("missing nested context: %#v", row)
	}
	address := "30023:" + root + ":essay:part-one"
	row = socialContext(map[string]any{"id": parent, "parent_id": root, "parent_address": address, "root_address": address}, data)
	if row["nested"] != false || !strings.Contains(plainString(row["parent_url"]), "address=30023%3A") {
		t.Fatalf("missing article parent: %#v", row)
	}
}
