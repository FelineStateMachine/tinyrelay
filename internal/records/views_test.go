package records

import (
	"strconv"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
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
