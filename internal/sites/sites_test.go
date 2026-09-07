package sites

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestSiteLabelsAndManifestValidation(t *testing.T) {
	key := "0000000000000000000000000000000000000000000000000000000000000001"
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	paths := [][]string{{"path", "/index.html", hash}, {"path", "/404.html", hash}}
	root := event.Event{Kind: KindSite, PubKey: key, Tags: paths}
	if got := SiteLabel(root); got == "" {
		t.Fatal("root site label is empty")
	}
	if err := ValidateManifest(root); err != nil {
		t.Fatalf("root manifest rejected: %v", err)
	}
	named := root
	named.Kind = KindNamedSite
	named.Tags = append(append([][]string(nil), paths...), []string{"d", "demo-site"})
	if err := ValidateManifest(named); err != nil {
		t.Fatalf("named manifest rejected: %v", err)
	}
	bad := root
	bad.Tags = append(append([][]string(nil), paths...), []string{"path", "/../x.js", hash})
	if err := ValidateManifest(bad); err == nil {
		t.Fatal("path traversal manifest accepted")
	}
}

func TestHandlerServesVerifiedBlobAndFallback(t *testing.T) {
	const hash = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(Config{Store: store, BaseDomain: "example.test", Policy: func() policy.Policy { return policy.Defaults("owner") }, GetBlob: func(context.Context, string) (Blob, error) {
		return Blob{Body: io.NopCloser(strings.NewReader("hello")), Type: "text/plain"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	key := "0000000000000000000000000000000000000000000000000000000000000001"
	e := event.Event{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Kind: KindSite, PubKey: key, Tags: [][]string{{"path", "/index.html", hash}, {"path", "/blog/index.html", hash}, {"path", "/404.html", hash}}}
	if _, err := store.Save(context.Background(), e, service.SaveOptions(e, 1)); err != nil {
		t.Fatal(err)
	}
	host := SiteLabel(e) + ".example.test"
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Body.String() != "hello" {
		t.Fatalf("root = %d %q", response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "https://"+host+"/index.html", nil)
	req.Header.Set("Range", "bytes=1-3")
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusPartialContent || response.Body.String() != "ell" {
		t.Fatalf("range = %d %q", response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "https://"+host+"/blog?x=1", nil)
	req.Header.Set("Accept", "text/html")
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusPermanentRedirect {
		t.Fatalf("directory redirect = %d", response.Code)
	}
	location, err := response.Result().Location()
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != "/blog/" || location.RawQuery != "x=1" {
		t.Fatalf("directory location = %s", location.String())
	}
	req = httptest.NewRequest(http.MethodGet, "https://"+host+"/.well-known/nostr.json?path=%2F", nil)
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(response.Body.String(), `"authors"`) {
		t.Fatalf("discovery = %d %q", response.Code, response.Body.String())
	}
	req.Method = http.MethodHead
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("discovery HEAD = %d %q", response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "https://"+host+"/missing.js", nil)
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusNotFound || response.Body.String() != "hello" {
		t.Fatalf("fallback = %d %q", response.Code, response.Body.String())
	}
}

func TestMirrorUsesVerifiedRemoteAndBlobWriter(t *testing.T) {
	const hash = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var saved []byte
	service, err := New(Config{Store: store, Policy: func() policy.Policy { return policy.Defaults("owner") }, Fetch: func(context.Context, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("hello")), Header: make(http.Header)}, nil
	}, ResolveIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{{8, 8, 8, 8}}, nil }, PutBlob: func(_ context.Context, _ string, _ string, body io.Reader) error {
		var err error
		saved, err = io.ReadAll(body)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := event.Event{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Kind: KindSite, PubKey: "0000000000000000000000000000000000000000000000000000000000000001", Tags: [][]string{{"path", "/index.html", hash}, {"server", "https://origin.example"}}}
	if err := service.Mirror(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if string(saved) != "hello" {
		t.Fatalf("saved = %q", saved)
	}
}

func TestManifestIndexFollowsEventDeletion(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(Config{Store: store, Policy: func() policy.Policy { return policy.Defaults("owner") }})
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{ID: strings.Repeat("b", 64), Kind: KindSite, PubKey: strings.Repeat("0", 63) + "1", Tags: [][]string{{"path", "/index.html", strings.Repeat("a", 64)}}}
	if _, err := store.Save(context.Background(), e, service.SaveOptions(e, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(context.Background(), func(tx *sql.Tx) error { _, err := tx.Exec("DELETE FROM events WHERE id=?", e.ID); return err }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM site_manifests WHERE event_id=?", e.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("manifest index count = %d", count)
	}
}

func TestSnapshotAggregateAndParseLabel(t *testing.T) {
	key := "0000000000000000000000000000000000000000000000000000000000000001"
	paths := [][]string{{"path", "/index.html", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	aggregate := Aggregate(paths)
	if len(aggregate) != 64 {
		t.Fatalf("aggregate length = %d", len(aggregate))
	}
	snapshot := event.Event{Kind: KindSiteSnapshot, PubKey: key, ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Tags: append(paths, []string{"x", aggregate, "aggregate"}, []string{"a", "15128:" + key + ":"})}
	if err := ValidateManifest(snapshot); err != nil {
		t.Fatalf("snapshot rejected: %v", err)
	}
	parsed, ok := ParseSite(SiteLabel(snapshot))
	if !ok || parsed.Kind != KindSiteSnapshot || parsed.ID != snapshot.ID {
		t.Fatalf("snapshot label did not round trip: %#v", parsed)
	}
}
