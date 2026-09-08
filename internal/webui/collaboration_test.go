package webui

import (
	"context"
	"encoding/json"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCollaborationTypedBackendResults(t *testing.T) {
	type item struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	value := map[string]any{"items": []item{{ID: "issue", Labels: []string{"bug"}}}, "replies": []item{{ID: "reply"}}}
	items := collaborationItems(value)
	if len(items) != 1 || valueMap(items[0])["id"] != "issue" {
		t.Fatalf("typed items lost: %v", items)
	}
	if labels := collaborationLabels(items[0]); len(labels) != 1 || labels[0] != "bug" {
		t.Fatalf("labels lost: %v", labels)
	}
	if replies := collaborationReplies(value); len(replies) != 1 || valueMap(replies[0])["id"] != "reply" {
		t.Fatalf("typed replies lost: %v", replies)
	}
}

type collaborationBackend struct{ *fakeBackend }

func (b *collaborationBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.call = method
	b.params = params
	type item struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Kind    int    `json:"kind"`
		Author  string `json:"author"`
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	root := item{ID: strings.Repeat("b", 64), Title: "A visible issue", Kind: 1621, Author: b.policy.Owner, Content: "Issue body", Status: "closed"}
	if method == "browseissues" {
		return map[string]any{"items": []item{root}}, nil
	}
	if method == "browseissue" {
		return map[string]any{"item": root, "replies": []any{}, "can_status": true, "repository": map[string]any{"owner": b.policy.Owner, "clone": []string{"https://git.example/test.git"}, "private": true}}, nil
	}
	return map[string]any{}, nil
}

func TestCollaborationPagesUseTypedResultsAndWorkingDetailLinks(t *testing.T) {
	backend := &collaborationBackend{&fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"owner": {backend.policy.Owner}, "repo": {"test"}}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, repoURL(query, "issues", ""), nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "A visible issue") {
		t.Fatalf("list failed: %d %s", w.Code, w.Body.String())
	}
	link := repoURL(query, "issue", strings.Repeat("b", 64))
	if !strings.Contains(w.Body.String(), html.EscapeString(link)) {
		t.Fatalf("detail link missing: %s", link)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, link, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Issue body") || !strings.Contains(w.Body.String(), `kind="status"`) || !strings.Contains(w.Body.String(), "https://git.example/test.git") || !strings.Contains(w.Body.String(), "<td>private</td>") {
		t.Fatalf("detail failed: %d %s", w.Code, w.Body.String())
	}
	var request map[string]any
	if err := json.Unmarshal(backend.params[0], &request); err != nil {
		t.Fatal(err)
	}
	if backend.call != "browseissue" || request["event"] != strings.Repeat("b", 64) {
		t.Fatalf("wrong detail request: %s %v", backend.call, request)
	}
}
