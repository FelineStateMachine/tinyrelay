package records

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func TestFoldKeepsProfileAndRelayForSameAuthor(t *testing.T) {
	pk := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	events := []event.Event{
		{Kind: event.KIND_PROFILE, PubKey: pk, Content: `{"name":"Alice"}`},
		{Kind: 10002, PubKey: pk, Tags: [][]string{{"r", "wss://relay.example/"}}},
	}
	profiles, _ := fold("profiles", events, "", 100)
	relays, _ := fold("relays", events, "", 100)
	if len(profiles) != 1 || profiles[0][1] != pk {
		t.Fatalf("profiles = %#v", profiles)
	}
	if len(relays) != 1 || relays[0][1] != "wss://relay.example" {
		t.Fatalf("relays = %#v", relays)
	}
}

func TestFoldCalendarWindowAndArticlesUsePublishedAt(t *testing.T) {
	now := int64(1_700_000_000)
	old := event.Event{Kind: 31923, PubKey: "a", Tags: [][]string{{"d", "old"}, {"start", strconv.FormatInt(now-2*86400, 10)}}}
	future := event.Event{Kind: 31923, PubKey: "a", Tags: [][]string{{"d", "future"}, {"start", strconv.FormatInt(now+2*86400, 10)}, {"title", "Future"}}}
	cal, _ := fold("calendar", []event.Event{old, future}, "", now)
	if len(cal) != 1 || cal[0][1] != "31923:a:future" {
		t.Fatalf("calendar = %#v", cal)
	}
	late := event.Event{Kind: 30023, PubKey: "a", CreatedAt: 10, Tags: [][]string{{"d", "late"}, {"published_at", "200"}}}
	early := event.Event{Kind: 30023, PubKey: "a", CreatedAt: 20, Tags: [][]string{{"d", "early"}, {"published_at", "100"}}}
	articles, _ := fold("articles", []event.Event{late, early}, "", now)
	if len(articles) != 2 || articles[0][1] != "30023:a:late" {
		t.Fatalf("articles = %#v", articles)
	}
}

func TestFoldPodcastsSummarizesNativeEpisodesAndShows(t *testing.T) {
	show := "podcast"
	author := "author"
	events := []event.Event{
		{ID: "meta", PubKey: show, Kind: 10154, CreatedAt: 2, Tags: [][]string{{"title", "The Show"}, {"p", author, "host"}}},
		{PubKey: author, Kind: 10064, CreatedAt: 3, Tags: [][]string{{"p", show}}},
		{ID: "episode", PubKey: author, Kind: 54, CreatedAt: 4, Content: "An episode", Tags: [][]string{{"title", "Episode one"}, {"audio", "https://cdn.example/one.mp3", "audio/mpeg", "42"}}},
	}
	tags, content := foldPodcasts(events)
	if len(tags) != 2 || tags[0][0] != "p" || tags[0][1] != show || tags[1][0] != "e" || tags[1][1] != "episode" {
		t.Fatalf("podcast tags = %#v", tags)
	}
	if !strings.Contains(content, `"The Show"`) || !strings.Contains(content, `"Episode one"`) || strings.Contains(content, "An episode") {
		t.Fatalf("podcast content = %s", content)
	}
}

func TestFoldPodcastsBoundsShowsAndEpisodes(t *testing.T) {
	events := make([]event.Event, 0, 205)
	for i := 0; i < 105; i++ {
		key := fmt.Sprintf("show-%03d", i)
		events = append(events, event.Event{ID: key, PubKey: key, Kind: 10154, CreatedAt: int64(i), Tags: [][]string{{"title", key}}})
	}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("episode-%03d", i)
		events = append(events, event.Event{ID: id, PubKey: "author", Kind: 54, CreatedAt: int64(i), Tags: [][]string{{"title", id}, {"audio", "https://cdn.example/episode.mp3", "audio/mpeg"}}})
	}
	tags, content := foldPodcasts(events)
	if len(tags) != 200 || strings.Contains(content, "episode-000") || strings.Contains(content, "show-104") {
		t.Fatalf("bounded podcast view has %d tags and content %d bytes", len(tags), len(content))
	}
}

func TestPodcastViewAppliesEventVisibilityCallback(t *testing.T) {
	s, ctx := testStore(t)
	owner := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := policy.Defaults(owner)
	visible := map[string]bool{"keep": true}
	r, err := New(ctx, Config{Community: emptyCommunityReader{}, Store: s, Policy: func() policy.Policy { return p }, RelayURL: "wss://relay.example", EventVisible: func(_ context.Context, e event.Event, _ policy.Access) bool { return visible[e.ID] }})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		{ID: "keep", PubKey: owner, Kind: 54, CreatedAt: 2, Tags: [][]string{{"title", "Kept"}, {"audio", "https://cdn.example/keep.mp3", "audio/mpeg"}}},
		{ID: "drop", PubKey: owner, Kind: 54, CreatedAt: 3, Tags: [][]string{{"title", "Blocked"}, {"audio", "https://cdn.example/drop.mp3", "audio/mpeg"}}},
	} {
		if _, err := s.Save(ctx, e, storage.SaveOptions{Now: e.CreatedAt}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := r.View(ctx, "podcasts", policy.Access{Owner: true}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Event.Tags) != 4 || view.Event.Tags[3][1] != "keep" {
		t.Fatalf("filtered podcast view = %#v", view.Event.Tags)
	}
}
