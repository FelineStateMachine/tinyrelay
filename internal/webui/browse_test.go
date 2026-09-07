package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/gitrelay"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type browseBackend struct {
	fakeBackend
	method string
	params []json.RawMessage
}

func (b *browseBackend) Query(_ context.Context, method string, params []json.RawMessage, _ string) (any, error) {
	b.method, b.params = method, params
	switch method {
	case "browserepos":
		return map[string]any{"items": []any{map[string]any{"owner": "alice", "identifier": "notes", "head": "abc"}}}, nil
	case "browserepo":
		return map[string]any{"view": "file", "content": "one\ntwo", "entries": []any{}}, nil
	case "browsefiles":
		return map[string]any{"items": []any{map[string]any{"sha256": "deadbeef", "type": "text/plain"}}}, nil
	case "browsefile":
		return map[string]any{"content": "blob", "binary": false}, nil
	case "browsestatus":
		return map[string]any{"jobs": []any{}, "storage": map[string]any{"ok": true}}, nil
	default:
		return nil, nil
	}
}

func TestBrowseRoutesUseObjectContractsAndRenderData(t *testing.T) {
	b := &browseBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path, method, marker string
	}{
		{"/repos", "browserepos", "notes"},
		{"/repo?owner=alice&repo=notes&view=file", "browserepo", "one"},
		{"/files", "browsefiles", "deadbeef"},
		{"/file?hash=deadbeef", "browsefile", "blob"},
		{"/manage/status", "browsestatus", "storage"},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), test.marker) {
			t.Fatalf("%s: status=%d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
		if b.method != test.method || len(b.params) != 1 {
			t.Fatalf("%s: method=%q params=%d", test.path, b.method, len(b.params))
		}
		var object map[string]any
		if err := json.Unmarshal(b.params[0], &object); err != nil {
			t.Fatalf("%s: params are not an object: %v", test.path, err)
		}
		if test.method == "browserepo" && object["view"] != "file" {
			t.Fatalf("repo view=%v", object["view"])
		}
	}
}

func TestConnectPublishesGRASPAndDeliveryKinds(t *testing.T) {
	b := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/connect", nil))
	body := recorder.Body.String()
	for _, marker := range []string{"10317", "10063", "tinySignedFetch", `name="relays"`, `map(x=>["g",x])`, `map(x=>["server",x])`, `tags:t,content:c`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("connect page missing %q", marker)
		}
	}
}

func TestBrowseRendersTypedGitRelayPage(t *testing.T) {
	b := &browseBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	// Keep the fixture typed exactly as daemon.executeBrowse returns it. This
	// catches template regressions where fields are assumed to be map values.
	page := gitrelay.BrowsePage{Owner: "alice", Identifier: "notes", View: "file", Content: "<script>\nline two", Clone: []string{"https://example.test/notes.git"}}
	browse := &typedBrowseBackend{browseBackend: b, page: page}
	app, err := New(browse, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/repo?owner=alice&repo=notes&view=file", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "line two") || !strings.Contains(body, "Raw/download") {
		t.Fatalf("typed browse page status=%d body=%s", recorder.Code, body)
	}
}

type typedBrowseBackend struct {
	*browseBackend
	page gitrelay.BrowsePage
}

func (b *typedBrowseBackend) Query(_ context.Context, method string, _ []json.RawMessage, _ string) (any, error) {
	if method == "browserepo" {
		return b.page, nil
	}
	return b.browseBackend.Query(context.Background(), method, nil, "")
}
