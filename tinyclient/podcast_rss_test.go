package tinyclient

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

type podcastRSSBackend struct {
	fakeBackend
	result any
	err    error
	query  map[string]any
	actor  string
	base   string
}

func (b *podcastRSSBackend) URL() string {
	if b.base != "" {
		return b.base
	}
	return b.fakeBackend.URL()
}

func (b *podcastRSSBackend) Query(ctx context.Context, method string, params []json.RawMessage, actor string) (any, error) {
	b.actor = actor
	b.query = nil
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &b.query)
	}
	return b.result, b.err
}

func TestPodcastRSSPageBuildsAudioEnclosures(t *testing.T) {
	author := strings.Repeat("a", 64)
	b := &podcastRSSBackend{
		fakeBackend: fakeBackend{policy: policy.Defaults(author)},
		result: map[string]any{"items": []any{
			map[string]any{"id": strings.Repeat("b", 64), "kind": 1, "title": `A & <episode>`, "content": "A description https://cdn.example/audio.mp3?a=1&b=2", "created_at": 1700000000, "tags": [][]string{{"imeta", "url https://cdn.example/audio.mp3?a=1&b=2", "m audio/mpeg", "size 42"}, {"imeta", "url https://cdn.example/photo.jpg", "m image/jpeg"}}},
			map[string]any{"id": strings.Repeat("c", 64), "kind": 30023, "content": "no audio", "tags": [][]string{{"imeta", "url https://cdn.example/photo.jpg", "m image/jpeg"}}},
		}},
	}
	a := &App{backend: b}
	r := httptest.NewRequest(http.MethodGet, "/social/podcasts.rss?author="+author, nil)
	w := httptest.NewRecorder()
	a.podcastRSS(w, r)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("response: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var feed podcastRSS
	if err := xml.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatalf("invalid RSS: %v\n%s", err, w.Body.String())
	}
	if len(feed.Channel.Items) != 1 || feed.Channel.Items[0].Enclosure.Length != 42 || feed.Channel.Items[0].Enclosure.Type != "audio/mpeg" {
		t.Fatalf("items: %#v", feed.Channel.Items)
	}
	if !strings.Contains(w.Body.String(), "A &amp; &lt;episode&gt;") || !strings.Contains(w.Body.String(), "a=1&amp;b=2") {
		t.Fatalf("escaped feed: %s", w.Body.String())
	}
	if b.query["kind"] != "podcasts" || b.query["limit"] != float64(100) || b.query["author"] != author {
		t.Fatalf("query: %#v", b.query)
	}
}

func TestPodcastRSSPageUsesUnknownLengthAndPropagatesErrors(t *testing.T) {
	b := &podcastRSSBackend{
		fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))},
		result: map[string]any{
			"items": []any{map[string]any{
				"id": "episode", "content": "no body reference",
				"tags": [][]string{{"imeta", "url https://cdn.example/episode.ogg", "m audio/ogg"}},
			}},
		},
	}
	a := &App{backend: b}
	w := httptest.NewRecorder()
	a.podcastRSS(w, httptest.NewRequest(http.MethodGet, "/social/podcasts.rss", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `length="0"`) {
		t.Fatalf("unknown length: %d %s", w.Code, w.Body.String())
	}
	b.err = errors.New("backend unavailable")
	w = httptest.NewRecorder()
	a.podcastRSS(w, httptest.NewRequest(http.MethodGet, "/social/podcasts.rss", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("backend error status: %d", w.Code)
	}
	a.actor = nil
	a.actor = func(*http.Request) (string, error) { return "", errors.New("actor failure") }
	w = httptest.NewRecorder()
	a.podcastRSS(w, httptest.NewRequest(http.MethodGet, "/social/podcasts.rss", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("actor error status: %d", w.Code)
	}
}

func TestPodcastRSSRouteNormalizesSubscriptionAndSupportsHead(t *testing.T) {
	author := strings.Repeat("a", 64)
	b := &podcastRSSBackend{fakeBackend: fakeBackend{policy: policy.Defaults(author)}, base: "https://relay.example/r/team"}
	a, err := New(b, Options{Actor: func(*http.Request) (string, error) { return strings.Repeat("b", 64), nil }})
	if err != nil {
		t.Fatal(err)
	}
	path := "/social/podcasts.rss?author=" + identityNpub(author) + "&q=morning&cursor=old&kind=notes"
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "https://relay.example/r/team/social/podcasts.rss?author="+author+"&amp;q=morning") || !strings.Contains(w.Body.String(), "https://relay.example/r/team/social/profile?author="+author) {
		t.Fatalf("tenant subscription: %d %s", w.Code, w.Body.String())
	}
	if b.query["author"] != author || b.query["cursor"] != nil || b.query["kind"] != "podcasts" || b.actor != strings.Repeat("b", 64) {
		t.Fatalf("subscription query: %#v actor=%s", b.query, b.actor)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/social/podcasts.rss", nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" || w.Header().Get("Cache-Control") != "private, no-store" || b.query["author"] != nil {
		t.Fatalf("HEAD response: %d %v query=%v", w.Code, w.Header(), b.query)
	}
}

func TestPodcastArticleGUIDSurvivesAnEdit(t *testing.T) {
	address := "30023:" + strings.Repeat("a", 64) + ":episode"
	row := map[string]any{"id": "first", "address": address, "kind": 30023, "content": "Episode one", "tags": [][]string{{"imeta", "url https://cdn.example/episode", "m audio/mpeg", "size 100"}}}
	first, ok := podcastFeedItem("https://relay.example", row)
	if !ok {
		t.Fatal("episode omitted")
	}
	row["id"] = "second"
	second, ok := podcastFeedItem("https://relay.example", row)
	if !ok || first.GUID != second.GUID || second.GUID.Value != address || second.Title != "Episode one" {
		t.Fatalf("edited episode identity: %#v %#v", first, second)
	}
}
