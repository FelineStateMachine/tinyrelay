package webui

import (
	"context"
	"encoding/json"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWebMCPScriptUsesNativeModelContextAndExistingAuth(t *testing.T) {
	backend := &fakeBackend{}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/webmcp.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"document.modelContext", "navigator.modelContext", "tiny.list_repositories", "tiny.read_repository", "tiny.read_file", "tiny.read_status", "tiny.add_job", "tiny.set_policy", "structuredContent", "state.errors", "tiny.read_management", "tinySignedFetch"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("WebMCP script missing %q", marker)
		}
	}
}

func TestWebMCPQueryReturnsStructuredBackendResult(t *testing.T) {
	backend := &webMCPBackend{fakeBackend: fakeBackend{}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "actor", nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/webmcp/query?method=browserepos&params="+url.QueryEscape(`[{"limit":2}]`), nil)
	request.Header.Set("content-type", "application/json")
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "repo-visible") || backend.method != "browserepos" || backend.actor != "actor" {
		t.Fatalf("status=%d body=%s method=%s actor=%s", recorder.Code, recorder.Body.String(), backend.method, backend.actor)
	}
	if recorder.Header().Get("cache-control") != "no-store, private" {
		t.Fatalf("query response must not be cached: %q", recorder.Header().Get("cache-control"))
	}
}

func TestWebMCPQueryErrorsAreJSON(t *testing.T) {
	app, err := New(&fakeBackend{}, Options{Actor: func(*http.Request) (string, error) {
		return "", context.Canceled
	}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/webmcp/query?method=browserepos", nil)
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Header().Get("content-type"), "application/json") || !strings.Contains(recorder.Body.String(), `"error"`) {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("content-type"), recorder.Body.String())
	}
}

func TestWebMCPQueryRejectsMutationMethods(t *testing.T) {
	app, err := New(&fakeBackend{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/webmcp/query?method=setpolicy", nil)
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type webMCPBackend struct {
	fakeBackend
	method string
	actor  string
}

func (b *webMCPBackend) Query(_ context.Context, method string, _ []json.RawMessage, actor string) (any, error) {
	b.method, b.actor = method, actor
	return map[string]any{"items": []any{map[string]any{"identifier": "repo-visible"}}}, nil
}

func TestPagesAdvertiseWebMCPScript(t *testing.T) {
	app, err := New(&fakeBackend{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(recorder.Body.String(), `src="/webmcp.js"`) {
		t.Fatal("page does not load WebMCP registration")
	}
}

func TestHealthPageReportsVersionsAndBrowserTools(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }, Version: "1.4.0", Revision: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/health", nil))
	body := recorder.Body.String()
	for _, want := range []string{"<code>1.4.0</code>", "abc1234", "<code>" + scriptsVersion + "</code>", `id="webmcp-status"`, "WebMCP is unavailable", "signed forms"} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("health page missing %q: status=%d", want, recorder.Code)
		}
	}
	for _, old := range []string{"/tools", "/manage/status"} {
		moved := httptest.NewRecorder()
		app.ServeHTTP(moved, httptest.NewRequest(http.MethodGet, old, nil))
		if moved.Code != http.StatusMovedPermanently || moved.Header().Get("Location") != "/manage/health" {
			t.Fatalf("%s: status=%d location=%q", old, moved.Code, moved.Header().Get("Location"))
		}
	}
}
