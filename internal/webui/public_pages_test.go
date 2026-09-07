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

type publicBackend struct{ fakeBackend }

func (b *publicBackend) Query(_ context.Context, method string, _ []json.RawMessage, actor string) (any, error) {
	if method != "queryevents" {
		return b.fakeBackend.Query(context.Background(), method, nil, actor)
	}
	return []map[string]any{{"id": "event-1", "pubkey": actor, "content": "visible result"}}, nil
}

type articleBackend struct{ fakeBackend }

func (b *articleBackend) Query(_ context.Context, method string, _ []json.RawMessage, _ string) (any, error) {
	if method == "queryevents" {
		return []map[string]any{{"id": strings.Repeat("b", 64), "kind": 30023, "created_at": float64(1700000000), "content": "A title\nbody"}}, nil
	}
	return nil, nil
}

func TestPublicSearchAndInboxRenderBackendResults(t *testing.T) {
	owner := strings.Repeat("a", 64)
	backend := &publicBackend{fakeBackend: fakeBackend{policy: policy.Defaults(owner)}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/search?q=visible", "/inbox", "/outbox", "/articles"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, "visible result") || !strings.Contains(body, "event-1") {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, body)
		}
	}
}

func TestArticleFeedsContainStoredRecordsAndRespectPagesFeature(t *testing.T) {
	backend := &articleBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/articles.json", "/feed.xml"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), strings.Repeat("b", 64)) || !strings.Contains(recorder.Body.String(), "A title") {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	backend.policy.Features.Pages = false
	for _, path := range []string{"/articles.json", "/feed.xml", "/e/" + strings.Repeat("b", 64), "/a/title"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d", path, recorder.Code)
		}
	}
}
