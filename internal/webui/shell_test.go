package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

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
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for path, wants := range map[string][]string{
		"/":                                      {`id="mark"`, `href="/search"`, `<b>demo</b> &raquo; <page-link url="http://relay.example" title="Copy the page address">home</page-link>`, `id="panel"`, `id="topbar"`, `rel="manifest"`},
		"/manage/rules":                          {`<b>manage</b>`, `href="/manage/rules" aria-current="page"`, `manage/rules</page-link></span>`},
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

func TestShellNavigationIsProgressiveAndAccessible(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`data-js="no"`, `id="content" tabindex="-1"`,
		`id="navigation-status" role="status" aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing progressive navigation marker %q", want)
		}
	}
	assertSharedNativeMenu(t, body, "/")

	// The menu is part of the shell, so each viewer and rail context retains
	// one native control and one shared navigation container.
	for _, path := range []string{"/search", "/articles", "/files", "/repo?owner=alice&repo=notes&view=history", "/e/" + strings.Repeat("1", 64), "/manage/rules"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", path, recorder.Code)
		}
		assertSharedNativeMenu(t, recorder.Body.String(), path)
	}

	privatePolicy := policy.Defaults(strings.Repeat("b", 64))
	privatePolicy.Reads = "members"
	private := &privateFakeBackend{fakeBackend: &fakeBackend{policy: privatePolicy}, allowed: map[string]bool{}}
	privateApp, err := New(private, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	privateApp.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/manage/identity", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("private shell: status=%d", recorder.Code)
	}
	assertSharedNativeMenu(t, recorder.Body.String(), "private")
}

func assertSharedNativeMenu(t *testing.T, body, path string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s: parse shell: %v", path, err)
	}
	var details, summary, rail *html.Node
	detailsCount, summaryCount, railCount := 0, 0, 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "details":
				if attr(node, "id") != "nav-menu" {
					break
				}
				detailsCount++
				if details == nil {
					details = node
				}
			case "summary":
				if attr(node, "id") != "menu" {
					break
				}
				summaryCount++
				if summary == nil {
					summary = node
				}
			case "td":
				if attr(node, "id") == "rail" {
					railCount++
					rail = node
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if detailsCount != 1 || summaryCount != 1 || railCount != 1 {
		t.Fatalf("%s: expected one details, summary and rail; got %d, %d, %d", path, detailsCount, summaryCount, railCount)
	}
	if hasAttr(details, "open") {
		t.Errorf("%s: menu details must be nav-menu without open", path)
	}
	if summary.Parent != details || attr(summary, "id") != "menu" || attr(summary, "aria-controls") != "rail" {
		t.Errorf("%s: summary must be the menu details child controlling rail", path)
	}
	if attr(rail, "id") != "rail" {
		t.Errorf("%s: menu target must be the rail", path)
	}
}

func attr(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

func hasAttr(node *html.Node, name string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return true
		}
	}
	return false
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

func TestSitesPagePreservesRelayPortInHostedURL(t *testing.T) {
	backend := &portSiteBackend{siteBackend: &siteBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}}
	app, err := New(backend, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sites", nil))
	if !strings.Contains(recorder.Body.String(), ".relay.example:8787") {
		t.Fatalf("hosted URL lost relay port: %s", recorder.Body.String())
	}
}

type portSiteBackend struct{ *siteBackend }

func (*portSiteBackend) URL() string { return "http://relay.example:8787" }

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

type imageBackend struct{ fakeBackend }

func (b *imageBackend) Query(_ context.Context, method string, _ []json.RawMessage, _ string) (any, error) {
	if method == "browsefile" {
		return map[string]any{"sha256": strings.Repeat("9", 64), "type": "image/png", "size": 12, "binary": true}, nil
	}
	return nil, nil
}

func TestFilePagePreviewsImages(t *testing.T) {
	app, err := New(&imageBackend{fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/file?sha="+strings.Repeat("9", 64), nil))
	if body := recorder.Body.String(); !strings.Contains(body, `<img src="/files/raw?hash=`+strings.Repeat("9", 64)+`"`) {
		t.Fatalf("no image preview: %s", body)
	}
}

func TestGuestsSeeSignInInsteadOfManagementPages(t *testing.T) {
	backend := &fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/manage/people", "/manage/agents", "/manage/owner", "/manage/health", "/manage/data"} {
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, "Sign in to manage this relay.") || strings.Contains(body, `<rpc-form method="setmember"`) || strings.Contains(body, `id="rail"><div id="railbox"><header><span><a href="/">`) {
			t.Fatalf("%s exposed management content to a guest: %d %s", path, recorder.Code, body[:min(600, len(body))])
		}
	}
}

type connectionsBackend struct{ fakeBackend }

func (b *connectionsBackend) Query(_ context.Context, method string, _ []json.RawMessage, _ string) (any, error) {
	if method == "connections" {
		return []any{map[string]any{"template": "notes", "title": "Notes", "about": "Everything posted here.", "app": "Jumble", "where": "web", "visibility": "public", "links": []any{map[string]any{"label": "Open", "href": "https://jumble.social/?r={relay:url|enc}"}}}}, nil
	}
	return nil, nil
}

func TestAccountPageOffersRelayListsAndHomeShowsConnectCards(t *testing.T) {
	backend := &connectionsBackend{fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}}
	app, err := New(backend, Options{Actor: func(*http.Request) (string, error) { return backend.policy.Owner, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/signin", nil))
	body := recorder.Body.String()
	for _, want := range []string{`<relay-lists relay="ws://relay.example" server="http://relay.example" pubkey="` + backend.policy.Owner + `">`, `data-kind="10002" data-tag="r"`, `data-kind="10050"`, `data-kind="10007"`, `data-kind="10063" data-tag="server"`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page missing %q", want)
		}
	}
	if !strings.Contains(servedScript(t, app, "/scripts/components.js"), `customElements.define("relay-lists"`) {
		t.Error("components script missing relay-lists")
	}
	recorder = httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body = recorder.Body.String()
	if !strings.Contains(body, "<h2>Connect</h2>") || !strings.Contains(body, `href="https://jumble.social/?r=ws%3A%2F%2Frelay.example"`) || !strings.Contains(body, "<connect-card>") {
		t.Fatalf("home page did not show connect cards: %s", body[:min(400, len(body))])
	}
}
