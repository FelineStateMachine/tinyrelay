package tinyclient

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/podcasts"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

func TestPodcastRSSUsesShowMetadataAndNativeShowNotes(t *testing.T) {
	key := strings.Repeat("a", 64)
	episode := event.Event{ID: strings.Repeat("b", 64), PubKey: key, Kind: 54, CreatedAt: 1700000000, Content: "## Links\n\nA **good** conversation.", Tags: [][]string{{"title", "Episode & one"}, {"description", "An <introduction>"}, {"audio", "https://example.test/one.mp3", "audio/mpeg"}, {"audio", "https://example.test/one.ogg", "audio/ogg"}}}
	show := podcasts.Show{ID: "metadata", Pubkey: key, Title: "Trail & Radio", Description: "A show about walking", Image: "https://example.test/cover.png", Websites: []string{"https://example.test/podcast"}}
	b := &podcastRSSBackend{fakeBackend: fakeBackend{policy: policy.Defaults(key)}, result: map[string]any{"show": show, "items": []any{map[string]any{"id": episode.ID, "kind": 54, "pubkey": key, "event": episode, "podcast": show}}}}
	a := &App{backend: b}
	w := httptest.NewRecorder()
	a.podcastRSS(w, httptest.NewRequest(http.MethodGet, "/social/podcasts.rss?author="+key, nil))
	var feed struct {
		Channel struct {
			Title, Description, Link string
			Items                    []struct {
				GUID       string `xml:"guid"`
				Enclosures []struct {
					URL string `xml:"url,attr"`
				} `xml:"enclosure"`
				Content string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	// Lowercase RSS element names are required.
	var document podcastRSS
	if err := xml.Unmarshal(w.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Channel.Title != show.Title || document.Channel.Description != show.Description || !strings.Contains(w.Body.String(), "<link>"+show.Websites[0]+"</link>") {
		t.Fatalf("show metadata: %#v", document.Channel)
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Channel.Items) != 1 || len(feed.Channel.Items[0].Enclosures) != 1 || feed.Channel.Items[0].Enclosures[0].URL != "https://example.test/one.mp3" || feed.Channel.Items[0].GUID != episode.ID {
		t.Fatalf("RSS episode: %s", w.Body.String())
	}
	if !strings.Contains(feed.Channel.Items[0].Content, "<strong>good</strong>") || !strings.Contains(w.Body.String(), `itunes:image href="https://example.test/cover.png"`) {
		t.Fatalf("artwork/show notes: %s", w.Body.String())
	}
}

func TestPodcastRSSRejectsAudioNotesAndOmitsEmptyArtwork(t *testing.T) {
	for _, kind := range []int{1, 30023} {
		_, ok := podcastFeedItem("https://relay.test", map[string]any{"id": "ordinary", "kind": kind, "tags": [][]string{{"audio", "https://example.test/one.mp3"}, {"imeta", "url https://example.test/one.mp3", "m audio/mpeg"}}})
		if ok {
			t.Fatalf("kind %d became a podcast", kind)
		}
	}
	b := &podcastRSSBackend{fakeBackend: fakeBackend{policy: policy.Defaults(strings.Repeat("a", 64))}, result: map[string]any{"show": podcasts.Show{Pubkey: strings.Repeat("a", 64)}}}
	w := httptest.NewRecorder()
	(&App{backend: b}).podcastRSS(w, httptest.NewRequest(http.MethodGet, "/social/podcasts.rss", nil))
	if strings.Contains(w.Body.String(), "itunes:image") || strings.Contains(w.Body.String(), "<description></description>") {
		t.Fatalf("empty optional metadata: %s", w.Body.String())
	}
}
