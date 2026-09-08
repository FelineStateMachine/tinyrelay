package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type siteBackend struct {
	fakeBackend
	filter map[string]any
}

func (b *siteBackend) Query(_ context.Context, method string, params []json.RawMessage, _ string) (any, error) {
	if method != "queryevents" {
		return nil, nil
	}
	_ = json.Unmarshal(params[0], &b.filter)
	return []any{map[string]any{"id": strings.Repeat("1", 64), "pubkey": "0629c45c1b2820eb649f44a0ab5dd39a16497208eebbe1210b641855c809dbd7", "kind": 15128, "created_at": float64(1757200000), "content": "", "tags": [][]string{{"path", "/index.html", strings.Repeat("c", 64)}}}}, nil
}

func TestShellRendersRailPanelAndPrompt(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for path, wants := range map[string][]string{
		"/":                                      {`id="mark"`, `href="/search"`, `<b>demo</b> &raquo; home`, `id="panel"`, `id="topbar"`, `rel="manifest"`},
		"/manage/rules":                          {`<b>manage</b>`, `href="/manage/rules" aria-current="page"`, `manage/rules</span>`},
		"/repo?owner=aa&repo=notes&view=history": {`/history</a>`, `repos/notes/history`},
	} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		if strings.Contains(body, "class=") {
			t.Errorf("%s carries a class attribute", path)
		}
	}
}

func TestRepositoryHomeRendersReadmeAsMarkdown(t *testing.T) {
	b := &browseBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/repo?owner=alice&repo=notes&view=home", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `id="readme"`) || !strings.Contains(body, "<p>one two</p>") {
		t.Fatalf("repository home did not render the README: %d %s", recorder.Code, body)
	}
}

func TestSitesPageListsHostedSites(t *testing.T) {
	backend := &siteBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sites", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `href="http://npub1`) || !strings.Contains(body, ".relay.example") || !strings.Contains(body, "1 paths") {
		t.Fatalf("sites page did not list the site: %d %s", recorder.Code, body)
	}
	if kinds, _ := backend.filter["kinds"].([]any); len(kinds) != 2 {
		t.Fatalf("sites query kinds = %v", backend.filter["kinds"])
	}
}

func TestFeedFiltersNarrowTheQuery(t *testing.T) {
	filter := map[string]any{"kinds": []int{1, 30023}, "limit": 12}
	applyFeedFilters(filter, url.Values{"kinds": {"7"}, "author": {strings.Repeat("b", 64)}, "since": {"2026-09-01"}})
	if kinds, _ := filter["kinds"].([]int); len(kinds) != 1 || kinds[0] != 7 {
		t.Fatalf("kinds = %v", filter["kinds"])
	}
	if authors, _ := filter["authors"].([]string); len(authors) != 1 {
		t.Fatalf("authors = %v", filter["authors"])
	}
	if since, _ := filter["since"].(int64); since != 1788220800 {
		t.Fatalf("since = %v", filter["since"])
	}
	applyFeedFilters(filter, url.Values{"kinds": {"all"}, "author": {"short"}})
	if _, ok := filter["kinds"]; ok {
		t.Fatal("kinds were not cleared by all")
	}
}

func TestManifestCarriesTenantPrefixAndRelayName(t *testing.T) {
	p := policy.Defaults(strings.Repeat("a", 64))
	p.Name = "my relay"
	backend := &fakeBackend{policy: p}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	request.RequestURI = "/r/alice/manifest.webmanifest"
	app.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("content-type"), "manifest+json") || !strings.Contains(body, `"start_url": "/r/alice/"`) || !strings.Contains(body, `"name": "my relay"`) || !strings.Contains(body, `"purpose": "monochrome"`) {
		t.Fatalf("manifest: %d %s", recorder.Code, body)
	}
	for _, path := range []string{"/sw.js", "/icon.svg", "/icon-mono.svg"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: %d", path, recorder.Code)
		}
	}
}

func TestPromptPathSpellsOutRepositoriesAndFiles(t *testing.T) {
	if got := promptPath("/", nil); got != "home" {
		t.Fatalf("home prompt = %q", got)
	}
	if got := promptPath("/repo", url.Values{"repo": {"notes"}, "view": {"file"}}); got != "repos/notes/file" {
		t.Fatalf("repo prompt = %q", got)
	}
	if got := promptPath("/manage/people", nil); got != "manage/people" {
		t.Fatalf("manage prompt = %q", got)
	}
}
