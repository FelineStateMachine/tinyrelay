package daemon

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/podcasts"
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestSocialBrowseNativePodcastsAndMixedFeed(t *testing.T) {
	_, tenant := testTenant(t)
	owner := tenant.Policy().Owner
	secret := testOwnerSecret
	show := signedEvent(t, secret, podcasts.KindMetadata, 100, [][]string{{"title", "Relay Radio"}, {"image", "https://example.test/cover.jpg"}}, "")
	episode := signedEvent(t, secret, podcasts.KindEpisode, 101, [][]string{{"title", "First episode"}, {"audio", "https://example.test/first.mp3", "audio/mpeg"}}, "The show notes")
	for _, row := range []event.Event{show, episode} {
		if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt, SearchMode: tenant.Policy().Features.Search}); err != nil {
			t.Fatal(err)
		}
	}
	value, err := tenant.Execute(context.Background(), owner, "browsepodcasts", []json.RawMessage{rawJSON(map[string]any{"author": episode.PubKey})})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	items := result["items"].([]socialItem)
	if len(items) != 1 || items[0].Kind != podcasts.KindEpisode || items[0].Podcast == nil || items[0].Podcast.Title != "Relay Radio" {
		t.Fatalf("podcast browse = %#v", result)
	}
	mixed, err := tenant.Execute(context.Background(), owner, "browsesocial", nil)
	if err != nil {
		t.Fatal(err)
	}
	if items := mixed.(map[string]any)["items"].([]socialItem); len(items) != 1 || items[0].Kind != podcasts.KindEpisode {
		t.Fatalf("mixed feed = %#v", items)
	}
}

func TestSocialPodcastThreadRequiresNativeAudio(t *testing.T) {
	_, tenant := testTenant(t)
	row := signedEvent(t, testOwnerSecret, 54, time.Now().Unix(), [][]string{{"title", "Unrelated kind 54 event"}}, "No podcast audio")
	if _, err := tenant.store.Save(context.Background(), row, storage.SaveOptions{Now: row.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.socialPost(context.Background(), tenant.Policy().Owner, row.ID); err == nil {
		t.Fatal("kind 54 without native audio should not open as a podcast")
	}
}

func TestSocialPodcastSearchPaginatesPastOtherEvents(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	var wanted []string
	for i := 1; i <= 8; i++ {
		tags := [][]string{{"title", "Other"}}
		if i%2 == 0 {
			tags = append(tags, []string{"audio", "https://example.test/" + strconv.Itoa(i) + ".mp3"})
		}
		if i == 2 || i == 6 {
			tags = append(tags, []string{"description", "walking"})
		}
		row := signedEvent(t, testOwnerSecret, 54, int64(i), tags, "show notes")
		if i == 2 || i == 6 {
			wanted = append([]string{row.ID}, wanted...)
		}
		if _, err := tenant.store.Save(ctx, row, storage.SaveOptions{Now: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	params := map[string]any{"author": tenant.Policy().Owner, "q": "walking", "limit": 1}
	for i, want := range wanted {
		value, err := tenant.Execute(ctx, tenant.Policy().Owner, "browsepodcasts", []json.RawMessage{rawJSON(params)})
		if err != nil {
			t.Fatal(err)
		}
		page := value.(map[string]any)
		items := page["items"].([]socialItem)
		if len(items) != 1 || items[0].ID != want {
			t.Fatalf("page %d: %#v", i, page)
		}
		params["cursor"] = page["next_cursor"]
		if i == 0 && params["cursor"] == "" {
			t.Fatal("missing continuation after filtered events")
		}
	}
}

func TestPodcastPublicationViewAndRSSFollowNativeEvents(t *testing.T) {
	app, tenant := testTenant(t)
	ctx, now := context.Background(), time.Now().Unix()
	show := signedEvent(t, testOwnerSecret, 10154, now-5, [][]string{{"title", "First show name"}, {"description", "Our show"}}, "")
	episode := signedEvent(t, testOwnerSecret, 54, now-4, [][]string{{"title", "A native episode"}, {"audio", "https://example.test/episode.mp3", "audio/mpeg"}}, "## Show notes")
	for _, row := range []event.Event{show, episode} {
		if _, err := tenant.router.Publish(ctx, row, relay.Session{PubKeys: []string{row.PubKey}}); err != nil {
			t.Fatal(err)
		}
	}
	var intents int
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM work_intents WHERE kind='view-publish' AND target='podcasts' AND event_id IN (?,?)`, show.ID, episode.ID).Scan(&intents); err != nil || intents != 2 {
		t.Fatalf("publication view intents=%d: %v", intents, err)
	}
	readRSS := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://relay.test/r/main/social/podcasts.rss?author="+show.PubKey, nil))
		if w.Code != 200 {
			t.Fatalf("RSS status %d: %s", w.Code, w.Body.String())
		}
		var root struct{ XMLName xml.Name }
		if err := xml.Unmarshal(w.Body.Bytes(), &root); err != nil || root.XMLName.Local != "rss" {
			t.Fatalf("RSS: %v %s", err, w.Body.String())
		}
		return w.Body.String()
	}
	if rss := readRSS(); !strings.Contains(rss, "First show name") || !strings.Contains(rss, episode.ID) {
		t.Fatalf("published RSS: %s", rss)
	}
	replacement := signedEvent(t, testOwnerSecret, 10154, now-3, [][]string{{"title", "Updated show name"}, {"description", "Our show"}}, "")
	if _, err := tenant.router.Publish(ctx, replacement, relay.Session{PubKeys: []string{show.PubKey}}); err != nil {
		t.Fatal(err)
	}
	if rss := readRSS(); !strings.Contains(rss, "Updated show name") || strings.Contains(rss, "First show name") || !strings.Contains(rss, episode.ID) {
		t.Fatalf("updated RSS: %s", rss)
	}
	deletion := signedEvent(t, testOwnerSecret, 5, now-2, [][]string{{"e", episode.ID}}, "")
	if _, err := tenant.router.Publish(ctx, deletion, relay.Session{PubKeys: []string{show.PubKey}}); err != nil {
		t.Fatal(err)
	}
	if rss := readRSS(); strings.Contains(rss, "<item>") || !strings.Contains(rss, "Updated show name") {
		t.Fatalf("deleted episode RSS: %s", rss)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://relay.test/r/main/view/podcasts", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), episode.ID) || !strings.Contains(w.Body.String(), "Updated show name") {
		t.Fatalf("signed view after deletion: %d %s", w.Code, w.Body.String())
	}
}

func TestSocialEpisodeCommentsRequireNativeRoot(t *testing.T) {
	root := event.Event{ID: "episode", PubKey: "show", Kind: podcasts.KindEpisode}
	unrelated := event.Event{ID: "other", PubKey: "show", Kind: podcasts.KindEpisode}
	comment := event.Event{Kind: 1111, Tags: [][]string{{"E", root.ID}, {"K", "54"}}}
	if !socialCommentFor(comment, root) {
		t.Fatal("native episode comment not associated")
	}
	if socialCommentFor(comment, unrelated) {
		t.Fatal("native episode comment associated with unrelated episode")
	}
	comment.Tags = [][]string{{"E", root.ID}, {"K", "30023"}}
	if socialCommentFor(comment, root) {
		t.Fatal("non-native root kind accepted for episode")
	}
}
